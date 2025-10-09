//go:build !norueidis
// +build !norueidis

/*
 * JuiceFS, Copyright 2024 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package meta

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/juicedata/juicefs/pkg/meta/pb"
	"github.com/pkg/errors"
	"github.com/redis/rueidis/rueidiscompat"
	"google.golang.org/protobuf/proto"
)

// Rueidis-specific backup operations that use m.compat instead of m.rdb.
// Most of the backup/restore logic is inherited from redisMeta, but methods
// that directly use m.rdb need Rueidis-specific implementations.

// load is the dispatcher that routes load operations to the correct handler.
// This override is necessary because embedded struct methods don't get virtual dispatch.
func (m *rueidisMeta) load(ctx Context, typ int, opt *LoadOption, val proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.load(ctx, typ, opt, val)
	}

	switch typ {
	case segTypeFormat:
		return m.loadFormat(ctx, val)
	case segTypeCounter:
		return m.loadCounters(ctx, val)
	case segTypeNode:
		return m.loadNodes(ctx, val)
	case segTypeChunk:
		return m.loadChunks(ctx, val)
	case segTypeEdge:
		return m.loadEdges(ctx, val)
	case segTypeSymlink:
		return m.loadSymlinks(ctx, val)
	case segTypeSustained:
		return m.loadSustained(ctx, val)
	case segTypeDelFile:
		return m.loadDelFiles(ctx, val)
	case segTypeSliceRef:
		return m.loadSliceRefs(ctx, val)
	case segTypeAcl:
		return m.loadAcl(ctx, val)
	case segTypeXattr:
		return m.loadXattrs(ctx, val)
	case segTypeQuota:
		return m.loadQuota(ctx, val)
	case segTypeStat:
		return m.loadDirStats(ctx, val)
	case segTypeParent:
		return m.loadParents(ctx, val)
	default:
		logger.Warnf("skip segment type %d", typ)
		return nil
	}
}

// dump dispatcher - ensures Rueidis-specific dump methods are called
func (m *rueidisMeta) dump(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	if m.compat == nil {
		return m.redisMeta.dump(ctx, opt, ch)
	}

	var dumps = []func(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error{
		m.dumpFormat,
		m.dumpCounters,  // Rueidis override
		m.dumpMix,       // inherits from redisMeta (but calls our overrides for nodes)
		m.dumpSustained, // Rueidis override
		m.dumpDelFiles,  // Rueidis override
		m.dumpSliceRef,
		m.dumpACL,     // Rueidis override
		m.dumpQuota,   // Rueidis override
		m.dumpDirStat, // Rueidis override
	}
	for _, f := range dumps {
		err := f(ctx, opt, ch)
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *rueidisMeta) dumpCounters(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	counters := make([]*pb.Counter, 0, len(counterNames))
	for _, name := range counterNames {
		cnt, err := m.getCounter(name)
		if err != nil {
			return errors.Wrapf(err, "get counter %s", name)
		}
		// Rueidis stores value-1 for nextInode/nextChunk, so add 1 when dumping
		if name == "nextInode" || name == "nextChunk" {
			cnt++
		}
		counters = append(counters, &pb.Counter{Key: name, Value: cnt})
	}
	return dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Counters: counters}})
}

func (m *rueidisMeta) dumpNodes(ctx Context, ch chan<- *dumpedResult, keys []string, pools []*sync.Pool, rel func(p proto.Message), sum *atomic.Uint64) error {
	if m.compat == nil {
		return m.redisMeta.dumpNodes(ctx, ch, keys, pools, rel, sum)
	}

	vals, err := m.compat.MGet(ctx, keys...).Result()
	if err != nil {
		return err
	}
	nodes := make([]*pb.Node, 0, len(vals))
	var inode uint64
	for idx, v := range vals {
		if v == nil {
			continue
		}
		inode, _ = strconv.ParseUint(keys[idx][len(m.prefix)+1:], 10, 64)
		node := pools[0].Get().(*pb.Node)
		node.Inode = inode
		node.Data = []byte(v.(string))
		nodes = append(nodes, node)
	}
	sum.Add(uint64(len(nodes)))
	return dumpResult(ctx, ch, &dumpedResult{&pb.Batch{Nodes: nodes}, rel})
}

func (m *rueidisMeta) dumpSustained(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	if m.compat == nil {
		return m.redisMeta.dumpSustained(ctx, opt, ch)
	}

	keys, err := m.compat.ZRange(ctx, m.allSessions(), 0, -1).Result()
	if err != nil {
		return err
	}

	sustained := make([]*pb.Sustained, 0, len(keys))
	for _, k := range keys {
		sid, _ := strconv.ParseUint(k, 10, 64)
		var ss []string
		ss, err = m.compat.SMembers(ctx, m.sustained(sid)).Result()
		if err != nil {
			return err
		}
		if len(ss) > 0 {
			inodes := make([]uint64, 0, len(ss))
			for _, s := range ss {
				inode, _ := strconv.ParseUint(s, 10, 64)
				inodes = append(inodes, inode)
			}
			sustained = append(sustained, &pb.Sustained{Sid: sid, Inodes: inodes})
		}
	}

	return dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Sustained: sustained}})
}

func (m *rueidisMeta) dumpDelFiles(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	if m.compat == nil {
		return m.redisMeta.dumpDelFiles(ctx, opt, ch)
	}

	zs, err := m.compat.ZRangeWithScores(ctx, m.delfiles(), 0, -1).Result()
	if err != nil {
		return err
	}

	delFiles := make([]*pb.DelFile, 0, min(len(zs), redisBatchSize))
	for i, z := range zs {
		parts := strings.Split(z.Member, ":")
		if len(parts) != 2 {
			logger.Warnf("invalid delfile string: %s", z.Member)
			continue
		}
		inode, _ := strconv.ParseUint(parts[0], 10, 64)
		length, _ := strconv.ParseUint(parts[1], 10, 64)
		delFiles = append(delFiles, &pb.DelFile{Inode: inode, Length: length, Expire: int64(z.Score)})

		if len(delFiles) >= redisBatchSize || i == len(zs)-1 {
			if err := dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Delfiles: delFiles}}); err != nil {
				return err
			}
			delFiles = make([]*pb.DelFile, 0, min(len(zs)-i-1, redisBatchSize))
		}
	}
	return nil
}

func (m *rueidisMeta) dumpACL(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	if m.compat == nil {
		return m.redisMeta.dumpACL(ctx, opt, ch)
	}

	vals, err := m.compat.HGetAll(ctx, m.aclKey()).Result()
	if err != nil {
		return err
	}

	acls := make([]*pb.Acl, 0, len(vals))
	for k, v := range vals {
		id, _ := strconv.ParseUint(k, 10, 32)
		acls = append(acls, &pb.Acl{Id: uint32(id), Data: []byte(v)})
	}
	return dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Acls: acls}})
}

func (m *rueidisMeta) dumpQuota(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	if m.compat == nil {
		return m.redisMeta.dumpQuota(ctx, opt, ch)
	}

	vals, err := m.compat.HGetAll(ctx, m.dirQuotaKey()).Result()
	if err != nil {
		return err
	}
	quotas := make(map[Ino]*pb.Quota)
	for k, v := range vals {
		inode, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			logger.Warnf("parse quota inode: %s: %v", k, err)
			continue
		}
		maxSpace, maxInodes := m.parseQuota([]byte(v))
		quotas[Ino(inode)] = &pb.Quota{Inode: inode, MaxSpace: maxSpace, MaxInodes: maxInodes}
	}

	vals, err = m.compat.HGetAll(ctx, m.dirQuotaUsedInodesKey()).Result()
	if err != nil {
		return fmt.Errorf("get dirQuotaUsedInodesKey err: %w", err)
	}
	for k, v := range vals {
		inode, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			logger.Warnf("parse quota inode: %s: %v", k, err)
			continue
		}
		inodes, _ := strconv.ParseInt(v, 10, 64)
		if q, ok := quotas[Ino(inode)]; !ok {
			logger.Warnf("quota for used inodes not found: %d", inode)
		} else {
			q.UsedInodes = inodes
		}
	}

	vals, err = m.compat.HGetAll(ctx, m.dirQuotaUsedSpaceKey()).Result()
	if err != nil {
		return fmt.Errorf("get dirQuotaUsedSpaceKey err: %w", err)
	}
	for k, v := range vals {
		inode, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			logger.Warnf("parse quota inode: %s: %v", k, err)
			continue
		}
		space, _ := strconv.ParseInt(v, 10, 64)
		if q, ok := quotas[Ino(inode)]; !ok {
			logger.Warnf("quota for used space not found: %d", inode)
		} else {
			q.UsedSpace = space
		}
	}

	qs := make([]*pb.Quota, 0, min(len(quotas), redisBatchSize))
	cnt := 0
	for _, q := range quotas {
		cnt++
		qs = append(qs, q)
		if len(qs) >= redisBatchSize {
			if err := dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Quotas: qs}}); err != nil {
				return err
			}
			qs = make([]*pb.Quota, 0, min(len(quotas)-cnt, redisBatchSize))
		}
	}
	return dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Quotas: qs}})
}

func (m *rueidisMeta) dumpDirStat(ctx Context, opt *DumpOption, ch chan<- *dumpedResult) error {
	if m.compat == nil {
		return m.redisMeta.dumpDirStat(ctx, opt, ch)
	}

	vals, err := m.compat.HGetAll(ctx, m.dirDataLengthKey()).Result()
	if err != nil {
		return err
	}
	stats := make(map[Ino]*pb.Stat)
	for k, v := range vals {
		inode, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			logger.Warnf("parse stat inode: %s: %v", k, err)
			continue
		}
		length, _ := strconv.ParseInt(v, 10, 64)
		stats[Ino(inode)] = &pb.Stat{Inode: inode, DataLength: length}
	}

	vals, err = m.compat.HGetAll(ctx, m.dirUsedInodesKey()).Result()
	if err != nil {
		return fmt.Errorf("get dirUsedInodesKey err: %w", err)
	}
	for k, v := range vals {
		inode, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			logger.Warnf("parse inodes stat inode: %s: %v", k, err)
			continue
		}
		inodes, _ := strconv.ParseInt(v, 10, 64)
		if q, ok := stats[Ino(inode)]; !ok {
			logger.Warnf("stat for used inodes not found: %d", inode)
		} else {
			q.UsedInodes = inodes
		}
	}

	vals, err = m.compat.HGetAll(ctx, m.dirUsedSpaceKey()).Result()
	if err != nil {
		return fmt.Errorf("get dirUsedSpaceKey err: %w", err)
	}
	for k, v := range vals {
		inode, err := strconv.ParseUint(k, 10, 64)
		if err != nil {
			logger.Warnf("parse space stat inode: %s: %v", k, err)
			continue
		}
		space, _ := strconv.ParseInt(v, 10, 64)
		if q, ok := stats[Ino(inode)]; !ok {
			logger.Warnf("stat for used space not found: %d", inode)
		} else {
			q.UsedSpace = space
		}
	}

	ss := make([]*pb.Stat, 0, min(len(stats), redisBatchSize))
	cnt := 0
	for _, s := range stats {
		cnt++
		ss = append(ss, s)
		if len(ss) >= redisBatchSize {
			if err := dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Dirstats: ss}}); err != nil {
				return err
			}
			ss = make([]*pb.Stat, 0, min(len(stats)-cnt, redisBatchSize))
		}
	}
	return dumpResult(ctx, ch, &dumpedResult{msg: &pb.Batch{Dirstats: ss}})
}

// Load operations - these need to use m.compat instead of m.rdb.Pipeline()

func (m *rueidisMeta) loadFormat(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadFormat(ctx, msg)
	}
	return m.compat.Set(ctx, m.setting(), msg.(*pb.Format).Data, 0).Err()
}

func (m *rueidisMeta) loadCounters(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadCounters(ctx, msg)
	}

	cs := make([]interface{}, 0, len(msg.(*pb.Batch).Counters)*2)
	for _, c := range msg.(*pb.Batch).Counters {
		// Like Redis, store nextInode/nextChunk as value-1
		// because incrCounter returns IncrBy result + 1
		var value int64
		if c.Key == "nextInode" || c.Key == "nextChunk" {
			value = c.Value - 1
		} else {
			value = c.Value
		}
		cs = append(cs, m.counterKey(c.Key), value)
	}
	return m.compat.MSet(ctx, cs...).Err()
}

func (m *rueidisMeta) loadNodes(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadNodes(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	for _, n := range msg.(*pb.Batch).Nodes {
		pipe.Set(ctx, m.inodeKey(Ino(n.Inode)), n.Data, 0)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadEdges(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadEdges(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	dm := make(map[uint64]map[string]interface{}) // {parent: {name: entry}}
	for _, e := range msg.(*pb.Batch).Edges {
		if _, ok := dm[e.Parent]; !ok {
			dm[e.Parent] = make(map[string]interface{})
		}
		dm[e.Parent][string(e.Name)] = m.packEntry(uint8(e.Type), Ino(e.Inode))
	}

	for parent, entries := range dm {
		pipe.HSet(ctx, m.entryKey(Ino(parent)), entries)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadChunks(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadChunks(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	for _, c := range msg.(*pb.Batch).Chunks {
		pipe.Set(ctx, m.chunkKey(Ino(c.Inode), uint32(c.Index)), c.Slices, 0) // nolint:staticcheck
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadSymlinks(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadSymlinks(ctx, msg)
	}

	syms := make([]interface{}, 0, len(msg.(*pb.Batch).Symlinks)*2)
	for _, s := range msg.(*pb.Batch).Symlinks {
		syms = append(syms, m.symKey(Ino(s.Inode)), s.Target)
		if len(syms) >= redisPipeLimit*2 {
			if err := m.compat.MSet(ctx, syms...).Err(); err != nil {
				return err
			}
			syms = syms[:0]
		}
	}
	if len(syms) > 0 {
		return m.compat.MSet(ctx, syms...).Err()
	}
	return nil
}

func (m *rueidisMeta) loadSustained(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadSustained(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	for _, s := range msg.(*pb.Batch).Sustained {
		members := make([]interface{}, len(s.Inodes))
		for i, inode := range s.Inodes {
			members[i] = inode
		}
		pipe.SAdd(ctx, m.sustained(s.Sid), members...)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadDelFiles(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadDelFiles(ctx, msg)
	}

	mbs := make([]rueidiscompat.Z, 0, len(msg.(*pb.Batch).Delfiles))
	for _, d := range msg.(*pb.Batch).Delfiles {
		mbs = append(mbs, rueidiscompat.Z{
			Score:  float64(d.Expire),
			Member: fmt.Sprintf("%d:%d", d.Inode, d.Length),
		})
	}
	return m.compat.ZAdd(ctx, m.delfiles(), mbs...).Err()
}

func (m *rueidisMeta) loadSliceRefs(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadSliceRefs(ctx, msg)
	}

	slices := make(map[string]interface{})
	for _, s := range msg.(*pb.Batch).SliceRefs {
		k := m.sliceKey(s.Id, s.Size)
		slices[k] = s.Refs
	}
	return m.compat.HSet(ctx, m.sliceRefs(), slices).Err()
}

func (m *rueidisMeta) loadAcl(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadAcl(ctx, msg)
	}

	acls := make(map[string]interface{})
	var maxAclId uint32
	for _, a := range msg.(*pb.Batch).Acls {
		acls[strconv.FormatUint(uint64(a.Id), 10)] = a.Data
		if a.Id > maxAclId {
			maxAclId = a.Id
		}
	}
	if err := m.compat.HSet(ctx, m.aclKey(), acls).Err(); err != nil {
		return err
	}
	return m.compat.Set(ctx, m.counterKey(aclCounter), maxAclId, 0).Err()
}

func (m *rueidisMeta) loadXattrs(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadXattrs(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	xm := make(map[uint64]map[string]interface{}) // {inode: {name: value}}
	for _, px := range msg.(*pb.Batch).Xattrs {
		if _, ok := xm[px.Inode]; !ok {
			xm[px.Inode] = make(map[string]interface{})
		}
		xm[px.Inode][px.Name] = px.Value
	}

	for inode, xattrs := range xm {
		pipe.HSet(ctx, m.xattrKey(Ino(inode)), xattrs)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadQuota(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadQuota(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	var inodeKey string
	for _, pq := range msg.(*pb.Batch).Quotas {
		inodeKey = Ino(pq.Inode).String()
		pipe.HSet(ctx, m.dirQuotaKey(), inodeKey, m.packQuota(pq.MaxSpace, pq.MaxInodes))
		pipe.HSet(ctx, m.dirQuotaUsedInodesKey(), inodeKey, pq.UsedInodes)
		pipe.HSet(ctx, m.dirQuotaUsedSpaceKey(), inodeKey, pq.UsedSpace)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadDirStats(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadDirStats(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	var inodeKey string
	for _, ps := range msg.(*pb.Batch).Dirstats {
		inodeKey = Ino(ps.Inode).String()
		pipe.HSet(ctx, m.dirDataLengthKey(), inodeKey, ps.DataLength)
		pipe.HSet(ctx, m.dirUsedInodesKey(), inodeKey, ps.UsedInodes)
		pipe.HSet(ctx, m.dirUsedSpaceKey(), inodeKey, ps.UsedSpace)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) loadParents(ctx Context, msg proto.Message) error {
	if m.compat == nil {
		return m.redisMeta.loadParents(ctx, msg)
	}

	pipe := m.compat.Pipeline()
	for _, p := range msg.(*pb.Batch).Parents {
		pipe.HIncrBy(ctx, m.parentKey(Ino(p.Inode)), Ino(p.Parent).String(), p.Cnt)
		if pipe.Len() >= redisPipeLimit {
			if err := execPipeCompat(ctx, pipe); err != nil {
				return err
			}
		}
	}
	return execPipeCompat(ctx, pipe)
}

func (m *rueidisMeta) prepareLoad(ctx Context, opt *LoadOption) error {
	if m.compat == nil {
		return m.redisMeta.prepareLoad(ctx, opt)
	}

	opt.check()
	// For Rueidis, we'll check if database is empty
	dbsize, err := m.compat.DBSize(ctx).Result()
	if err != nil {
		return err
	}
	if dbsize > 0 {
		return fmt.Errorf("database rueidis://%s is not empty (size: %d)", m.addr, dbsize)
	}
	return nil
}

// Helper function to execute a Rueidis pipeline via compat adapter
func execPipeCompat(ctx context.Context, pipe rueidiscompat.Pipeliner) error {
	if pipe.Len() == 0 {
		return nil
	}
	_, err := pipe.Exec(ctx)
	if err != nil {
		return errors.Wrap(err, "execute pipeline")
	}
	return nil
}

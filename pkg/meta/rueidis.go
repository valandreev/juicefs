//go:build !norueidis
// +build !norueidis

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/utils"
	pkgerrors "github.com/pkg/errors"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
	"golang.org/x/sync/errgroup"
)

type rueidisMeta struct {
	*redisMeta

	scheme    string
	canonical string
	option    rueidis.ClientOption
	client    rueidis.Client
	compat    rueidiscompat.Cmdable
}

// Temporary Rueidis registration that delegates to the Redis implementation.
// This keeps the Rueidis schemes usable during the driver bring-up and lets
// the registration test enforce that the schemes stay wired.
func init() {
	Register("rueidis", newRueidisMeta)
	Register("ruediss", newRueidisMeta)
}

func newRueidisMeta(driver, addr string, conf *Config) (Meta, error) {
	canonical := mapRueidisScheme(driver)
	uri := canonical + "://" + addr

	opt, err := rueidis.ParseURL(uri)
	if err != nil {
		return nil, fmt.Errorf("rueidis parse %s: %w", uri, err)
	}

	delegate, err := newRedisMeta(canonical, addr, conf)
	if err != nil {
		return nil, err
	}

	base, ok := delegate.(*redisMeta)
	if !ok {
		return nil, fmt.Errorf("unexpected meta implementation %T", delegate)
	}

	client, err := rueidis.NewClient(opt)
	if err != nil {
		return nil, fmt.Errorf("rueidis connect %s: %w", uri, err)
	}

	m := &rueidisMeta{
		redisMeta: base,
		scheme:    driver,
		canonical: canonical,
		option:    opt,
		client:    client,
		compat:    rueidiscompat.NewAdapter(client),
	}
	m.redisMeta.en = m
	return m, nil
}

func mapRueidisScheme(driver string) string {
	switch strings.ToLower(driver) {
	case "rueidis":
		return "redis"
	case "ruediss":
		return "rediss"
	default:
		return driver
	}
}

func (m *rueidisMeta) Name() string {
	return "rueidis"
}

func (m *rueidisMeta) Shutdown() error {
	if m.client != nil {
		m.client.Close()
	}
	return m.redisMeta.Shutdown()
}

func (m *rueidisMeta) doDeleteSlice(id uint64, size uint32) error {
	if m.compat == nil {
		return m.redisMeta.doDeleteSlice(id, size)
	}
	return m.compat.HDel(Background(), m.sliceRefs(), m.sliceKey(id, size)).Err()
}

func (m *rueidisMeta) doLoad() ([]byte, error) {
	if m.compat == nil {
		return m.redisMeta.doLoad()
	}
	cmd := m.compat.Get(Background(), m.setting())
	if err := cmd.Err(); err != nil {
		if err == rueidiscompat.Nil {
			return nil, nil
		}
		return nil, err
	}
	return cmd.Bytes()
}

func (m *rueidisMeta) getCounter(name string) (int64, error) {
	if m.compat == nil {
		return m.redisMeta.getCounter(name)
	}
	cmd := m.compat.Get(Background(), m.counterKey(name))
	v, err := cmd.Int64()
	if err == rueidiscompat.Nil {
		err = nil
	}
	return v, err
}

func (m *rueidisMeta) incrCounter(name string, value int64) (int64, error) {
	if m.compat == nil {
		return m.redisMeta.incrCounter(name, value)
	}
	if m.conf.ReadOnly {
		return 0, syscall.EROFS
	}
	key := m.counterKey(name)
	cmd := m.compat.IncrBy(Background(), key, value)
	v, err := cmd.Result()
	if err != nil {
		return v, err
	}
	if name == "nextInode" || name == "nextChunk" {
		return v + 1, nil
	}
	return v, nil
}

func (m *rueidisMeta) doInit(format *Format, force bool) error {
	if m.compat == nil {
		return m.redisMeta.doInit(format, force)
	}

	ctx := Background()
	body, err := m.compat.Get(ctx, m.setting()).Bytes()
	if err != nil && err != rueidiscompat.Nil {
		return err
	}
	if err == nil {
		var old Format
		if err = json.Unmarshal(body, &old); err != nil {
			return fmt.Errorf("existing format is broken: %w", err)
		}
		if !old.DirStats && format.DirStats {
			if err := m.compat.Del(ctx, m.dirUsedInodesKey(), m.dirUsedSpaceKey()).Err(); err != nil {
				return pkgerrors.Wrap(err, "remove dir stats")
			}
		}
		if !old.UserGroupQuota && format.UserGroupQuota {
			if err := m.compat.Del(ctx,
				m.userQuotaKey(), m.userQuotaUsedSpaceKey(), m.userQuotaUsedInodesKey(),
				m.groupQuotaKey(), m.groupQuotaUsedSpaceKey(), m.groupQuotaUsedInodesKey()).Err(); err != nil {
				return pkgerrors.Wrap(err, "remove user group quota")
			}
		}
		if err = format.update(&old, force); err != nil {
			return pkgerrors.Wrap(err, "update format")
		}
	}

	data, err := json.MarshalIndent(format, "", "")
	if err != nil {
		return fmt.Errorf("json: %w", err)
	}
	ts := time.Now().Unix()
	attr := &Attr{
		Typ:    TypeDirectory,
		Atime:  ts,
		Mtime:  ts,
		Ctime:  ts,
		Nlink:  2,
		Length: 4 << 10,
		Parent: 1,
	}
	if format.TrashDays > 0 {
		attr.Mode = 0555
		if err = m.compat.SetNX(ctx, m.inodeKey(TrashInode), m.marshal(attr), 0).Err(); err != nil {
			return err
		}
	}
	if err = m.compat.Set(ctx, m.setting(), data, 0).Err(); err != nil {
		return err
	}
	m.fmt = format
	if body != nil {
		return nil
	}

	attr.Mode = 0777
	return m.compat.Set(ctx, m.inodeKey(1), m.marshal(attr), 0).Err()
}

func (m *rueidisMeta) cacheACLs(ctx Context) error {
	if !m.getFormat().EnableACL {
		return nil
	}
	if m.compat == nil {
		return m.redisMeta.cacheACLs(ctx)
	}
	vals, err := m.compat.HGetAll(ctx, m.aclKey()).Result()
	if err != nil {
		return err
	}
	for k, v := range vals {
		id, _ := strconv.ParseUint(k, 10, 32)
		tmpRule := &aclAPI.Rule{}
		tmpRule.Decode([]byte(v))
		m.aclCache.Put(uint32(id), tmpRule)
	}
	return nil
}

func (m *rueidisMeta) getSession(sid string, detail bool) (*Session, error) {
	if m.compat == nil {
		return m.redisMeta.getSession(sid, detail)
	}
	ctx := Background()
	info, err := m.compat.HGet(ctx, m.sessionInfos(), sid).Bytes()
	if err == rueidiscompat.Nil {
		info = []byte("{}")
	} else if err != nil {
		return nil, fmt.Errorf("HGet sessionInfos %s: %v", sid, err)
	}
	var s Session
	if err = json.Unmarshal(info, &s); err != nil {
		return nil, fmt.Errorf("corrupted session info; json error: %w", err)
	}
	s.Sid, _ = strconv.ParseUint(sid, 10, 64)
	if detail {
		inodes, err := m.compat.SMembers(ctx, m.sustained(s.Sid)).Result()
		if err != nil {
			return nil, fmt.Errorf("SMembers %s: %v", sid, err)
		}
		s.Sustained = make([]Ino, 0, len(inodes))
		for _, sinode := range inodes {
			inode, _ := strconv.ParseUint(sinode, 10, 64)
			s.Sustained = append(s.Sustained, Ino(inode))
		}

		locks, err := m.compat.SMembers(ctx, m.lockedKey(s.Sid)).Result()
		if err != nil {
			return nil, fmt.Errorf("SMembers %s: %v", sid, err)
		}
		s.Flocks = make([]Flock, 0, len(locks))
		s.Plocks = make([]Plock, 0, len(locks))
		for _, lock := range locks {
			owners, err := m.compat.HGetAll(ctx, lock).Result()
			if err != nil {
				return nil, fmt.Errorf("HGetAll %s: %v", lock, err)
			}
			isFlock := strings.HasPrefix(lock, m.prefix+"lockf")
			inode, _ := strconv.ParseUint(lock[len(m.prefix)+5:], 10, 64)
			for k, v := range owners {
				parts := strings.Split(k, "_")
				if parts[0] != sid {
					continue
				}
				owner, _ := strconv.ParseUint(parts[1], 16, 64)
				if isFlock {
					s.Flocks = append(s.Flocks, Flock{Ino(inode), owner, v})
				} else {
					s.Plocks = append(s.Plocks, Plock{Ino(inode), owner, loadLocks([]byte(v))})
				}
			}
		}
	}
	return &s, nil
}

func (m *rueidisMeta) GetSession(sid uint64, detail bool) (*Session, error) {
	if m.compat == nil {
		return m.redisMeta.GetSession(sid, detail)
	}
	var legacy bool
	key := strconv.FormatUint(sid, 10)
	score, err := m.compat.ZScore(Background(), m.allSessions(), key).Result()
	if err == rueidiscompat.Nil {
		legacy = true
		score, err = m.compat.ZScore(Background(), legacySessions, key).Result()
	}
	if err == rueidiscompat.Nil {
		err = fmt.Errorf("session not found: %d", sid)
	}
	if err != nil {
		return nil, err
	}
	s, err := m.getSession(key, detail)
	if err != nil {
		return nil, err
	}
	s.Expire = time.Unix(int64(score), 0)
	if legacy {
		s.Expire = s.Expire.Add(5 * time.Minute)
	}
	return s, nil
}

func (m *rueidisMeta) ListSessions() ([]*Session, error) {
	if m.compat == nil {
		return m.redisMeta.ListSessions()
	}
	keys, err := m.compat.ZRangeWithScores(Background(), m.allSessions(), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	sessions := make([]*Session, 0, len(keys))
	for _, k := range keys {
		sid := k.Member
		s, err := m.getSession(sid, false)
		if err != nil {
			logger.Errorf("get session: %v", err)
			continue
		}
		s.Expire = time.Unix(int64(k.Score), 0)
		sessions = append(sessions, s)
	}

	legacyKeys, err := m.compat.ZRangeWithScores(Background(), legacySessions, 0, -1).Result()
	if err != nil {
		logger.Errorf("Scan legacy sessions: %v", err)
		return sessions, nil
	}
	for _, k := range legacyKeys {
		sid := k.Member
		s, err := m.getSession(sid, false)
		if err != nil {
			logger.Errorf("Get legacy session: %v", err)
			continue
		}
		s.Expire = time.Unix(int64(k.Score), 0).Add(5 * time.Minute)
		sessions = append(sessions, s)
	}
	return sessions, nil
}

func (m *rueidisMeta) doNewSession(sinfo []byte, update bool) error {
	if m.compat == nil {
		return m.redisMeta.doNewSession(sinfo, update)
	}
	ctx := Background()
	member := strconv.FormatUint(m.sid, 10)
	if err := m.compat.ZAdd(ctx, m.allSessions(), rueidiscompat.Z{
		Score:  float64(m.expireTime()),
		Member: member,
	}).Err(); err != nil {
		return fmt.Errorf("set session ID %d: %v", m.sid, err)
	}
	if err := m.compat.HSet(ctx, m.sessionInfos(), m.sid, sinfo).Err(); err != nil {
		return fmt.Errorf("set session info: %v", err)
	}
	if sha, err := m.compat.ScriptLoad(ctx, scriptLookup).Result(); err != nil {
		logger.Warnf("load scriptLookup: %v", err)
		m.shaLookup = ""
	} else {
		m.shaLookup = sha
	}
	if sha, err := m.compat.ScriptLoad(ctx, scriptResolve).Result(); err != nil {
		logger.Warnf("load scriptResolve: %v", err)
		m.shaResolve = ""
	} else {
		m.shaResolve = sha
	}
	if !m.conf.NoBGJob {
		go m.cleanupLegacies()
	}
	return nil
}

func (m *rueidisMeta) cleanupLegacies() {
	if m.compat == nil {
		m.redisMeta.cleanupLegacies()
		return
	}
	for {
		utils.SleepWithJitter(time.Minute)
		rng := rueidiscompat.ZRangeBy{
			Min:   "-inf",
			Max:   strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10),
			Count: 1000,
		}
		vals, err := m.compat.ZRangeByScore(Background(), m.delfiles(), rng).Result()
		if err != nil {
			continue
		}
		var count int
		for _, v := range vals {
			ps := strings.Split(v, ":")
			if len(ps) != 2 {
				inode, _ := strconv.ParseUint(ps[0], 10, 64)
				var length uint64 = 1 << 30
				if len(ps) > 2 {
					length, _ = strconv.ParseUint(ps[2], 10, 64)
				}
				logger.Infof("cleanup legacy delfile inode %d with %d bytes (%s)", inode, length, v)
				m.doDeleteFileData_(Ino(inode), length, v)
				count++
			}
		}
		if count == 0 {
			return
		}
	}
}

func (m *rueidisMeta) doFindDeletedFiles(ts int64, limit int) (map[Ino]uint64, error) {
	if m.compat == nil {
		return m.redisMeta.doFindDeletedFiles(ts, limit)
	}

	rng := rueidiscompat.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(ts, 10),
		Count: int64(limit),
	}
	vals, err := m.compat.ZRangeByScore(Background(), m.delfiles(), rng).Result()
	if err != nil {
		return nil, err
	}

	files := make(map[Ino]uint64, len(vals))
	for _, v := range vals {
		ps := strings.Split(v, ":")
		if len(ps) != 2 { // will be cleaned up as legacy
			continue
		}
		inode, _ := strconv.ParseUint(ps[0], 10, 64)
		length, _ := strconv.ParseUint(ps[1], 10, 64)
		files[Ino(inode)] = length
	}
	return files, nil
}

func (m *rueidisMeta) doCleanupSlices(ctx Context) {
	if m.compat == nil {
		m.redisMeta.doCleanupSlices(ctx)
		return
	}

	var (
		cursor uint64
		key    = m.sliceRefs()
	)

	for {
		kvs, next, err := m.compat.HScan(ctx, key, cursor, "*", 10000).Result()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return
			}
			logger.Warnf("HSCAN %s: %v", key, err)
			return
		}
		for i := 0; i < len(kvs); i += 2 {
			if ctx.Canceled() {
				return
			}
			field, val := kvs[i], kvs[i+1]
			if strings.HasPrefix(val, "-") {
				parts := strings.Split(field, "_")
				if len(parts) == 2 {
					id, _ := strconv.ParseUint(parts[0][1:], 10, 64)
					size, _ := strconv.ParseUint(parts[1], 10, 32)
					if id > 0 && size > 0 {
						m.deleteSlice(id, uint32(size))
					}
				}
			} else if val == "0" {
				m.cleanupZeroRef(field)
			}
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

func (m *rueidisMeta) cleanupZeroRef(key string) {
	if m.compat == nil {
		m.redisMeta.cleanupZeroRef(key)
		return
	}

	ctx := Background()
	if err := m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
		cmd := tx.HGet(ctx, m.sliceRefs(), key)
		val, err := cmd.Int64()
		if err != nil && err != rueidiscompat.Nil {
			return err
		}
		if err == rueidiscompat.Nil {
			val = 0
		}
		if val != 0 {
			return syscall.EINVAL
		}
		_, err = tx.TxPipelined(ctx, func(p rueidiscompat.Pipeliner) error {
			p.HDel(ctx, m.sliceRefs(), key)
			return nil
		})
		return err
	}, m.sliceRefs()); err != nil && !errors.Is(err, syscall.EINVAL) {
		logger.Warnf("cleanupZeroRef %s: %v", key, err)
	}
}

func (m *rueidisMeta) cleanupLeakedChunks(delete bool) {
	if m.compat == nil {
		m.redisMeta.cleanupLeakedChunks(delete)
		return
	}

	ctx := Background()
	prefix := len(m.prefix)
	pattern := m.prefix + "c*"
	var cursor uint64

	for {
		keys, next, err := m.compat.Scan(ctx, cursor, pattern, 10000).Result()
		if err != nil {
			logger.Warnf("scan %s: %v", pattern, err)
			return
		}
		if len(keys) > 0 {
			var (
				chunkKeys []string
				rs        []*rueidiscompat.IntCmd
			)
			pipe := m.compat.Pipeline()
			for _, k := range keys {
				parts := strings.Split(k, "_")
				if len(parts) != 2 {
					continue
				}
				if len(parts[0]) <= prefix {
					continue
				}
				ino, _ := strconv.ParseUint(parts[0][prefix+1:], 10, 64)
				chunkKeys = append(chunkKeys, k)
				rs = append(rs, pipe.Exists(ctx, m.inodeKey(Ino(ino))))
			}
			if len(rs) > 0 {
				cmds, err := pipe.Exec(ctx)
				if err != nil {
					for _, c := range cmds {
						if execErr := c.Err(); execErr != nil {
							logger.Errorf("check inode existence pipeline: %v", execErr)
						}
					}
					continue
				}
				for i, rr := range rs {
					if rr.Val() == 0 {
						key := chunkKeys[i]
						logger.Infof("found leaked chunk %s", key)
						if delete {
							parts := strings.Split(key, "_")
							if len(parts) != 2 || len(parts[0]) <= prefix {
								continue
							}
							ino, _ := strconv.ParseUint(parts[0][prefix+1:], 10, 64)
							indx, _ := strconv.Atoi(parts[1])
							_ = m.deleteChunk(Ino(ino), uint32(indx))
						}
					}
				}
			}
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

func (m *rueidisMeta) cleanupOldSliceRefs(delete bool) {
	if m.compat == nil {
		m.redisMeta.cleanupOldSliceRefs(delete)
		return
	}

	ctx := Background()
	pattern := m.prefix + "k*"
	var cursor uint64

	for {
		keys, next, err := m.compat.Scan(ctx, cursor, pattern, 10000).Result()
		if err != nil {
			logger.Warnf("scan %s: %v", pattern, err)
			return
		}
		if len(keys) > 0 {
			vals, err := m.compat.MGet(ctx, keys...).Result()
			if err != nil {
				logger.Warnf("mget slices: %v", err)
				continue
			}
			var todel []string
			for i, raw := range vals {
				if raw == nil {
					continue
				}
				var val string
				switch v := raw.(type) {
				case string:
					val = v
				case []byte:
					val = string(v)
				default:
					logger.Warnf("unexpected value type %T for key %s", raw, keys[i])
					continue
				}
				if strings.HasPrefix(val, m.prefix+"-") || val == "0" {
					todel = append(todel, keys[i])
				} else {
					vv, err := strconv.Atoi(val)
					if err != nil {
						logger.Warnf("invalid slice ref %s=%s", keys[i], val)
						continue
					}
					if err := m.compat.HIncrBy(ctx, m.sliceRefs(), keys[i], int64(vv)).Err(); err != nil {
						logger.Warnf("HIncrBy sliceRefs %s: %v", keys[i], err)
						continue
					}
					if err := m.compat.DecrBy(ctx, keys[i], int64(vv)).Err(); err != nil {
						logger.Warnf("DecrBy %s: %v", keys[i], err)
					} else {
						logger.Infof("move refs %d for slice %s", vv, keys[i])
					}
				}
			}
			if delete && len(todel) > 0 {
				if err := m.compat.Del(ctx, todel...).Err(); err != nil {
					logger.Warnf("Del old slice refs: %v", err)
				}
			}
		}
		if next == 0 {
			return
		}
		cursor = next
	}
}

func (m *rueidisMeta) cleanupLeakedInodes(delete bool) {
	if m.compat == nil {
		m.redisMeta.cleanupLeakedInodes(delete)
		return
	}

	ctx := Background()
	foundInodes := make(map[Ino]struct{})
	foundInodes[RootInode] = struct{}{}
	foundInodes[TrashInode] = struct{}{}
	cutoff := time.Now().Add(-time.Hour)
	prefix := len(m.prefix)

	patternDirs := m.prefix + "d[0-9]*"
	var cursor uint64
	for {
		keys, next, err := m.compat.Scan(ctx, cursor, patternDirs, 10000).Result()
		if err != nil {
			logger.Warnf("scan dirs %s: %v", patternDirs, err)
			return
		}
		for _, key := range keys {
			ino, err := strconv.Atoi(key[prefix+1:])
			if err != nil {
				continue
			}
			var entries []*Entry
			if eno := m.doReaddir(ctx, Ino(ino), 0, &entries, 0); eno != syscall.ENOENT && eno != 0 {
				logger.Errorf("readdir %d: %s", ino, eno)
				return
			}
			for _, e := range entries {
				foundInodes[e.Inode] = struct{}{}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	patternInodes := m.prefix + "i*"
	cursor = 0
	for {
		keys, next, err := m.compat.Scan(ctx, cursor, patternInodes, 10000).Result()
		if err != nil {
			logger.Warnf("scan inodes %s: %v", patternInodes, err)
			return
		}
		if len(keys) > 0 {
			vals, err := m.compat.MGet(ctx, keys...).Result()
			if err != nil {
				logger.Warnf("mget inodes: %v", err)
				continue
			}
			for i, raw := range vals {
				if raw == nil {
					continue
				}
				buf, ok := raw.(string)
				if !ok {
					if b, bOk := raw.([]byte); bOk {
						buf = string(b)
					} else {
						logger.Warnf("unexpected inode value type %T for %s", raw, keys[i])
						continue
					}
				}
				var attr Attr
				m.parseAttr([]byte(buf), &attr)
				ino, err := strconv.Atoi(keys[i][prefix+1:])
				if err != nil {
					continue
				}
				if _, ok := foundInodes[Ino(ino)]; !ok && time.Unix(attr.Ctime, 0).Before(cutoff) {
					logger.Infof("found dangling inode: %s %+v", keys[i], attr)
					if delete {
						if err := m.doDeleteSustainedInode(0, Ino(ino)); err != nil {
							logger.Errorf("delete leaked inode %d : %v", ino, err)
						}
					}
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
}

func (m *rueidisMeta) doCleanupDetachedNode(ctx Context, ino Ino) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doCleanupDetachedNode(ctx, ino)
	}

	exists, err := m.compat.Exists(ctx, m.inodeKey(ino)).Result()
	if err != nil || exists == 0 {
		return errno(err)
	}

	rmConcurrent := make(chan int, 10)
	if eno := m.emptyDir(ctx, ino, true, nil, rmConcurrent); eno != 0 {
		return eno
	}

	m.updateStats(-align4K(0), -1)

	err = m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
		_, e := tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Del(ctx, m.inodeKey(ino))
			pipe.Del(ctx, m.xattrKey(ino))
			pipe.DecrBy(ctx, m.usedSpaceKey(), align4K(0))
			pipe.Decr(ctx, m.totalInodesKey())
			field := ino.String()
			pipe.HDel(ctx, m.dirUsedInodesKey(), field)
			pipe.HDel(ctx, m.dirDataLengthKey(), field)
			pipe.HDel(ctx, m.dirUsedSpaceKey(), field)
			pipe.ZRem(ctx, m.detachedNodes(), field)
			return nil
		})
		return e
	}, m.inodeKey(ino), m.xattrKey(ino))

	return errno(err)
}

func (m *rueidisMeta) doFindDetachedNodes(t time.Time) []Ino {
	if m.compat == nil {
		return m.redisMeta.doFindDetachedNodes(t)
	}

	rng := rueidiscompat.ZRangeBy{
		Min: "-inf",
		Max: strconv.FormatInt(t.Unix(), 10),
	}
	vals, err := m.compat.ZRangeByScore(Background(), m.detachedNodes(), rng).Result()
	if err != nil {
		logger.Errorf("Scan detached nodes error: %v", err)
		return nil
	}
	inodes := make([]Ino, 0, len(vals))
	for _, node := range vals {
		inode, err := strconv.ParseUint(node, 10, 64)
		if err != nil {
			continue
		}
		inodes = append(inodes, Ino(inode))
	}
	return inodes
}

func (m *rueidisMeta) doAttachDirNode(ctx Context, parent Ino, dstIno Ino, name string) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doAttachDirNode(ctx, parent, dstIno, name)
	}

	var pattr Attr
	err := m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
		cmd := tx.Get(ctx, m.inodeKey(parent))
		data, err := cmd.Bytes()
		if err != nil {
			if err == rueidiscompat.Nil {
				return syscall.ENOENT
			}
			return err
		}
		m.parseAttr(data, &pattr)
		if pattr.Typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if pattr.Parent > TrashInode {
			return syscall.ENOENT
		}
		if (pattr.Flags & FlagImmutable) != 0 {
			return syscall.EPERM
		}

		exist, err := tx.HExists(ctx, m.entryKey(parent), name).Result()
		if err != nil {
			return err
		}
		if exist {
			return syscall.EEXIST
		}

		_, err = tx.TxPipelined(ctx, func(p rueidiscompat.Pipeliner) error {
			p.HSet(ctx, m.entryKey(parent), name, m.packEntry(TypeDirectory, dstIno))
			pattr.Nlink++
			now := time.Now()
			pattr.Mtime = now.Unix()
			pattr.Mtimensec = uint32(now.Nanosecond())
			pattr.Ctime = now.Unix()
			pattr.Ctimensec = uint32(now.Nanosecond())
			p.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
			p.ZRem(ctx, m.detachedNodes(), dstIno.String())
			return nil
		})
		return err
	}, m.inodeKey(parent), m.entryKey(parent))

	return errno(err)
}

func (m *rueidisMeta) doTouchAtime(ctx Context, inode Ino, attr *Attr, now time.Time) (bool, error) {
	if m.compat == nil {
		return m.redisMeta.doTouchAtime(ctx, inode, attr, now)
	}

	var updated bool
	err := m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
		cmd := tx.Get(ctx, m.inodeKey(inode))
		data, err := cmd.Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(data, attr)
		if !m.atimeNeedsUpdate(attr, now) {
			return nil
		}
		attr.Atime = now.Unix()
		attr.Atimensec = uint32(now.Nanosecond())
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(attr), 0)
			return nil
		})
		if err == nil {
			updated = true
		}
		return err
	}, m.inodeKey(inode))
	return updated, err
}

func (m *rueidisMeta) doSetFacl(ctx Context, ino Ino, aclType uint8, rule *aclAPI.Rule) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doSetFacl(ctx, ino, aclType, rule)
	}

	err := m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
		val, err := tx.Get(ctx, m.inodeKey(ino)).Bytes()
		if err != nil {
			return err
		}
		attr := &Attr{}
		m.parseAttr(val, attr)

		if ctx.Uid() != 0 && ctx.Uid() != attr.Uid {
			return syscall.EPERM
		}
		if attr.Flags&FlagImmutable != 0 {
			return syscall.EPERM
		}

		oriACL := getAttrACLId(attr, aclType)
		oriMode := attr.Mode

		if ctx.Uid() != 0 && !inGroup(ctx, attr.Gid) {
			attr.Mode &= 05777
		}

		if rule.IsEmpty() {
			setAttrACLId(attr, aclType, aclAPI.None)
			attr.Mode &= 07000
			attr.Mode |= ((rule.Owner & 7) << 6) | ((rule.Group & 7) << 3) | (rule.Other & 7)
		} else if rule.IsMinimal() && aclType == aclAPI.TypeAccess {
			setAttrACLId(attr, aclType, aclAPI.None)
			attr.Mode &= 07000
			attr.Mode |= ((rule.Owner & 7) << 6) | ((rule.Group & 7) << 3) | (rule.Other & 7)
		} else {
			rule.InheritPerms(attr.Mode)
			aclId, err := m.insertACLCompat(ctx, tx, rule)
			if err != nil {
				return err
			}
			setAttrACLId(attr, aclType, aclId)
			if aclType == aclAPI.TypeAccess {
				attr.Mode &= 07000
				attr.Mode |= ((rule.Owner & 7) << 6) | ((rule.Mask & 7) << 3) | (rule.Other & 7)
			}
		}

		if oriACL != getAttrACLId(attr, aclType) || oriMode != attr.Mode {
			now := time.Now()
			attr.Ctime = now.Unix()
			attr.Ctimensec = uint32(now.Nanosecond())
			_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
				pipe.Set(ctx, m.inodeKey(ino), m.marshal(attr), 0)
				return nil
			})
			return err
		}
		return nil
	}, m.inodeKey(ino))

	return errno(err)
}

func (m *rueidisMeta) tryLoadMissACLsCompat(ctx Context, tx rueidiscompat.Tx) error {
	missIds := m.aclCache.GetMissIds()
	if len(missIds) == 0 {
		return nil
	}
	missKeys := make([]string, len(missIds))
	for i, id := range missIds {
		missKeys[i] = strconv.FormatUint(uint64(id), 10)
	}

	vals, err := tx.HMGet(ctx, m.aclKey(), missKeys...).Result()
	if err != nil {
		return err
	}
	for i, data := range vals {
		var rule aclAPI.Rule
		if data != nil {
			switch v := data.(type) {
			case string:
				rule.Decode([]byte(v))
			case []byte:
				rule.Decode(v)
			default:
				logger.Warnf("unexpected ACL value type %T for %s", data, missKeys[i])
			}
		}
		m.aclCache.Put(missIds[i], &rule)
	}
	return nil
}

func (m *rueidisMeta) insertACLCompat(ctx Context, tx rueidiscompat.Tx, rule *aclAPI.Rule) (uint32, error) {
	if rule == nil || rule.IsEmpty() {
		return aclAPI.None, nil
	}

	if err := m.tryLoadMissACLsCompat(ctx, tx); err != nil {
		logger.Warnf("SetFacl: load miss acls error: %v", err)
	}

	if aclId := m.aclCache.GetId(rule); aclId != aclAPI.None {
		return aclId, nil
	}

	newId, err := m.incrCounter(aclCounter, 1)
	if err != nil {
		return aclAPI.None, err
	}
	aclId := uint32(newId)

	ok, err := tx.HSetNX(ctx, m.aclKey(), strconv.FormatUint(uint64(aclId), 10), rule.Encode()).Result()
	if err != nil {
		return aclAPI.None, err
	}
	if !ok {
		return aclId, nil
	}
	m.aclCache.Put(aclId, rule)
	return aclId, nil
}

func (m *rueidisMeta) getACLCompat(ctx Context, tx rueidiscompat.Tx, id uint32) (*aclAPI.Rule, error) {
	if id == aclAPI.None {
		return nil, nil
	}
	if cRule := m.aclCache.Get(id); cRule != nil {
		return cRule, nil
	}

	key := strconv.FormatUint(uint64(id), 10)
	var (
		val []byte
		err error
	)
	if tx != nil {
		data, e := tx.HGet(ctx, m.aclKey(), key).Result()
		if e != nil {
			return nil, e
		}
		val = []byte(data)
	} else {
		val, err = m.compat.HGet(ctx, m.aclKey(), key).Bytes()
		if err != nil {
			return nil, err
		}
	}
	if val == nil {
		return nil, syscall.EIO
	}

	rule := &aclAPI.Rule{}
	rule.Decode(val)
	m.aclCache.Put(id, rule)
	return rule, nil
}

func (m *rueidisMeta) doGetFacl(ctx Context, ino Ino, aclType uint8, aclId uint32, rule *aclAPI.Rule) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doGetFacl(ctx, ino, aclType, aclId, rule)
	}

	if aclId == aclAPI.None {
		val, err := m.compat.Get(ctx, m.inodeKey(ino)).Bytes()
		if err != nil {
			return errno(err)
		}
		attr := &Attr{}
		m.parseAttr(val, attr)
		m.of.Update(ino, attr)
		aclId = getAttrACLId(attr, aclType)
	}

	a, err := m.getACLCompat(ctx, nil, aclId)
	if err != nil {
		return errno(err)
	}
	if a == nil {
		return ENOATTR
	}
	*rule = *a
	return 0
}

func (m *rueidisMeta) loadDumpedACLs(ctx Context) error {
	if m.compat == nil {
		return m.redisMeta.loadDumpedACLs(ctx)
	}

	id2Rule := m.aclCache.GetAll()
	if len(id2Rule) == 0 {
		return nil
	}

	return m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
		maxId := uint32(0)
		acls := make(map[string]interface{}, len(id2Rule))
		for id, rule := range id2Rule {
			if id > maxId {
				maxId = id
			}
			acls[strconv.FormatUint(uint64(id), 10)] = rule.Encode()
		}
		if len(acls) > 0 {
			if err := tx.HSet(ctx, m.aclKey(), acls).Err(); err != nil {
				return err
			}
		}
		return tx.Set(ctx, m.prefix+aclCounter, maxId, 0).Err()
	}, m.aclKey())
}

func (m *rueidisMeta) doCleanupDelayedSlices(ctx Context, edge int64) (int, error) {
	if m.compat == nil {
		return m.redisMeta.doCleanupDelayedSlices(ctx, edge)
	}

	var (
		count  int
		ss     []Slice
		rs     []*rueidiscompat.IntCmd
		cursor uint64
		delKey = m.delSlices()
	)

	for {
		keys, next, err := m.compat.HScan(ctx, delKey, cursor, "*", 10000).Result()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			logger.Warnf("HSCAN %s: %v", delKey, err)
			return count, err
		}
		if len(keys) > 0 {
			for i := 0; i < len(keys); i += 2 {
				if ctx.Canceled() {
					return count, ctx.Err()
				}
				key := keys[i]
				ps := strings.Split(key, "_")
				if len(ps) != 2 {
					logger.Warnf("Invalid key %s", key)
					continue
				}
				ts, e := strconv.ParseUint(ps[1], 10, 64)
				if e != nil {
					logger.Warnf("Invalid key %s", key)
					continue
				}
				if ts >= uint64(edge) {
					continue
				}

				if err := m.compat.Watch(ctx, func(tx rueidiscompat.Tx) error {
					ss = ss[:0]
					rs = rs[:0]
					val, e := tx.HGet(ctx, delKey, key).Result()
					if e == rueidiscompat.Nil {
						return nil
					} else if e != nil {
						return e
					}
					buf := []byte(val)
					m.decodeDelayedSlices(buf, &ss)
					if len(ss) == 0 {
						return fmt.Errorf("invalid value for delSlices %s: %v", key, buf)
					}
					_, e = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
						for _, s := range ss {
							rs = append(rs, pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.Id, s.Size), -1))
						}
						pipe.HDel(ctx, delKey, key)
						return nil
					})
					return e
				}, delKey); err != nil {
					logger.Warnf("Cleanup delSlices %s: %v", key, err)
					continue
				}

				for i, s := range ss {
					if rs[i].Err() == nil && rs[i].Val() < 0 {
						m.deleteSlice(s.Id, s.Size)
						count++
					}
					if ctx.Canceled() {
						return count, ctx.Err()
					}
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return count, nil
		}
		return count, err
	}
	return count, nil
}

func (m *rueidisMeta) doCleanStaleSession(sid uint64) error {
	if m.compat == nil {
		return m.redisMeta.doCleanStaleSession(sid)
	}
	var fail bool
	ctx := Background()
	ssid := strconv.FormatInt(int64(sid), 10)
	key := m.lockedKey(sid)

	inodes, err := m.compat.SMembers(ctx, key).Result()
	if err == nil {
		for _, k := range inodes {
			owners, err := m.compat.HKeys(ctx, k).Result()
			if err != nil {
				logger.Warnf("HKeys %s: %v", k, err)
				fail = true
				continue
			}
			var fields []string
			for _, o := range owners {
				if strings.Split(o, "_")[0] == ssid {
					fields = append(fields, o)
				}
			}
			if len(fields) > 0 {
				if err = m.compat.HDel(ctx, k, fields...).Err(); err != nil {
					logger.Warnf("HDel %s %v: %v", k, fields, err)
					fail = true
					continue
				}
			}
			if err = m.compat.SRem(ctx, key, k).Err(); err != nil {
				logger.Warnf("SRem %s %s: %v", key, k, err)
				fail = true
			}
		}
	} else {
		logger.Warnf("SMembers %s: %v", key, err)
		fail = true
	}

	key = m.sustained(sid)
	inodes, err = m.compat.SMembers(ctx, key).Result()
	if err == nil {
		for _, sinode := range inodes {
			inode, _ := strconv.ParseUint(sinode, 10, 64)
			if err = m.doDeleteSustainedInode(sid, Ino(inode)); err != nil {
				logger.Warnf("Delete sustained inode %d of sid %d: %v", inode, sid, err)
				fail = true
			}
		}
	} else {
		logger.Warnf("SMembers %s: %v", key, err)
		fail = true
	}

	if !fail {
		if err := m.compat.HDel(ctx, m.sessionInfos(), ssid).Err(); err != nil {
			logger.Warnf("HDel sessionInfos %s: %v", ssid, err)
			fail = true
		}
	}
	if fail {
		return fmt.Errorf("failed to clean up sid %d", sid)
	}
	if n, err := m.compat.ZRem(ctx, m.allSessions(), ssid).Result(); err != nil {
		return err
	} else if n == 1 {
		return nil
	}
	return m.compat.ZRem(ctx, legacySessions, ssid).Err()
}

func (m *rueidisMeta) newDirHandler(inode Ino, plus bool, entries []*Entry) DirHandler {
	if m.compat == nil {
		return m.redisMeta.newDirHandler(inode, plus, entries)
	}
	return &rueidisDirHandler{
		en:          m,
		inode:       inode,
		plus:        plus,
		initEntries: entries,
		batchNum:    DirBatchNum["redis"],
	}
}

type rueidisDirHandler struct {
	sync.Mutex
	inode       Ino
	plus        bool
	en          *rueidisMeta
	initEntries []*Entry
	entries     []*Entry
	indexes     map[string]int
	readOff     int
	batchNum    int
}

func (s *rueidisDirHandler) Close() {
	s.Lock()
	s.entries = nil
	s.readOff = 0
	s.Unlock()
}

func (s *rueidisDirHandler) Delete(name string) {
	s.Lock()
	defer s.Unlock()

	if len(s.entries) == 0 {
		return
	}

	if idx, ok := s.indexes[name]; ok && idx >= s.readOff {
		delete(s.indexes, name)
		n := len(s.entries)
		if idx < n-1 {
			// TODO: sorted
			s.entries[idx] = s.entries[n-1]
			s.indexes[string(s.entries[idx].Name)] = idx
		}
		s.entries = s.entries[:n-1]
	}
}

func (s *rueidisDirHandler) Insert(inode Ino, name string, attr *Attr) {
	s.Lock()
	defer s.Unlock()

	if len(s.entries) == 0 {
		return
	}

	// TODO: sorted
	s.entries = append(s.entries, &Entry{Inode: inode, Name: []byte(name), Attr: attr})
	s.indexes[name] = len(s.entries) - 1
}

func (s *rueidisDirHandler) List(ctx Context, offset int) ([]*Entry, syscall.Errno) {
	var prefix []*Entry
	if offset < len(s.initEntries) {
		prefix = s.initEntries[offset:]
		offset = 0
	} else {
		offset -= len(s.initEntries)
	}

	s.Lock()
	defer s.Unlock()
	if s.entries == nil {
		entries, err := s.loadEntries(ctx)
		if err != nil {
			return nil, errno(err)
		}
		s.entries = entries
		indexes := make(map[string]int, len(entries))
		for i, e := range entries {
			indexes[string(e.Name)] = i
		}
		s.indexes = indexes
	}

	size := len(s.entries) - offset
	if size > s.batchNum {
		size = s.batchNum
	}
	s.readOff = offset + size
	entries := s.entries[offset : offset+size]
	if len(prefix) > 0 {
		entries = append(prefix, entries...)
	}
	return entries, 0
}

func (s *rueidisDirHandler) Read(offset int) {
	s.readOff = offset - len(s.initEntries)
}

func (s *rueidisDirHandler) loadEntries(ctx Context) ([]*Entry, error) {
	var (
		entries []*Entry
		cursor  uint64
		key     = s.en.entryKey(s.inode)
	)

	for {
		kvs, next, err := s.en.compat.HScan(ctx, key, cursor, "*", 10000).Result()
		if err != nil {
			return nil, err
		}
		if len(kvs) > 0 {
			newEntries := make([]Entry, len(kvs)/2)
			newAttrs := make([]Attr, len(kvs)/2)
			for i := 0; i < len(kvs); i += 2 {
				typ, ino := s.en.parseEntry([]byte(kvs[i+1]))
				if kvs[i] == "" {
					logger.Errorf("Corrupt entry with empty name: inode %d parent %d", ino, s.inode)
					continue
				}
				ent := &newEntries[i/2]
				ent.Inode = ino
				ent.Name = []byte(kvs[i])
				ent.Attr = &newAttrs[i/2]
				ent.Attr.Typ = typ
				entries = append(entries, ent)
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}

	if s.en.conf.SortDir {
		sort.Slice(entries, func(i, j int) bool {
			return string(entries[i].Name) < string(entries[j].Name)
		})
	}
	if s.plus {
		nEntries := len(entries)
		var err error
		if nEntries <= s.batchNum {
			err = s.en.fillAttr(ctx, entries)
		} else {
			eg := errgroup.Group{}
			eg.SetLimit(2)
			for i := 0; i < nEntries; i += s.batchNum {
				var es []*Entry
				if i+s.batchNum > nEntries {
					es = entries[i:]
				} else {
					es = entries[i : i+s.batchNum]
				}
				eg.Go(func() error {
					return s.en.fillAttr(ctx, es)
				})
			}
			err = eg.Wait()
		}
		if err != nil {
			return nil, err
		}
	}
	return entries, nil
}

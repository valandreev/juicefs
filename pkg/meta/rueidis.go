//go:build !norueidis
// +build !norueidis

// Rueidis metadata engine implementation.
//
// This file provides a high-performance Redis-compatible metadata engine using
// the Rueidis client library. Rueidis offers automatic client-side caching with
// broadcast mode tracking, providing better performance than the standard Redis
// client for read-heavy workloads.
//
// Build tags: Compile with `-tags norueidis` to exclude this implementation.

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
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
	cacheTTL  time.Duration // client-side cache TTL for read operations
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

	// Parse and extract cache-ttl query parameter before passing to rueidis.ParseURL
	// Default: 5 seconds (good default for most scenarios)
	cacheTTL := 10 * time.Second
	cleanAddr := addr
	if u, err := url.Parse(uri); err == nil {
		if ttlStr := u.Query().Get("cache-ttl"); ttlStr != "" {
			if parsed, err := time.ParseDuration(ttlStr); err == nil && parsed >= 0 {
				cacheTTL = parsed
			}
			// Strip cache-ttl from query params for rueidis.ParseURL
			q := u.Query()
			q.Del("cache-ttl")
			u.RawQuery = q.Encode()
			// Extract just the address part (without scheme)
			cleanAddr = u.Host + u.Path
			if u.RawQuery != "" {
				cleanAddr += "?" + u.RawQuery
			}
		}
	}

	cleanURI := canonical + "://" + cleanAddr
	opt, err := rueidis.ParseURL(cleanURI)
	if err != nil {
		return nil, fmt.Errorf("rueidis parse %s: %w", cleanURI, err)
	}

	delegate, err := newRedisMeta(canonical, cleanAddr, conf)
	if err != nil {
		return nil, err
	}

	base, ok := delegate.(*redisMeta)
	if !ok {
		return nil, fmt.Errorf("unexpected meta implementation %T", delegate)
	}

	// Enable server-assisted client-side caching with broadcast mode
	// This enables automatic cache invalidation when keys change on the server
	// We track all keys with the metadata prefix to ensure proper invalidation
	prefix := base.prefix
	if prefix == "" {
		prefix = "jfs"
	}
	opt.ClientTrackingOptions = []string{
		"PREFIX", prefix + "i", // inode keys
		"PREFIX", prefix + "d", // directory entry keys
		"PREFIX", prefix + "c", // chunk keys
		"PREFIX", prefix + "x", // xattr keys
		"PREFIX", prefix + "p", // parent keys
		"PREFIX", prefix + "s", // symlink keys
		"BCAST", // broadcast mode - automatic invalidation notifications
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
		cacheTTL:  cacheTTL,
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

// replaceErrnoCompat wraps a transaction function to convert syscall.Errno to errNo
// for proper error handling in rueidiscompat Watch transactions
func replaceErrnoCompat(txf func(tx rueidiscompat.Tx) error) func(tx rueidiscompat.Tx) error {
	return func(tx rueidiscompat.Tx) error {
		err := txf(tx)
		if eno, ok := err.(syscall.Errno); ok {
			err = errNo(eno)
		}
		return err
	}
}

// txn wraps rueidiscompat.Watch with retry logic and pessimistic locking
// to match redisMeta.txn behavior. This ensures transaction consistency
// and handles optimistic lock failures (TxFailedErr) with exponential backoff.
func (m *rueidisMeta) txn(ctx Context, txf func(tx rueidiscompat.Tx) error, keys ...string) error {
	if m.compat == nil {
		// If compat is not initialized, this is a critical error
		// This should never happen in production as newRueidisMeta initializes compat
		return fmt.Errorf("compat adapter not initialized in rueidisMeta.txn")
	}

	if m.conf.ReadOnly {
		return syscall.EROFS
	}

	for _, k := range keys {
		if !strings.HasPrefix(k, m.prefix) {
			panic(fmt.Sprintf("Invalid key %s not starts with prefix %s", k, m.prefix))
		}
	}

	var khash = fnv.New32()
	_, _ = khash.Write([]byte(keys[0]))
	h := uint(khash.Sum32())

	start := time.Now()
	defer func() { m.txDist.Observe(time.Since(start).Seconds()) }()

	m.txLock(h)
	defer m.txUnlock(h)

	var (
		retryOnFailure = false
		lastErr        error
		method         string
	)

	for i := 0; i < 50; i++ {
		if ctx.Canceled() {
			return syscall.EINTR
		}

		err := m.compat.Watch(ctx, replaceErrnoCompat(txf), keys...)

		// Handle errNo type (internal error wrapper)
		if eno, ok := err.(errNo); ok {
			if eno == 0 {
				err = nil
			} else {
				err = syscall.Errno(eno)
			}
		}

		// rueidiscompat.TxFailedErr indicates optimistic lock failure - retry
		if err == rueidiscompat.TxFailedErr {
			if method == "" {
				method = callerName(ctx)
			}
			m.txRestart.WithLabelValues(method).Add(1)
			logger.Debugf("Transaction failed (optimistic lock), restart it (tried %d)", i+1)
			lastErr = err
			time.Sleep(time.Millisecond * time.Duration(rand.Int()%((i+1)*(i+1))))
			continue
		}

		// Check if error should trigger retry
		if err != nil && m.shouldRetry(err, retryOnFailure) {
			if method == "" {
				method = callerName(ctx)
			}
			m.txRestart.WithLabelValues(method).Add(1)
			logger.Debugf("Transaction failed, restart it (tried %d): %s", i+1, err)
			lastErr = err
			time.Sleep(time.Millisecond * time.Duration(rand.Int()%((i+1)*(i+1))))
			continue
		}

		if err == nil && i > 1 {
			logger.Warnf("Transaction succeeded after %d tries (%s), keys: %v, method: %s, last error: %s",
				i+1, time.Since(start), keys, method, lastErr)
		}

		return err
	}

	logger.Warnf("Already tried 50 times, returning: %s", lastErr)
	return lastErr
}

func (m *rueidisMeta) doFlushStats() {}

func (m *rueidisMeta) doSyncVolumeStat(ctx Context) error {
	if m.compat == nil {
		return m.redisMeta.doSyncVolumeStat(ctx)
	}

	if m.conf.ReadOnly {
		return syscall.EROFS
	}

	var used, inodes int64
	if err := m.hscan(ctx, m.dirUsedSpaceKey(), func(keys []string) error {
		for i := 0; i < len(keys); i += 2 {
			v, err := strconv.ParseInt(keys[i+1], 10, 64)
			if err != nil {
				logger.Warnf("invalid used space: %s->%s", keys[i], keys[i+1])
				continue
			}
			used += v
		}
		return nil
	}); err != nil {
		return err
	}

	if err := m.hscan(ctx, m.dirUsedInodesKey(), func(keys []string) error {
		for i := 0; i < len(keys); i += 2 {
			v, err := strconv.ParseInt(keys[i+1], 10, 64)
			if err != nil {
				logger.Warnf("invalid used inode: %s->%s", keys[i], keys[i+1])
				continue
			}
			inodes += v
		}
		return nil
	}); err != nil {
		return err
	}

	var inoKeys []string
	if err := m.scan(ctx, m.prefix+"session*", func(keys []string) error {
		for i := 0; i < len(keys); i += 2 {
			key := keys[i]
			if key == "sessions" {
				continue
			}

			sustInodes, err := m.compat.SMembers(ctx, key).Result()
			if err != nil {
				logger.Warnf("SMembers %s: %v", key, err)
				continue
			}
			for _, sinode := range sustInodes {
				ino, err := strconv.ParseInt(sinode, 10, 64)
				if err != nil {
					logger.Warnf("invalid sustained: %s->%s", key, sinode)
					continue
				}
				inoKeys = append(inoKeys, m.inodeKey(Ino(ino)))
			}
		}
		return nil
	}); err != nil {
		return err
	}

	batch := 1000
	for i := 0; i < len(inoKeys); i += batch {
		end := i + batch
		if end > len(inoKeys) {
			end = len(inoKeys)
		}
		values, err := m.compat.MGet(ctx, inoKeys[i:end]...).Result()
		if err != nil {
			return err
		}
		var attr Attr
		for _, v := range values {
			if v != nil {
				if s, ok := v.(string); ok {
					m.parseAttr([]byte(s), &attr)
					used += align4K(attr.Length)
					inodes += 1
				}
			}
		}
	}

	if err := m.scanTrashEntry(ctx, func(_ Ino, length uint64) {
		used += align4K(length)
		inodes += 1
	}); err != nil {
		return err
	}

	logger.Debugf("Used space: %s, inodes: %d", humanize.IBytes(uint64(used)), inodes)
	if err := m.compat.Set(ctx, m.totalInodesKey(), strconv.FormatInt(inodes, 10), 0).Err(); err != nil {
		return fmt.Errorf("set total inodes: %w", err)
	}
	return m.compat.Set(ctx, m.usedSpaceKey(), strconv.FormatInt(used, 10), 0).Err()
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
	// Rueidis stores nextInode/nextChunk as value-1, add 1 when reading
	if name == "nextInode" || name == "nextChunk" {
		return v + 1, err
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

func (m *rueidisMeta) setIfSmall(name string, value, diff int64) (bool, error) {
	if m.compat == nil {
		return m.redisMeta.setIfSmall(name, value, diff)
	}

	ctx := Background()
	name = m.prefix + name
	ctx = ctx.WithValue(txMethodKey{}, "setIfSmall:"+name)
	var changed bool
	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		changed = false
		old, err := tx.Get(ctx, name).Int64()
		if err != nil && err != rueidiscompat.Nil {
			return err
		}
		if old > value-diff {
			return nil
		}
		changed = true
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, name, value, 0)
			return nil
		})
		return err
	}, name)

	return changed, err
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

	// Check if format exists - must check err, not just body
	// because empty []byte is not nil even when key doesn't exist
	var formatExists bool
	if err == nil && len(body) > 0 {
		formatExists = true
	}

	if formatExists {
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
	if formatExists {
		return nil
	}

	// Initialize counters with proper offset values
	// For nextInode and nextChunk, we store value-1 because incrCounter returns IncrBy+1
	// Root inode (1) exists, so nextInode starts at 2, stored as 2-1=1
	// First chunk should be ID 1, so after first IncrBy(100), we want value 100 stored (not 99)
	// This means we initialize to 0 (not -1), so IncrBy(0+100)=100, incrCounter returns 101
	// Then NewSlice: next = 101-100 = 1, maxid = 101
	pipe := m.compat.Pipeline()
	ctx2 := Background()
	pipe.Set(ctx2, m.counterKey("nextInode"), 1, 0)   // Root inode (1) exists, next is 2
	pipe.Set(ctx2, m.counterKey("nextChunk"), 0, 0)   // First chunk will be 1
	pipe.Set(ctx2, m.counterKey("nextSession"), 0, 0) // No sessions yet
	pipe.Set(ctx2, m.counterKey("nextTrash"), 0, 0)   // No trash entries yet
	if _, err := pipe.Exec(ctx2); err != nil {
		return err
	}

	// Create root inode
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

func (m *rueidisMeta) Reset() error {
	if m.compat == nil {
		return m.redisMeta.Reset()
	}

	ctx := Background()
	if m.prefix != "" {
		return m.scan(ctx, "*", func(keys []string) error {
			if len(keys) == 0 {
				return nil
			}
			return m.compat.Del(ctx, keys...).Err()
		})
	}
	return m.compat.FlushDB(ctx).Err()
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

func (m *rueidisMeta) doFindStaleSessions(limit int) ([]uint64, error) {
	if m.compat == nil {
		return m.redisMeta.doFindStaleSessions(limit)
	}

	ctx := Background()
	rng := rueidiscompat.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(time.Now().Unix(), 10),
		Count: int64(limit),
	}
	vals, err := m.compat.ZRangeByScore(ctx, m.allSessions(), rng).Result()
	if err != nil {
		return nil, err
	}
	sids := make([]uint64, len(vals))
	for i, v := range vals {
		sids[i], _ = strconv.ParseUint(v, 10, 64)
	}
	limit -= len(sids)
	if limit <= 0 {
		return sids, nil
	}

	legacyRange := rueidiscompat.ZRangeBy{
		Min:   "-inf",
		Max:   strconv.FormatInt(time.Now().Add(-5*time.Minute).Unix(), 10),
		Count: int64(limit),
	}
	legacyVals, err := m.compat.ZRangeByScore(ctx, legacySessions, legacyRange).Result()
	if err != nil {
		logger.Errorf("Scan stale legacy sessions: %v", err)
		return sids, nil
	}
	for _, v := range legacyVals {
		sid, _ := strconv.ParseUint(v, 10, 64)
		sids = append(sids, sid)
	}
	return sids, nil
}

func (m *rueidisMeta) doRefreshSession() error {
	if m.compat == nil {
		return m.redisMeta.doRefreshSession()
	}

	ctx := Background()
	ssid := strconv.FormatUint(m.sid, 10)
	ok, err := m.compat.HExists(ctx, m.sessionInfos(), ssid).Result()
	if err == nil && !ok {
		logger.Warnf("Session %d was stale and cleaned up, but now it comes back again", m.sid)
		err = m.compat.HSet(ctx, m.sessionInfos(), m.sid, m.newSessionInfo()).Err()
	}
	if err != nil {
		return err
	}

	return m.compat.ZAdd(ctx, m.allSessions(), rueidiscompat.Z{
		Score:  float64(m.expireTime()),
		Member: ssid,
	}).Err()
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
	if err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
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

func (m *rueidisMeta) scan(ctx context.Context, pattern string, f func([]string) error) error {
	if m.compat == nil {
		return m.redisMeta.scan(ctx, pattern, f)
	}

	var cursor uint64
	match := m.prefix + pattern
	for {
		keys, next, err := m.compat.Scan(ctx, cursor, match, 10000).Result()
		if err != nil {
			logger.Warnf("scan %s: %v", pattern, err)
			return err
		}
		if len(keys) > 0 {
			if err := f(keys); err != nil {
				return err
			}
		}
		if next == 0 {
			break
		}
		cursor = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (m *rueidisMeta) hscan(ctx context.Context, key string, f func([]string) error) error {
	if m.compat == nil {
		return m.redisMeta.hscan(ctx, key, f)
	}

	var cursor uint64
	for {
		keys, next, err := m.compat.HScan(ctx, key, cursor, "*", 10000).Result()
		if err != nil {
			logger.Warnf("HSCAN %s: %v", key, err)
			return err
		}
		if len(keys) > 0 {
			if err := f(keys); err != nil {
				return err
			}
		}
		if next == 0 {
			break
		}
		cursor = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return nil
}

func (m *rueidisMeta) ListSlices(ctx Context, slices map[Ino][]Slice, scanPending, delete bool, showProgress func()) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.ListSlices(ctx, slices, scanPending, delete, showProgress)
	}

	m.cleanupLeakedInodes(delete)
	m.cleanupLeakedChunks(delete)
	m.cleanupOldSliceRefs(delete)
	if delete {
		m.doCleanupSlices(ctx)
	}

	err := m.scan(ctx, "c*_*", func(keys []string) error {
		if len(keys) == 0 {
			return nil
		}
		pipe := m.compat.Pipeline()
		chunkKeys := make([]string, 0, len(keys))
		for _, key := range keys {
			chunkKeys = append(chunkKeys, key)
			pipe.LRange(ctx, key, 0, -1)
		}
		cmds, execErr := pipe.Exec(ctx)
		if execErr != nil {
			for i, c := range cmds {
				if c.Err() != nil {
					logger.Warnf("List slices with key %s failed: %v", chunkKeys[i], c.Err())
				}
			}
			return execErr
		}
		prefix := len(m.prefix)
		for i, cmd := range cmds {
			sliceCmd, ok := cmd.(*rueidiscompat.StringSliceCmd)
			if !ok {
				logger.Warnf("unexpected pipeline cmd type %T for key %s", cmd, chunkKeys[i])
				continue
			}
			vals, valErr := sliceCmd.Result()
			if valErr != nil {
				logger.Warnf("List slices read %s: %v", chunkKeys[i], valErr)
				continue
			}
			parts := strings.Split(chunkKeys[i][prefix+1:], "_")
			if len(parts) < 1 {
				logger.Warnf("invalid chunk key %s", chunkKeys[i])
				continue
			}
			inode, err := strconv.Atoi(parts[0])
			if err != nil {
				logger.Warnf("invalid chunk key %s: %v", chunkKeys[i], err)
				continue
			}
			ss := readSlices(vals)
			if ss == nil {
				logger.Errorf("Corrupt value for inode %d chunk key %s", inode, chunkKeys[i])
				continue
			}
			for _, s := range ss {
				if s.id > 0 {
					slices[Ino(inode)] = append(slices[Ino(inode)], Slice{Id: s.id, Size: s.size})
					if showProgress != nil {
						showProgress()
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		logger.Warnf("scan chunks: %v", err)
		return errno(err)
	}

	if scanPending {
		_ = m.hscan(Background(), m.sliceRefs(), func(keys []string) error {
			for i := 0; i < len(keys); i += 2 {
				key, val := keys[i], keys[i+1]
				if strings.HasPrefix(val, "-") {
					parts := strings.Split(key, "_")
					if len(parts) == 2 {
						id, _ := strconv.ParseUint(parts[0][1:], 10, 64)
						size, _ := strconv.ParseUint(parts[1], 10, 32)
						if id > 0 && size > 0 {
							slices[0] = append(slices[0], Slice{Id: id, Size: uint32(size)})
						}
					}
				}
			}
			return nil
		})
	}

	if m.getFormat().TrashDays == 0 {
		return 0
	}
	return errno(m.scanTrashSlices(ctx, func(ss []Slice, _ int64) (bool, error) {
		slices[1] = append(slices[1], ss...)
		if showProgress != nil {
			for range ss {
				showProgress()
			}
		}
		return false, nil
	}))
}

func (m *rueidisMeta) scanTrashSlices(ctx Context, scan trashSliceScan) error {
	if m.compat == nil {
		return m.redisMeta.scanTrashSlices(ctx, scan)
	}
	if scan == nil {
		return nil
	}

	delKeys := make(chan string, 1000)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = m.hscan(c, m.delSlices(), func(keys []string) error {
			for i := 0; i < len(keys); i += 2 {
				select {
				case delKeys <- keys[i]:
				case <-c.Done():
					return c.Err()
				}
			}
			return nil
		})
		close(delKeys)
	}()

	var ss []Slice
	var rs []*rueidiscompat.IntCmd
	for key := range delKeys {
		var clean bool
		err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
			ss = ss[:0]
			rs = rs[:0]
			val, err := tx.HGet(ctx, m.delSlices(), key).Result()
			if err == rueidiscompat.Nil {
				return nil
			} else if err != nil {
				return err
			}
			parts := strings.Split(key, "_")
			if len(parts) != 2 {
				return fmt.Errorf("invalid key %s", key)
			}
			ts, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return fmt.Errorf("invalid key %s, fail to parse timestamp", key)
			}

			m.decodeDelayedSlices([]byte(val), &ss)
			clean, err = scan(ss, ts)
			if err != nil {
				return err
			}
			if clean {
				_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
					for _, s := range ss {
						rs = append(rs, pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.Id, s.Size), -1))
					}
					pipe.HDel(ctx, m.delSlices(), key)
					return nil
				})
			}
			return err
		}, m.delSlices())
		if err != nil {
			return err
		}
		if clean && len(rs) == len(ss) {
			for i, s := range ss {
				if rs[i].Err() == nil && rs[i].Val() < 0 {
					m.deleteSlice(s.Id, s.Size)
				}
			}
		}
	}

	return nil
}

func (m *rueidisMeta) scanPendingSlices(ctx Context, scan pendingSliceScan) error {
	if m.compat == nil {
		return m.redisMeta.scanPendingSlices(ctx, scan)
	}
	if scan == nil {
		return nil
	}

	pendingKeys := make(chan string, 1000)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_ = m.hscan(c, m.sliceRefs(), func(keys []string) error {
			for i := 0; i < len(keys); i += 2 {
				val := keys[i+1]
				refs, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					logger.Warn(pkgerrors.Wrapf(err, "parse slice ref: %s", val))
					return nil
				}
				if refs < 0 {
					select {
					case pendingKeys <- keys[i]:
					case <-c.Done():
						return c.Err()
					}
				}
			}
			return nil
		})
		close(pendingKeys)
	}()

	for key := range pendingKeys {
		parts := strings.Split(key[1:], "_")
		if len(parts) != 2 {
			return fmt.Errorf("invalid key %s", key)
		}
		id, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil {
			return pkgerrors.Wrapf(err, "invalid key %s, fail to parse id", key)
		}
		size, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			return pkgerrors.Wrapf(err, "invalid key %s, fail to parse size", key)
		}
		clean, err := scan(id, uint32(size))
		if err != nil {
			return pkgerrors.Wrap(err, "scan pending slices")
		}
		if clean {
			// TODO: m.deleteSlice(id, uint32(size))
			_ = clean
		}
	}

	return nil
}

func (m *rueidisMeta) scanPendingFiles(ctx Context, scan pendingFileScan) error {
	if m.compat == nil {
		return m.redisMeta.scanPendingFiles(ctx, scan)
	}
	if scan == nil {
		return nil
	}

	visited := make(map[Ino]bool)
	start := int64(0)
	const batchSize = 1000

	for {
		pairs, err := m.compat.ZRangeWithScores(Background(), m.delfiles(), start, start+batchSize).Result()
		if err != nil {
			return err
		}
		if len(pairs) == 0 {
			break
		}
		for _, p := range pairs {
			v := p.Member
			ps := strings.Split(v, ":")
			if len(ps) != 2 {
				continue
			}
			inode, _ := strconv.ParseUint(ps[0], 10, 64)
			if visited[Ino(inode)] {
				continue
			}
			visited[Ino(inode)] = true
			size, _ := strconv.ParseUint(ps[1], 10, 64)
			if _, err := scan(Ino(inode), size, int64(p.Score)); err != nil {
				return err
			}
		}
		start += batchSize + 1
	}

	return nil
}

func (m *rueidisMeta) doCloneEntry(ctx Context, srcIno Ino, parent Ino, name string, ino Ino, originAttr *Attr, cmode uint8, cumask uint16, top bool) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doCloneEntry(ctx, srcIno, parent, name, ino, originAttr, cmode, cumask, top)
	}

	return errno(m.txn(ctx, func(tx rueidiscompat.Tx) error {
		a, err := tx.Get(ctx, m.inodeKey(srcIno)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(a, originAttr)
		attr := *originAttr
		if eno := m.Access(ctx, srcIno, MODE_MASK_R, &attr); eno != 0 {
			return eno
		}
		attr.Parent = parent
		now := time.Now()
		if cmode&CLONE_MODE_PRESERVE_ATTR == 0 {
			attr.Uid = ctx.Uid()
			attr.Gid = ctx.Gid()
			attr.Mode &= ^cumask
			attr.Atime = now.Unix()
			attr.Mtime = now.Unix()
			attr.Ctime = now.Unix()
			attr.Atimensec = uint32(now.Nanosecond())
			attr.Mtimensec = uint32(now.Nanosecond())
			attr.Ctimensec = uint32(now.Nanosecond())
		}
		// TODO: preserve hardlink
		if attr.Typ == TypeFile && attr.Nlink > 1 {
			attr.Nlink = 1
		}
		srcXattr, err := tx.HGetAll(ctx, m.xattrKey(srcIno)).Result()
		if err != nil {
			return err
		}

		var pattr Attr
		if top {
			if a, err := tx.Get(ctx, m.inodeKey(parent)).Bytes(); err != nil {
				return err
			} else {
				m.parseAttr(a, &pattr)
			}
			if pattr.Typ != TypeDirectory {
				return syscall.ENOTDIR
			}
			if (pattr.Flags & FlagImmutable) != 0 {
				return syscall.EPERM
			}
			if exist, err := tx.HExists(ctx, m.entryKey(parent), name).Result(); err != nil {
				return err
			} else if exist {
				return syscall.EEXIST
			}
			if eno := m.Access(ctx, parent, MODE_MASK_W|MODE_MASK_X, &pattr); eno != 0 {
				return eno
			}
		}

		_, err = tx.TxPipelined(ctx, func(p rueidiscompat.Pipeliner) error {
			p.Set(ctx, m.inodeKey(ino), m.marshal(&attr), 0)
			p.IncrBy(ctx, m.usedSpaceKey(), align4K(attr.Length))
			p.Incr(ctx, m.totalInodesKey())
			if len(srcXattr) > 0 {
				p.HMSet(ctx, m.xattrKey(ino), srcXattr)
			}
			if top && attr.Typ == TypeDirectory {
				p.ZAdd(ctx, m.detachedNodes(), rueidiscompat.Z{Member: ino.String(), Score: float64(time.Now().Unix())})
			} else {
				p.HSet(ctx, m.entryKey(parent), name, m.packEntry(attr.Typ, ino))
				if top {
					now := time.Now()
					pattr.Mtime = now.Unix()
					pattr.Mtimensec = uint32(now.Nanosecond())
					pattr.Ctime = now.Unix()
					pattr.Ctimensec = uint32(now.Nanosecond())
					p.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
				}
			}

			switch attr.Typ {
			case TypeDirectory:
				sfield := srcIno.String()
				field := ino.String()
				if v, err := tx.HGet(ctx, m.dirUsedInodesKey(), sfield).Result(); err == nil {
					p.HSet(ctx, m.dirUsedInodesKey(), field, v)
					p.HSet(ctx, m.dirDataLengthKey(), field, tx.HGet(ctx, m.dirDataLengthKey(), sfield).Val())
					p.HSet(ctx, m.dirUsedSpaceKey(), field, tx.HGet(ctx, m.dirUsedSpaceKey(), sfield).Val())
				}
			case TypeFile:
				// copy chunks
				if attr.Length != 0 {
					var vals [][]string
					for i := 0; i <= int(attr.Length/ChunkSize); i++ {
						val, err := tx.LRange(ctx, m.chunkKey(srcIno, uint32(i)), 0, -1).Result()
						if err != nil {
							return err
						}
						vals = append(vals, val)
					}

					for i, sv := range vals {
						if len(sv) == 0 {
							continue
						}
						ss := readSlices(sv)
						if ss == nil {
							return syscall.EIO
						}
						p.RPush(ctx, m.chunkKey(ino, uint32(i)), sv)
						for _, s := range ss {
							if s.id > 0 {
								p.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.id, s.size), 1)
							}
						}
					}
				}
			case TypeSymlink:
				path, err := tx.Get(ctx, m.symKey(srcIno)).Result()
				if err != nil {
					return err
				}
				p.Set(ctx, m.symKey(ino), path, 0)
			}
			return nil
		})
		return err
	}, m.inodeKey(srcIno), m.xattrKey(srcIno)))
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

	err = m.txn(ctx, func(tx rueidiscompat.Tx) error {
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
	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
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
	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
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

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
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

func (m *rueidisMeta) getParentsCompat(ctx Context, tx rueidiscompat.Tx, inode, parent Ino) []Ino {
	if parent > 0 {
		return []Ino{parent}
	}
	vals, err := tx.HGetAll(ctx, m.parentKey(inode)).Result()
	if err != nil {
		logger.Warnf("Scan parent key of inode %d: %v", inode, err)
		return nil
	}
	ps := make([]Ino, 0, len(vals))
	for k, v := range vals {
		if n, _ := strconv.Atoi(v); n > 0 {
			ino, _ := strconv.ParseUint(k, 10, 64)
			ps = append(ps, Ino(ino))
		}
	}
	return ps
}

func (m *rueidisMeta) doGetParents(ctx Context, inode Ino) map[Ino]int {
	if m.compat == nil {
		return m.redisMeta.doGetParents(ctx, inode)
	}

	vals, err := m.compat.HGetAll(ctx, m.parentKey(inode)).Result()
	if err != nil {
		logger.Warnf("Scan parent key of inode %d: %v", inode, err)
		return nil
	}
	ps := make(map[Ino]int, len(vals))
	for k, v := range vals {
		if n, _ := strconv.Atoi(v); n > 0 {
			ino, _ := strconv.ParseUint(k, 10, 64)
			ps[Ino(ino)] = n
		}
	}
	return ps
}

func (m *rueidisMeta) doSyncDirStat(ctx Context, ino Ino) (*dirStat, syscall.Errno) {
	if m.compat == nil {
		return m.redisMeta.doSyncDirStat(ctx, ino)
	}
	if m.conf.ReadOnly {
		return nil, syscall.EROFS
	}

	field := ino.String()
	stat, st := m.calcDirStat(ctx, ino)
	if st != 0 {
		return nil, st
	}

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		n, err := tx.Exists(ctx, m.inodeKey(ino)).Result()
		if err != nil {
			return err
		}
		if n <= 0 {
			return syscall.ENOENT
		}
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.HSet(ctx, m.dirDataLengthKey(), field, stat.length)
			pipe.HSet(ctx, m.dirUsedSpaceKey(), field, stat.space)
			pipe.HSet(ctx, m.dirUsedInodesKey(), field, stat.inodes)
			return nil
		})
		return err
	}, m.inodeKey(ino))

	return stat, errno(err)
}

func (m *rueidisMeta) doUpdateDirStat(ctx Context, batch map[Ino]dirStat) error {
	if m.compat == nil {
		return m.redisMeta.doUpdateDirStat(ctx, batch)
	}

	spaceKey := m.dirUsedSpaceKey()
	lengthKey := m.dirDataLengthKey()
	inodesKey := m.dirUsedInodesKey()
	nonexist := make(map[Ino]bool)
	statList := make([]Ino, 0, len(batch))
	pipe := m.compat.Pipeline()
	for ino := range batch {
		pipe.HExists(ctx, spaceKey, ino.String())
		statList = append(statList, ino)
	}
	cmds, err := pipe.Exec(ctx)
	if err != nil {
		return err
	}
	for i, cmd := range cmds {
		if cmd.Err() != nil {
			return cmd.Err()
		}
		boolCmd, ok := cmd.(*rueidiscompat.BoolCmd)
		if !ok {
			return fmt.Errorf("unexpected pipeline result type %T", cmd)
		}
		if exist, _ := boolCmd.Result(); !exist {
			nonexist[statList[i]] = true
		}
	}
	if len(nonexist) > 0 {
		wg := m.parallelSyncDirStat(ctx, nonexist)
		defer wg.Wait()
	}

	for _, group := range m.groupBatch(batch, 1000) {
		_, err := m.compat.Pipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			for _, ino := range group {
				if nonexist[ino] {
					continue
				}
				field := ino.String()
				stat := batch[ino]
				if stat.length != 0 {
					pipe.HIncrBy(ctx, lengthKey, field, stat.length)
				}
				if stat.space != 0 {
					pipe.HIncrBy(ctx, spaceKey, field, stat.space)
				}
				if stat.inodes != 0 {
					pipe.HIncrBy(ctx, inodesKey, field, stat.inodes)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (m *rueidisMeta) doGetDirStat(ctx Context, ino Ino, trySync bool) (*dirStat, syscall.Errno) {
	if m.compat == nil {
		return m.redisMeta.doGetDirStat(ctx, ino, trySync)
	}

	field := ino.String()
	dataLength, errLength := m.compat.HGet(ctx, m.dirDataLengthKey(), field).Int64()
	if errLength != nil && errLength != rueidiscompat.Nil {
		return nil, errno(errLength)
	}
	usedSpace, errSpace := m.compat.HGet(ctx, m.dirUsedSpaceKey(), field).Int64()
	if errSpace != nil && errSpace != rueidiscompat.Nil {
		return nil, errno(errSpace)
	}
	usedInodes, errInodes := m.compat.HGet(ctx, m.dirUsedInodesKey(), field).Int64()
	if errInodes != nil && errInodes != rueidiscompat.Nil {
		return nil, errno(errInodes)
	}

	if errLength != rueidiscompat.Nil && errSpace != rueidiscompat.Nil && errInodes != rueidiscompat.Nil {
		if trySync && (dataLength < 0 || usedSpace < 0 || usedInodes < 0) {
			return m.doSyncDirStat(ctx, ino)
		}
		return &dirStat{dataLength, usedSpace, usedInodes}, 0
	}

	if trySync {
		return m.doSyncDirStat(ctx, ino)
	}
	return nil, 0
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

	return m.txn(ctx, func(tx rueidiscompat.Tx) error {
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

				if err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
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

func (m *rueidisMeta) deleteChunk(inode Ino, indx uint32) error {
	if m.compat == nil {
		return m.redisMeta.deleteChunk(inode, indx)
	}

	ctx := Background()
	key := m.chunkKey(inode, indx)
	var (
		todel []*slice
		rs    []*rueidiscompat.IntCmd
	)

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		todel = todel[:0]
		rs = rs[:0]
		vals, err := tx.LRange(ctx, key, 0, -1).Result()
		if err != nil || len(vals) == 0 {
			return err
		}
		slices := readSlices(vals)
		if slices == nil {
			logger.Errorf("Corrupt value for inode %d chunk index %d, use `gc` to clean up leaked slices", inode, indx)
		}
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Del(ctx, key)
			for _, s := range slices {
				if s.id > 0 {
					todel = append(todel, s)
					rs = append(rs, pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.id, s.size), -1))
				}
			}
			return nil
		})
		return err
	}, key)
	if err != nil {
		return fmt.Errorf("delete slice from chunk %s fail: %v, retry later", key, err)
	}
	for i, s := range todel {
		if rs[i].Err() == nil && rs[i].Val() < 0 {
			m.deleteSlice(s.id, s.size)
		}
	}
	return nil
}

func (m *rueidisMeta) doDeleteFileData(inode Ino, length uint64) {
	if m.compat == nil {
		m.redisMeta.doDeleteFileData(inode, length)
		return
	}
	m.doDeleteFileData_(inode, length, "")
}

func (m *rueidisMeta) doDeleteFileData_(inode Ino, length uint64, tracking string) {
	if m.compat == nil {
		m.redisMeta.doDeleteFileData_(inode, length, tracking)
		return
	}

	ctx := Background()
	var indx uint32
	for uint64(indx)*ChunkSize < length {
		keys := make([]string, 0, 1000)
		pipe := m.compat.Pipeline()
		for i := 0; uint64(indx)*ChunkSize < length && i < 1000; i++ {
			key := m.chunkKey(inode, indx)
			keys = append(keys, key)
			pipe.LLen(ctx, key)
			indx++
		}
		cmds, err := pipe.Exec(ctx)
		if err != nil {
			logger.Warnf("delete chunks of inode %d: %v", inode, err)
			return
		}
		for i, cmd := range cmds {
			llenCmd, ok := cmd.(*rueidiscompat.IntCmd)
			if !ok {
				logger.Warnf("unexpected pipeline result type %T for chunk %s", cmd, keys[i])
				continue
			}
			val, err := llenCmd.Result()
			if err == rueidiscompat.Nil || val == 0 {
				continue
			}
			parts := strings.Split(keys[i][len(m.prefix):], "_")
			if len(parts) < 2 {
				logger.Warnf("invalid chunk key format: %s", keys[i])
				continue
			}
			idx, _ := strconv.Atoi(parts[1])
			if e := m.deleteChunk(inode, uint32(idx)); e != nil {
				logger.Warnf("delete chunk %s: %v", keys[i], e)
				return
			}
		}
	}
	if tracking == "" {
		tracking = inode.String() + ":" + strconv.FormatInt(int64(length), 10)
	}
	if err := m.compat.ZRem(ctx, m.delfiles(), tracking).Err(); err != nil && err != rueidiscompat.Nil {
		logger.Warnf("ZRem %s %s: %v", m.delfiles(), tracking, err)
	}
}

func (m *rueidisMeta) doCompactChunk(inode Ino, indx uint32, origin []byte, ss []*slice, skipped int, pos uint32, id uint64, size uint32, delayed []byte) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doCompactChunk(inode, indx, origin, ss, skipped, pos, id, size, delayed)
	}

	var rs []*rueidiscompat.IntCmd
	if delayed == nil {
		rs = make([]*rueidiscompat.IntCmd, len(ss))
	}
	key := m.chunkKey(inode, indx)
	ctx := Background()
	st := errno(m.txn(ctx, func(tx rueidiscompat.Tx) error {
		n := len(origin) / sliceBytes
		vals2, err := tx.LRange(ctx, key, 0, int64(n-1)).Result()
		if err != nil {
			return err
		}
		if len(vals2) != n {
			return syscall.EINVAL
		}
		for i, val := range vals2 {
			if val != string(origin[i*sliceBytes:(i+1)*sliceBytes]) {
				return syscall.EINVAL
			}
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.LTrim(ctx, key, int64(n), -1)
			pipe.LPush(ctx, key, string(marshalSlice(pos, id, size, 0, size)))
			for i := skipped; i > 0; i-- {
				pipe.LPush(ctx, key, string(origin[(i-1)*sliceBytes:i*sliceBytes]))
			}
			pipe.HSet(ctx, m.sliceRefs(), m.sliceKey(id, size), "0")
			if delayed != nil {
				if len(delayed) > 0 {
					pipe.HSet(ctx, m.delSlices(), fmt.Sprintf("%d_%d", id, time.Now().Unix()), string(delayed))
				}
			} else {
				for i, s := range ss {
					if s.id > 0 {
						rs[i] = pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.id, s.size), -1)
					}
				}
			}
			return nil
		})
		return err
	}, key))

	if st != 0 && st != syscall.EINVAL {
		if err := m.compat.HGet(ctx, m.sliceRefs(), m.sliceKey(id, size)).Err(); err == nil {
			st = 0
		} else if err == rueidiscompat.Nil {
			logger.Infof("compacted chunk %d was not used", id)
			st = syscall.EINVAL
		}
	}

	if st == syscall.EINVAL {
		m.compat.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(id, size), -1)
	} else if st == 0 {
		m.cleanupZeroRef(m.sliceKey(id, size))
		if delayed == nil {
			for i, s := range ss {
				if s.id > 0 && rs[i] != nil && rs[i].Err() == nil && rs[i].Val() < 0 {
					m.deleteSlice(s.id, s.size)
				}
			}
		}
	}
	return st
}

func (m *rueidisMeta) scanAllChunks(ctx Context, ch chan<- cchunk, bar *utils.Bar) error {
	if m.compat == nil {
		return m.redisMeta.scanAllChunks(ctx, ch, bar)
	}

	pattern := m.prefix + "c*_*"
	var cursor uint64
	for {
		keys, next, err := m.compat.Scan(ctx, cursor, pattern, 10000).Result()
		if err != nil {
			logger.Warnf("scan %s: %v", pattern, err)
			return err
		}
		if len(keys) > 0 {
			pipe := m.compat.Pipeline()
			cmds := make([]*rueidiscompat.IntCmd, len(keys))
			for i, key := range keys {
				cmds[i] = pipe.LLen(ctx, key)
			}
			execCmds, execErr := pipe.Exec(ctx)
			if execErr != nil {
				for _, c := range execCmds {
					if err := c.Err(); err != nil {
						logger.Warnf("scan chunks pipeline error: %v", err)
					}
				}
				return execErr
			}
			for i, cmd := range cmds {
				cnt := cmd.Val()
				if cnt <= 1 {
					continue
				}
				var inode uint64
				var chunkIdx uint32
				if _, err := fmt.Sscanf(keys[i], m.prefix+"c%d_%d", &inode, &chunkIdx); err == nil {
					bar.IncrTotal(1)
					ch <- cchunk{Ino(inode), chunkIdx, int(cnt)}
				}
			}
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	return nil
}

func (m *rueidisMeta) doRepair(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doRepair(ctx, inode, attr)
	}

	return errno(m.txn(ctx, func(tx rueidiscompat.Tx) error {
		attr.Nlink = 2
		vals, err := tx.HGetAll(ctx, m.entryKey(inode)).Result()
		if err != nil {
			return err
		}
		for _, v := range vals {
			typ, _ := m.parseEntry([]byte(v))
			if typ == TypeDirectory {
				attr.Nlink++
			}
		}
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), string(m.marshal(attr)), 0)
			return nil
		})
		return err
	}, m.inodeKey(inode), m.entryKey(inode)))
}

func (m *rueidisMeta) GetXattr(ctx Context, inode Ino, name string, vbuff *[]byte) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.GetXattr(ctx, inode, name, vbuff)
	}

	defer m.timeit("GetXattr", time.Now())
	inode = m.checkRoot(inode)
	val, err := m.compat.HGet(ctx, m.xattrKey(inode), name).Bytes()
	if err == rueidiscompat.Nil {
		return ENOATTR
	}
	if err != nil {
		return errno(err)
	}
	*vbuff = append((*vbuff)[:0], val...)
	return 0
}

func (m *rueidisMeta) ListXattr(ctx Context, inode Ino, names *[]byte) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.ListXattr(ctx, inode, names)
	}

	defer m.timeit("ListXattr", time.Now())
	inode = m.checkRoot(inode)
	vals, err := m.compat.HKeys(ctx, m.xattrKey(inode)).Result()
	if err != nil {
		return errno(err)
	}
	*names = (*names)[:0]
	for _, name := range vals {
		*names = append(*names, name...)
		*names = append(*names, 0)
	}

	data, err := m.compat.Get(ctx, m.inodeKey(inode)).Bytes()
	if err != nil {
		return errno(err)
	}
	attr := &Attr{}
	m.parseAttr(data, attr)
	setXAttrACL(names, attr.AccessACL, attr.DefaultACL)
	return 0
}

func (m *rueidisMeta) doSetXattr(ctx Context, inode Ino, name string, value []byte, flags uint32) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doSetXattr(ctx, inode, name, value, flags)
	}

	key := m.xattrKey(inode)
	return errno(m.txn(ctx, func(tx rueidiscompat.Tx) error {
		switch flags {
		case XattrCreate:
			ok, err := tx.HSetNX(ctx, key, name, value).Result()
			if err != nil {
				return err
			}
			if !ok {
				return syscall.EEXIST
			}
			return nil
		case XattrReplace:
			exists, err := tx.HExists(ctx, key, name).Result()
			if err != nil {
				return err
			}
			if !exists {
				return ENOATTR
			}
			_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
				pipe.HSet(ctx, key, name, value)
				return nil
			})
			return err
		default:
			_, err := tx.HSet(ctx, key, name, value).Result()
			return err
		}
	}, key))
}

func (m *rueidisMeta) doRemoveXattr(ctx Context, inode Ino, name string) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doRemoveXattr(ctx, inode, name)
	}

	n, err := m.compat.HDel(ctx, m.xattrKey(inode), name).Result()
	if err != nil {
		return errno(err)
	}
	if n == 0 {
		return ENOATTR
	}
	return 0
}

func (m *rueidisMeta) doGetQuota(ctx Context, qtype uint32, key uint64) (*Quota, error) {
	if m.compat == nil {
		return m.redisMeta.doGetQuota(ctx, qtype, key)
	}

	config, err := m.redisMeta.getQuotaKeys(qtype)
	if err != nil {
		return nil, err
	}

	field := strconv.FormatUint(key, 10)
	var (
		quotaCmd      *rueidiscompat.StringCmd
		usedSpaceCmd  *rueidiscompat.StringCmd
		usedInodesCmd *rueidiscompat.StringCmd
	)
	_, err = m.compat.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
		quotaCmd = pipe.HGet(ctx, config.quotaKey, field)
		usedSpaceCmd = pipe.HGet(ctx, config.usedSpaceKey, field)
		usedInodesCmd = pipe.HGet(ctx, config.usedInodesKey, field)
		return nil
	})
	if err == rueidiscompat.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	buf, err := quotaCmd.Bytes()
	if err == rueidiscompat.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(buf) != 16 {
		return nil, fmt.Errorf("invalid quota value: %v", buf)
	}

	var quota Quota
	quota.MaxSpace, quota.MaxInodes = m.parseQuota(buf)
	if quota.UsedSpace, err = usedSpaceCmd.Int64(); err != nil {
		return nil, err
	}
	if quota.UsedInodes, err = usedInodesCmd.Int64(); err != nil {
		return nil, err
	}
	return &quota, nil
}

func (m *rueidisMeta) doSetQuota(ctx Context, qtype uint32, key uint64, quota *Quota) (bool, error) {
	if m.compat == nil {
		return m.redisMeta.doSetQuota(ctx, qtype, key, quota)
	}

	config, err := m.redisMeta.getQuotaKeys(qtype)
	if err != nil {
		return false, err
	}

	var created bool
	field := strconv.FormatUint(key, 10)
	err = m.txn(ctx, func(tx rueidiscompat.Tx) error {
		origin := &Quota{MaxSpace: -1, MaxInodes: -1}
		buf, e := tx.HGet(ctx, config.quotaKey, field).Bytes()
		if e == nil {
			created = false
			origin.MaxSpace, origin.MaxInodes = m.parseQuota(buf)
		} else if e == rueidiscompat.Nil {
			created = true
		} else {
			return e
		}

		if quota.MaxSpace >= 0 {
			origin.MaxSpace = quota.MaxSpace
		}
		if quota.MaxInodes >= 0 {
			origin.MaxInodes = quota.MaxInodes
		}

		_, e = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.HSet(ctx, config.quotaKey, field, m.packQuota(origin.MaxSpace, origin.MaxInodes))
			if quota.UsedSpace >= 0 {
				pipe.HSet(ctx, config.usedSpaceKey, field, quota.UsedSpace)
			} else if created {
				pipe.HSet(ctx, config.usedSpaceKey, field, 0)
			}
			if quota.UsedInodes >= 0 {
				pipe.HSet(ctx, config.usedInodesKey, field, quota.UsedInodes)
			} else if created {
				pipe.HSet(ctx, config.usedInodesKey, field, 0)
			}
			return nil
		})
		return e
	}, m.inodeKey(Ino(key)))
	return created, err
}

func (m *rueidisMeta) doDelQuota(ctx Context, qtype uint32, key uint64) error {
	if m.compat == nil {
		return m.redisMeta.doDelQuota(ctx, qtype, key)
	}

	config, err := m.redisMeta.getQuotaKeys(qtype)
	if err != nil {
		return err
	}

	field := strconv.FormatUint(key, 10)
	_, err = m.compat.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
		if qtype == UserQuotaType || qtype == GroupQuotaType {
			pipe.HSet(ctx, config.quotaKey, field, m.packQuota(-1, -1))
		} else {
			pipe.HDel(ctx, config.quotaKey, field)
			pipe.HDel(ctx, config.usedSpaceKey, field)
			pipe.HDel(ctx, config.usedInodesKey, field)
		}
		return nil
	})
	return err
}

func (m *rueidisMeta) doLoadQuotas(ctx Context) (map[uint64]*Quota, map[uint64]*Quota, map[uint64]*Quota, error) {
	if m.compat == nil {
		return m.redisMeta.doLoadQuotas(ctx)
	}

	quotaTypes := []struct {
		qtype uint32
		name  string
	}{
		{DirQuotaType, "dir"},
		{UserQuotaType, "user"},
		{GroupQuotaType, "group"},
	}

	quotaMaps := make([]map[uint64]*Quota, 3)
	for i, qt := range quotaTypes {
		config, err := m.redisMeta.getQuotaKeys(qt.qtype)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to load %s quotas: %w", qt.name, err)
		}

		quotas := make(map[uint64]*Quota)
		var cursor uint64
		for {
			kvs, next, err := m.compat.HScan(ctx, config.quotaKey, cursor, "*", 10000).Result()
			if err != nil {
				return nil, nil, nil, err
			}
			for j := 0; j < len(kvs); j += 2 {
				keyStr := kvs[j]
				val := []byte(kvs[j+1])
				id, err := strconv.ParseUint(keyStr, 10, 64)
				if err != nil {
					logger.Errorf("invalid inode: %s", keyStr)
					continue
				}
				if len(val) != 16 {
					logger.Errorf("invalid quota: %s=%s", keyStr, val)
					continue
				}

				maxSpace, maxInodes := m.parseQuota(val)
				usedSpace, err := m.compat.HGet(ctx, config.usedSpaceKey, keyStr).Int64()
				if err != nil && err != rueidiscompat.Nil {
					return nil, nil, nil, err
				}
				if err == rueidiscompat.Nil {
					usedSpace = 0
				}
				usedInodes, err := m.compat.HGet(ctx, config.usedInodesKey, keyStr).Int64()
				if err != nil && err != rueidiscompat.Nil {
					return nil, nil, nil, err
				}
				if err == rueidiscompat.Nil {
					usedInodes = 0
				}

				quotas[id] = &Quota{
					MaxSpace:   int64(maxSpace),
					MaxInodes:  int64(maxInodes),
					UsedSpace:  usedSpace,
					UsedInodes: usedInodes,
				}
			}
			if next == 0 {
				break
			}
			cursor = next
		}
		quotaMaps[i] = quotas
	}

	return quotaMaps[0], quotaMaps[1], quotaMaps[2], nil
}

func (m *rueidisMeta) doFlushQuotas(ctx Context, quotas []*iQuota) error {
	if m.compat == nil {
		return m.redisMeta.doFlushQuotas(ctx, quotas)
	}

	_, err := m.compat.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
		for _, q := range quotas {
			config, err := m.redisMeta.getQuotaKeys(q.qtype)
			if err != nil {
				return err
			}

			field := strconv.FormatUint(q.qkey, 10)
			pipe.HIncrBy(ctx, config.usedSpaceKey, field, q.quota.newSpace)
			pipe.HIncrBy(ctx, config.usedInodesKey, field, q.quota.newInodes)
		}
		return nil
	})
	return err
}

func (m *rueidisMeta) doDeleteSustainedInode(sid uint64, inode Ino) error {
	if m.compat == nil {
		return m.redisMeta.doDeleteSustainedInode(sid, inode)
	}

	var (
		attr     Attr
		newSpace int64
	)
	ctx := Background()
	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		newSpace = 0
		cmd := tx.Get(ctx, m.inodeKey(inode))
		data, err := cmd.Bytes()
		if err == rueidiscompat.Nil {
			return nil
		}
		if err != nil {
			return err
		}
		m.parseAttr(data, &attr)
		newSpace = -align4K(attr.Length)
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.ZAdd(ctx, m.delfiles(), rueidiscompat.Z{Score: float64(time.Now().Unix()), Member: m.toDelete(inode, attr.Length)})
			pipe.Del(ctx, m.inodeKey(inode))
			pipe.IncrBy(ctx, m.usedSpaceKey(), newSpace)
			pipe.Decr(ctx, m.totalInodesKey())
			pipe.SRem(ctx, m.sustained(sid), strconv.Itoa(int(inode)))
			return nil
		})
		return err
	}, m.inodeKey(inode))
	if err == nil && newSpace < 0 {
		m.updateStats(newSpace, -1)
		m.tryDeleteFileData(inode, attr.Length, false)
	}
	return err
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

func (m *rueidisMeta) fillAttr(ctx Context, es []*Entry) error {
	if m.compat == nil {
		return m.redisMeta.fillAttr(ctx, es)
	}
	if len(es) == 0 {
		return nil
	}
	keys := make([]string, len(es))
	for i, e := range es {
		keys[i] = m.inodeKey(e.Inode)
	}

	var vals []interface{}
	var err error

	// Use native Rueidis MGetCache if caching is enabled
	if m.cacheTTL > 0 {
		cachedVals, cacheErr := rueidis.MGetCache(m.client, ctx, m.cacheTTL, keys)
		if cacheErr != nil {
			return cacheErr
		}
		// Convert map to slice in key order
		vals = make([]interface{}, len(keys))
		for i, key := range keys {
			if result, ok := cachedVals[key]; ok {
				if s, err := result.ToString(); err == nil {
					vals[i] = s
				} else if !rueidis.IsRedisNil(err) {
					return err
				}
			}
		}
	} else {
		vals, err = m.compat.MGet(ctx, keys...).Result()
		if err != nil {
			return err
		}
	}

	for j, v := range vals {
		if v == nil {
			continue
		}
		var data []byte
		switch vv := v.(type) {
		case string:
			data = []byte(vv)
		case []byte:
			data = vv
		default:
			logger.Warnf("unexpected value type %T for inode %s", v, keys[j])
			continue
		}
		m.parseAttr(data, es[j].Attr)
		m.of.Update(es[j].Inode, es[j].Attr)
	}
	return nil
}

func (m *rueidisMeta) doReaddir(ctx Context, inode Ino, plus uint8, entries *[]*Entry, limit int) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doReaddir(ctx, inode, plus, entries, limit)
	}

	var (
		cursor  uint64
		reached bool
		key     = m.entryKey(inode)
	)

	for {
		kvs, next, err := m.compat.HScan(ctx, key, cursor, "*", 10000).Result()
		if err != nil {
			return errno(err)
		}
		if len(kvs) > 0 {
			newEntries := make([]Entry, len(kvs)/2)
			newAttrs := make([]Attr, len(kvs)/2)
			for i := 0; i < len(kvs); i += 2 {
				name := kvs[i]
				typ, ino := m.parseEntry([]byte(kvs[i+1]))
				if name == "" {
					logger.Errorf("Corrupt entry with empty name: inode %d parent %d", ino, inode)
					continue
				}
				ent := &newEntries[i/2]
				ent.Inode = ino
				ent.Name = []byte(name)
				ent.Attr = &newAttrs[i/2]
				ent.Attr.Typ = typ
				*entries = append(*entries, ent)
				if limit > 0 && len(*entries) >= limit {
					reached = true
					break
				}
			}
		}
		if reached || next == 0 {
			break
		}
		cursor = next
	}

	if plus != 0 && len(*entries) != 0 {
		const batchSize = 4096
		nEntries := len(*entries)
		if nEntries <= batchSize {
			if err := m.fillAttr(ctx, *entries); err != nil {
				return errno(err)
			}
		} else {
			var eg errgroup.Group
			eg.SetLimit(2)
			for i := 0; i < nEntries; i += batchSize {
				start := i
				end := i + batchSize
				if end > nEntries {
					end = nEntries
				}
				es := (*entries)[start:end]
				eg.Go(func() error {
					return m.fillAttr(ctx, es)
				})
			}
			if err := eg.Wait(); err != nil {
				return errno(err)
			}
		}
	}
	return 0
}

func (m *rueidisMeta) doLookup(ctx Context, parent Ino, name string, inode *Ino, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doLookup(ctx, parent, name, inode, attr)
	}

	entryKey := m.entryKey(parent)
	var (
		foundIno    Ino
		foundType   uint8
		encodedAttr []byte
		err         error
	)

	if len(m.shaLookup) > 0 && attr != nil && !m.conf.CaseInsensi && m.prefix == "" {
		cmd := m.compat.EvalSha(ctx, m.shaLookup, []string{entryKey, name})
		res, evalErr := cmd.Result()
		var (
			returnedIno  int64
			returnedAttr string
		)
		if st := m.handleLuaResult("lookup", res, evalErr, &returnedIno, &returnedAttr); st == 0 {
			foundIno = Ino(returnedIno)
			encodedAttr = []byte(returnedAttr)
		} else if st == syscall.EAGAIN {
			return m.doLookup(ctx, parent, name, inode, attr)
		} else if st != syscall.ENOTSUP {
			return st
		}
	}

	if foundIno == 0 || len(encodedAttr) == 0 {
		buf, e := m.compat.HGet(ctx, entryKey, name).Bytes()
		if e != nil {
			return errno(e)
		}
		foundType, foundIno = m.parseEntry(buf)

		// Use client-side caching for inode attribute reads if cacheTTL > 0
		if m.cacheTTL > 0 {
			encodedAttr, err = m.compat.Cache(m.cacheTTL).Get(ctx, m.inodeKey(foundIno)).Bytes()
		} else {
			encodedAttr, err = m.compat.Get(ctx, m.inodeKey(foundIno)).Bytes()
		}
	} else {
		err = nil
	}

	if err == nil {
		if attr != nil && len(encodedAttr) > 0 {
			m.parseAttr(encodedAttr, attr)
			m.of.Update(foundIno, attr)
		}
	} else if err == rueidiscompat.Nil {
		if attr != nil {
			logger.Warnf("no attribute for inode %d (%d, %s)", foundIno, parent, name)
			*attr = Attr{Typ: foundType}
		}
		err = nil
	}

	if inode != nil {
		*inode = foundIno
	}
	return errno(err)
}

func (m *rueidisMeta) Resolve(ctx Context, parent Ino, path string, inode *Ino, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.Resolve(ctx, parent, path, inode, attr)
	}

	if len(m.shaResolve) == 0 || m.conf.CaseInsensi || m.prefix != "" {
		return syscall.ENOTSUP
	}

	defer m.timeit("Resolve", time.Now())
	parent = m.checkRoot(parent)
	keys := []string{parent.String(), path, strconv.FormatUint(uint64(ctx.Uid()), 10)}
	var args []interface{}
	for _, gid := range ctx.Gids() {
		args = append(args, strconv.FormatUint(uint64(gid), 10))
	}

	cmd := m.compat.EvalSha(ctx, m.shaResolve, keys, args...)
	res, err := cmd.Result()
	var (
		returnedIno  int64
		returnedAttr string
	)
	st := m.handleLuaResult("resolve", res, err, &returnedIno, &returnedAttr)
	if st == 0 {
		if inode != nil {
			*inode = Ino(returnedIno)
		}
		m.parseAttr([]byte(returnedAttr), attr)
	} else if st == syscall.EAGAIN {
		return m.Resolve(ctx, parent, path, inode, attr)
	}
	return st
}

func (m *rueidisMeta) doGetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doGetAttr(ctx, inode, attr)
	}

	// Use client-side caching for inode attribute reads if cacheTTL > 0
	var data []byte
	var err error
	if m.cacheTTL > 0 {
		data, err = m.compat.Cache(m.cacheTTL).Get(ctx, m.inodeKey(inode)).Bytes()
	} else {
		data, err = m.compat.Get(ctx, m.inodeKey(inode)).Bytes()
	}

	if err != nil {
		return errno(err)
	}
	if attr != nil {
		m.parseAttr(data, attr)
	}
	return 0
}

func (m *rueidisMeta) doSetAttr(ctx Context, inode Ino, set uint16, sugidclearmode uint8, attr *Attr, oldAttr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doSetAttr(ctx, inode, set, sugidclearmode, attr, oldAttr)
	}

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		var cur Attr
		val, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(val, &cur)
		if oldAttr != nil {
			*oldAttr = cur
		}
		if cur.Parent > TrashInode {
			return syscall.EPERM
		}

		now := time.Now()
		rule, err := m.getACLCompat(ctx, tx, cur.AccessACL)
		if err != nil {
			return err
		}
		rule = rule.Dup()
		dirtyAttr, st := m.mergeAttr(ctx, inode, set, &cur, attr, now, rule)
		if st != 0 {
			return st
		}
		if dirtyAttr == nil {
			return nil
		}

		aclId, err := m.insertACLCompat(ctx, tx, rule)
		if err != nil {
			return err
		}
		dirtyAttr.AccessACL = aclId
		dirtyAttr.Ctime = now.Unix()
		dirtyAttr.Ctimensec = uint32(now.Nanosecond())

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(dirtyAttr), 0)
			return nil
		})
		if err == nil {
			*attr = *dirtyAttr
		}
		return err
	}, m.inodeKey(inode))

	return errno(err)
}

func (m *rueidisMeta) doReadlink(ctx Context, inode Ino, noatime bool) (int64, []byte, error) {
	if m.compat == nil {
		return m.redisMeta.doReadlink(ctx, inode, noatime)
	}

	if noatime {
		target, err := m.compat.Get(ctx, m.symKey(inode)).Bytes()
		if err == rueidiscompat.Nil {
			err = nil
		}
		return 0, target, err
	}

	var (
		atime  int64
		target []byte
	)
	now := time.Now()
	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		rs, err := tx.MGet(ctx, m.inodeKey(inode), m.symKey(inode)).Result()
		if err != nil {
			return err
		}
		if len(rs) != 2 || rs[0] == nil {
			return syscall.ENOENT
		}

		var attr Attr
		switch v := rs[0].(type) {
		case string:
			m.parseAttr([]byte(v), &attr)
		case []byte:
			m.parseAttr(v, &attr)
		default:
			return fmt.Errorf("unexpected attr value type %T", rs[0])
		}
		if attr.Typ != TypeSymlink {
			return syscall.EINVAL
		}
		if rs[1] == nil {
			return syscall.EIO
		}
		switch v := rs[1].(type) {
		case string:
			target = []byte(v)
		case []byte:
			target = v
		default:
			return fmt.Errorf("unexpected link value type %T", rs[1])
		}

		if !m.atimeNeedsUpdate(&attr, now) {
			atime = attr.Atime*int64(time.Second) + int64(attr.Atimensec)
			return nil
		}

		attr.Atime = now.Unix()
		attr.Atimensec = uint32(now.Nanosecond())
		atime = now.UnixNano()
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(&attr), 0)
			return nil
		})
		return err
	}, m.inodeKey(inode), m.symKey(inode))

	return atime, target, err
}

func (m *rueidisMeta) doMknod(ctx Context, parent Ino, name string, _type uint8, mode, cumask uint16, path string, inode *Ino, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doMknod(ctx, parent, name, _type, mode, cumask, path, inode, attr)
	}

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		var pattr Attr
		parentKey := m.inodeKey(parent)
		data, err := tx.Get(ctx, parentKey).Bytes()
		if err != nil {
			// In rueidiscompat, we must explicitly check for Nil within transactions
			// Unlike go-redis where errno() handles it, transactions need explicit handling
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
		if st := m.Access(ctx, parent, MODE_MASK_W|MODE_MASK_X, &pattr); st != 0 {
			return st
		}
		if (pattr.Flags & FlagImmutable) != 0 {
			return syscall.EPERM
		}
		if (pattr.Flags & FlagSkipTrash) != 0 {
			attr.Flags |= FlagSkipTrash
		}

		entryKey := m.entryKey(parent)
		bufCmd := tx.HGet(ctx, entryKey, name)
		buf, err := bufCmd.Bytes()
		if err != nil && err != rueidiscompat.Nil {
			return err
		}
		var (
			foundIno  Ino
			foundType uint8
		)
		if err == nil {
			foundType, foundIno = m.parseEntry(buf)
		} else if m.conf.CaseInsensi {
			if entry := m.resolveCase(ctx, parent, name); entry != nil {
				foundType, foundIno = entry.Attr.Typ, entry.Inode
			}
		}
		if foundIno != 0 {
			if _type == TypeFile || _type == TypeDirectory {
				val, e := tx.Get(ctx, m.inodeKey(foundIno)).Bytes()
				if e == nil {
					m.parseAttr(val, attr)
				} else if e == rueidiscompat.Nil {
					*attr = Attr{Typ: foundType, Parent: parent}
					e = nil
				} else {
					return e
				}
				if inode != nil {
					*inode = foundIno
				}
			}
			return syscall.EEXIST
		} else if parent == TrashInode {
			next, e := tx.Incr(ctx, m.nextTrashKey()).Result()
			if e != nil {
				return e
			}
			if inode != nil {
				*inode = TrashInode + Ino(next)
			}
		}

		mode &= 07777
		if pattr.DefaultACL != aclAPI.None && _type != TypeSymlink {
			if _type == TypeDirectory {
				attr.DefaultACL = pattr.DefaultACL
			}
			rule, e := m.getACLCompat(ctx, tx, pattr.DefaultACL)
			if e != nil {
				return e
			}
			if rule.IsMinimal() { // simple ACL
				attr.Mode = mode & (0xFE00 | rule.GetMode())
			} else {
				cRule := rule.ChildAccessACL(mode)
				id, e := m.insertACLCompat(ctx, tx, cRule)
				if e != nil {
					return e
				}
				attr.AccessACL = id
				attr.Mode = (mode & 0xFE00) | cRule.GetMode()
			}
		} else {
			attr.Mode = mode & ^cumask
		}

		var updateParent bool
		now := time.Now()
		if parent != TrashInode {
			if _type == TypeDirectory {
				pattr.Nlink++
				updateParent = true
			}
			if updateParent || now.Sub(time.Unix(pattr.Mtime, int64(pattr.Mtimensec))) >= m.conf.SkipDirMtime {
				pattr.Mtime = now.Unix()
				pattr.Mtimensec = uint32(now.Nanosecond())
				pattr.Ctime = now.Unix()
				pattr.Ctimensec = uint32(now.Nanosecond())
				updateParent = true
			}
		}

		attr.Atime = now.Unix()
		attr.Atimensec = uint32(now.Nanosecond())
		attr.Mtime = now.Unix()
		attr.Mtimensec = uint32(now.Nanosecond())
		attr.Ctime = now.Unix()
		attr.Ctimensec = uint32(now.Nanosecond())

		if ctx.Value(CtxKey("behavior")) == "Hadoop" || runtime.GOOS == "darwin" {
			attr.Gid = pattr.Gid
		} else if runtime.GOOS == "linux" && pattr.Mode&02000 != 0 {
			attr.Gid = pattr.Gid
			if _type == TypeDirectory {
				attr.Mode |= 02000
			} else if attr.Mode&02010 == 02010 && ctx.Uid() != 0 {
				var found bool
				for _, gid := range ctx.Gids() {
					if gid == pattr.Gid {
						found = true
						break
					}
				}
				if !found {
					attr.Mode &= ^uint16(02000)
				}
			}
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(*inode), m.marshal(attr), 0)
			if updateParent {
				pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
			}
			if _type == TypeSymlink {
				pipe.Set(ctx, m.symKey(*inode), path, 0)
			}
			pipe.HSet(ctx, entryKey, name, m.packEntry(_type, *inode))
			if _type == TypeDirectory {
				field := (*inode).String()
				pipe.HSet(ctx, m.dirUsedInodesKey(), field, "0")
				pipe.HSet(ctx, m.dirDataLengthKey(), field, "0")
				pipe.HSet(ctx, m.dirUsedSpaceKey(), field, "0")
			}
			pipe.IncrBy(ctx, m.usedSpaceKey(), align4K(0))
			pipe.Incr(ctx, m.totalInodesKey())
			return nil
		})
		return err
	}, m.inodeKey(parent), m.entryKey(parent))

	return errno(err)
}

func (m *rueidisMeta) doUnlink(ctx Context, parent Ino, name string, attr *Attr, skipCheckTrash ...bool) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doUnlink(ctx, parent, name, attr, skipCheckTrash...)
	}

	var trash, inode Ino
	if !(len(skipCheckTrash) == 1 && skipCheckTrash[0]) {
		if st := m.checkTrash(parent, &trash); st != 0 {
			return st
		}
	}
	if trash == 0 {
		defer func() { m.of.InvalidateChunk(inode, invalidateAttrOnly) }()
	}
	if attr == nil {
		attr = &Attr{}
	}

	var (
		typ      uint8
		opened   bool
		newSpace int64
		newInode int64
	)

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		opened = false
		*attr = Attr{}
		newSpace, newInode = 0, 0

		entryBuf, err := tx.HGet(ctx, m.entryKey(parent), name).Bytes()
		if err == rueidiscompat.Nil && m.conf.CaseInsensi {
			if e := m.resolveCase(ctx, parent, name); e != nil {
				name = string(e.Name)
				entryBuf = m.packEntry(e.Attr.Typ, e.Inode)
				err = nil
			}
		}
		if err != nil {
			return err
		}

		typ, inode = m.parseEntry(entryBuf)
		if typ == TypeDirectory {
			return syscall.EPERM
		}

		if watchErr := tx.Watch(ctx, m.inodeKey(inode)).Err(); watchErr != nil {
			return watchErr
		}

		rs, err := tx.MGet(ctx, m.inodeKey(parent), m.inodeKey(inode)).Result()
		if err != nil {
			return err
		}
		if len(rs) < 2 || rs[0] == nil {
			return rueidiscompat.Nil
		}

		var pattr Attr
		switch v := rs[0].(type) {
		case string:
			m.parseAttr([]byte(v), &pattr)
		case []byte:
			m.parseAttr(v, &pattr)
		default:
			return fmt.Errorf("unexpected parent attr type %T", rs[0])
		}
		if pattr.Typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if st := m.Access(ctx, parent, MODE_MASK_W|MODE_MASK_X, &pattr); st != 0 {
			return st
		}
		if (pattr.Flags&FlagAppend) != 0 || (pattr.Flags&FlagImmutable) != 0 {
			return syscall.EPERM
		}

		var updateParent bool
		now := time.Now()
		if !parent.IsTrash() && now.Sub(time.Unix(pattr.Mtime, int64(pattr.Mtimensec))) >= m.conf.SkipDirMtime {
			pattr.Mtime = now.Unix()
			pattr.Mtimensec = uint32(now.Nanosecond())
			pattr.Ctime = now.Unix()
			pattr.Ctimensec = uint32(now.Nanosecond())
			updateParent = true
		}

		if rs[1] != nil {
			switch v := rs[1].(type) {
			case string:
				m.parseAttr([]byte(v), attr)
			case []byte:
				m.parseAttr(v, attr)
			default:
				return fmt.Errorf("unexpected attr type %T", rs[1])
			}
			if ctx.Uid() != 0 && pattr.Mode&01000 != 0 && ctx.Uid() != pattr.Uid && ctx.Uid() != attr.Uid {
				return syscall.EACCES
			}
			if (attr.Flags&FlagAppend) != 0 || (attr.Flags&FlagImmutable) != 0 {
				return syscall.EPERM
			}
			if (attr.Flags&FlagSkipTrash) != 0 && trash > 0 {
				trash = 0
				defer func() { m.of.InvalidateChunk(inode, invalidateAttrOnly) }()
			}
			if trash > 0 && attr.Nlink > 1 {
				exists, e := tx.HExists(ctx, m.entryKey(trash), m.trashEntry(parent, inode, name)).Result()
				if e != nil {
					return e
				}
				if exists {
					trash = 0
					defer func() { m.of.InvalidateChunk(inode, invalidateAttrOnly) }()
				}
			}
			attr.Ctime = now.Unix()
			attr.Ctimensec = uint32(now.Nanosecond())
			if trash == 0 {
				attr.Nlink--
				if typ == TypeFile && attr.Nlink == 0 && m.sid > 0 {
					opened = m.of.IsOpen(inode)
				}
			} else if attr.Parent > 0 {
				attr.Parent = trash
			}
		} else {
			logger.Warnf("no attribute for inode %d (%d, %s)", inode, parent, name)
			trash = 0
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.HDel(ctx, m.entryKey(parent), name)
			if updateParent {
				pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
			}
			if attr.Nlink > 0 {
				pipe.Set(ctx, m.inodeKey(inode), m.marshal(attr), 0)
				if trash > 0 {
					pipe.HSet(ctx, m.entryKey(trash), m.trashEntry(parent, inode, name), entryBuf)
					if attr.Parent == 0 {
						pipe.HIncrBy(ctx, m.parentKey(inode), trash.String(), 1)
					}
				}
				if attr.Parent == 0 {
					pipe.HIncrBy(ctx, m.parentKey(inode), parent.String(), -1)
				}
			} else {
				switch typ {
				case TypeFile:
					if opened {
						pipe.Set(ctx, m.inodeKey(inode), m.marshal(attr), 0)
						pipe.SAdd(ctx, m.sustained(m.sid), strconv.Itoa(int(inode)))
					} else {
						pipe.ZAdd(ctx, m.delfiles(), rueidiscompat.Z{Score: float64(now.Unix()), Member: m.toDelete(inode, attr.Length)})
						pipe.Del(ctx, m.inodeKey(inode))
						newSpace, newInode = -align4K(attr.Length), -1
						pipe.IncrBy(ctx, m.usedSpaceKey(), newSpace)
						pipe.Decr(ctx, m.totalInodesKey())
					}
				case TypeSymlink:
					pipe.Del(ctx, m.symKey(inode))
					fallthrough
				default:
					pipe.Del(ctx, m.inodeKey(inode))
					newSpace, newInode = -align4K(0), -1
					pipe.IncrBy(ctx, m.usedSpaceKey(), newSpace)
					pipe.Decr(ctx, m.totalInodesKey())
				}
				pipe.Del(ctx, m.xattrKey(inode))
				if attr.Parent == 0 {
					pipe.Del(ctx, m.parentKey(inode))
				}
			}
			return nil
		})
		return err
	}, m.inodeKey(parent), m.entryKey(parent))

	if err == nil && trash == 0 {
		if typ == TypeFile && attr.Nlink == 0 {
			m.fileDeleted(opened, parent.IsTrash(), inode, attr.Length)
		}
		m.updateStats(newSpace, newInode)
	}
	return errno(err)
}

func (m *rueidisMeta) doRename(ctx Context, parentSrc Ino, nameSrc string, parentDst Ino, nameDst string, flags uint32, inode, tInode *Ino, attr, tAttr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doRename(ctx, parentSrc, nameSrc, parentDst, nameDst, flags, inode, tInode, attr, tAttr)
	}

	exchange := flags == RenameExchange
	var opened bool
	var trash, dino Ino
	var dtyp uint8
	var tattr Attr
	var newSpace, newInode int64
	keys := []string{m.inodeKey(parentSrc), m.entryKey(parentSrc), m.inodeKey(parentDst), m.entryKey(parentDst)}
	if parentSrc.IsTrash() {
		keys[0], keys[2] = keys[2], keys[0]
	}

	var ino Ino
	var typ uint8

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		opened = false
		dino, dtyp = 0, 0
		tattr = Attr{}
		newSpace, newInode = 0, 0

		buf, err := tx.HGet(ctx, m.entryKey(parentSrc), nameSrc).Bytes()
		if err == rueidiscompat.Nil && m.conf.CaseInsensi {
			if e := m.resolveCase(ctx, parentSrc, nameSrc); e != nil {
				nameSrc = string(e.Name)
				buf = m.packEntry(e.Attr.Typ, e.Inode)
				err = nil
			}
		}
		if err != nil {
			return err
		}

		typ, ino = m.parseEntry(buf)
		if parentSrc == parentDst && nameSrc == nameDst {
			if inode != nil {
				*inode = ino
			}
			return nil
		}

		watchKeys := []string{m.inodeKey(ino)}
		dbuf, err := tx.HGet(ctx, m.entryKey(parentDst), nameDst).Bytes()
		if err == rueidiscompat.Nil && m.conf.CaseInsensi {
			if e := m.resolveCase(ctx, parentDst, nameDst); e != nil {
				if nameSrc != string(e.Name) || parentDst != parentSrc {
					nameDst = string(e.Name)
					dbuf = m.packEntry(e.Attr.Typ, e.Inode)
					err = nil
				}
			}
		}
		if err != nil && err != rueidiscompat.Nil {
			return err
		}
		if err == nil {
			if flags&RenameNoReplace != 0 {
				return syscall.EEXIST
			}
			dtyp, dino = m.parseEntry(dbuf)
			watchKeys = append(watchKeys, m.inodeKey(dino))
			if dtyp == TypeDirectory {
				watchKeys = append(watchKeys, m.entryKey(dino))
			}
			if !exchange {
				if st := m.checkTrash(parentDst, &trash); st != 0 {
					return st
				}
			}
		}
		if watchErr := tx.Watch(ctx, watchKeys...).Err(); watchErr != nil {
			return watchErr
		}
		if dino > 0 {
			if ino == dino {
				return nil
			}
			if exchange {
			} else if typ == TypeDirectory && dtyp != TypeDirectory {
				return syscall.ENOTDIR
			} else if typ != TypeDirectory && dtyp == TypeDirectory {
				return syscall.EISDIR
			}
		}

		attrKeys := []string{m.inodeKey(parentSrc), m.inodeKey(parentDst), m.inodeKey(ino)}
		if dino > 0 {
			attrKeys = append(attrKeys, m.inodeKey(dino))
		}
		rs, err := tx.MGet(ctx, attrKeys...).Result()
		if err != nil {
			return err
		}
		if len(rs) < 3 || rs[0] == nil || rs[1] == nil || rs[2] == nil {
			return rueidiscompat.Nil
		}

		var sattr, dattr, iattr Attr
		switch v := rs[0].(type) {
		case string:
			m.parseAttr([]byte(v), &sattr)
		case []byte:
			m.parseAttr(v, &sattr)
		default:
			return fmt.Errorf("unexpected source parent attr type %T", rs[0])
		}
		if sattr.Typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if st := m.Access(ctx, parentSrc, MODE_MASK_W|MODE_MASK_X, &sattr); st != 0 {
			return st
		}
		switch v := rs[1].(type) {
		case string:
			m.parseAttr([]byte(v), &dattr)
		case []byte:
			m.parseAttr(v, &dattr)
		default:
			return fmt.Errorf("unexpected dest parent attr type %T", rs[1])
		}
		if dattr.Typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if flags&RenameRestore == 0 && dattr.Parent > TrashInode {
			return syscall.ENOENT
		}
		if st := m.Access(ctx, parentDst, MODE_MASK_W|MODE_MASK_X, &dattr); st != 0 {
			return st
		}
		if ino == parentDst || ino == dattr.Parent {
			return syscall.EPERM
		}
		switch v := rs[2].(type) {
		case string:
			m.parseAttr([]byte(v), &iattr)
		case []byte:
			m.parseAttr(v, &iattr)
		default:
			return fmt.Errorf("unexpected inode attr type %T", rs[2])
		}
		if (sattr.Flags&FlagAppend) != 0 || (sattr.Flags&FlagImmutable) != 0 || (dattr.Flags&FlagImmutable) != 0 || (iattr.Flags&FlagAppend) != 0 || (iattr.Flags&FlagImmutable) != 0 {
			return syscall.EPERM
		}
		if parentSrc != parentDst && sattr.Mode&0o1000 != 0 && ctx.Uid() != 0 &&
			ctx.Uid() != iattr.Uid && (ctx.Uid() != sattr.Uid || iattr.Typ == TypeDirectory) {
			return syscall.EACCES
		}

		var supdate, dupdate bool
		now := time.Now()
		if dino > 0 {
			if len(rs) < 4 || rs[3] == nil {
				logger.Warnf("no attribute for inode %d (%d, %s)", dino, parentDst, nameDst)
				trash = 0
			} else {
				switch v := rs[3].(type) {
				case string:
					m.parseAttr([]byte(v), &tattr)
				case []byte:
					m.parseAttr(v, &tattr)
				default:
					return fmt.Errorf("unexpected target attr type %T", rs[3])
				}
			}
			if (tattr.Flags&FlagAppend) != 0 || (tattr.Flags&FlagImmutable) != 0 {
				return syscall.EPERM
			}
			if (tattr.Flags & FlagSkipTrash) != 0 {
				trash = 0
			}
			tattr.Ctime = now.Unix()
			tattr.Ctimensec = uint32(now.Nanosecond())
			if exchange {
				if parentSrc != parentDst {
					if dtyp == TypeDirectory {
						tattr.Parent = parentSrc
						dattr.Nlink--
						sattr.Nlink++
						supdate, dupdate = true, true
					} else if tattr.Parent > 0 {
						tattr.Parent = parentSrc
					}
				}
			} else {
				if dtyp == TypeDirectory {
					cnt, err := tx.HLen(ctx, m.entryKey(dino)).Result()
					if err != nil {
						return err
					}
					if cnt != 0 {
						return syscall.ENOTEMPTY
					}
					dattr.Nlink--
					dupdate = true
					if trash > 0 {
						tattr.Parent = trash
					}
				} else {
					if trash == 0 {
						tattr.Nlink--
						if dtyp == TypeFile && tattr.Nlink == 0 {
							opened = m.of.IsOpen(dino)
						}
						defer func() { m.of.InvalidateChunk(dino, invalidateAttrOnly) }()
					} else if tattr.Parent > 0 {
						tattr.Parent = trash
					}
				}
			}
			if ctx.Uid() != 0 && dattr.Mode&01000 != 0 && ctx.Uid() != dattr.Uid && ctx.Uid() != tattr.Uid {
				return syscall.EACCES
			}
		} else if exchange {
			return syscall.ENOENT
		}
		if ctx.Uid() != 0 && sattr.Mode&01000 != 0 && ctx.Uid() != sattr.Uid && ctx.Uid() != iattr.Uid {
			return syscall.EACCES
		}

		if parentSrc != parentDst {
			if typ == TypeDirectory {
				iattr.Parent = parentDst
				sattr.Nlink--
				dattr.Nlink++
				supdate, dupdate = true, true
			} else if iattr.Parent > 0 {
				iattr.Parent = parentDst
			}
		}
		if supdate || now.Sub(time.Unix(sattr.Mtime, int64(sattr.Mtimensec))) >= m.conf.SkipDirMtime {
			sattr.Mtime = now.Unix()
			sattr.Mtimensec = uint32(now.Nanosecond())
			sattr.Ctime = now.Unix()
			sattr.Ctimensec = uint32(now.Nanosecond())
			supdate = true
		}
		if dupdate || now.Sub(time.Unix(dattr.Mtime, int64(dattr.Mtimensec))) >= m.conf.SkipDirMtime {
			dattr.Mtime = now.Unix()
			dattr.Mtimensec = uint32(now.Nanosecond())
			dattr.Ctime = now.Unix()
			dattr.Ctimensec = uint32(now.Nanosecond())
			dupdate = true
		}
		iattr.Ctime = now.Unix()
		iattr.Ctimensec = uint32(now.Nanosecond())
		if inode != nil {
			*inode = ino
		}
		if attr != nil {
			*attr = iattr
		}
		if dino > 0 {
			if tInode != nil {
				*tInode = dino
			}
			if tAttr != nil {
				*tAttr = tattr
			}
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			if exchange {
				pipe.Set(ctx, m.inodeKey(dino), m.marshal(&tattr), 0)
				pipe.HSet(ctx, m.entryKey(parentSrc), nameSrc, dbuf)
				if parentSrc != parentDst && tattr.Parent == 0 {
					pipe.HIncrBy(ctx, m.parentKey(dino), parentSrc.String(), 1)
					pipe.HIncrBy(ctx, m.parentKey(dino), parentDst.String(), -1)
				}
			} else {
				pipe.HDel(ctx, m.entryKey(parentSrc), nameSrc)
				if dino > 0 {
					if trash > 0 {
						pipe.Set(ctx, m.inodeKey(dino), m.marshal(&tattr), 0)
						pipe.HSet(ctx, m.entryKey(trash), m.trashEntry(parentDst, dino, nameDst), dbuf)
						if tattr.Parent == 0 {
							pipe.HIncrBy(ctx, m.parentKey(dino), trash.String(), 1)
							pipe.HIncrBy(ctx, m.parentKey(dino), parentDst.String(), -1)
						}
					} else if dtyp != TypeDirectory && tattr.Nlink > 0 {
						pipe.Set(ctx, m.inodeKey(dino), m.marshal(&tattr), 0)
						if tattr.Parent == 0 {
							pipe.HIncrBy(ctx, m.parentKey(dino), parentDst.String(), -1)
						}
					} else {
						if dtyp == TypeFile {
							if opened {
								pipe.Set(ctx, m.inodeKey(dino), m.marshal(&tattr), 0)
								pipe.SAdd(ctx, m.sustained(m.sid), strconv.Itoa(int(dino)))
							} else {
								pipe.ZAdd(ctx, m.delfiles(), rueidiscompat.Z{Score: float64(now.Unix()), Member: m.toDelete(dino, tattr.Length)})
								pipe.Del(ctx, m.inodeKey(dino))
								newSpace, newInode = -align4K(tattr.Length), -1
								pipe.IncrBy(ctx, m.usedSpaceKey(), newSpace)
								pipe.Decr(ctx, m.totalInodesKey())
							}
						} else {
							if dtyp == TypeSymlink {
								pipe.Del(ctx, m.symKey(dino))
							}
							pipe.Del(ctx, m.inodeKey(dino))
							newSpace, newInode = -align4K(0), -1
							pipe.IncrBy(ctx, m.usedSpaceKey(), newSpace)
							pipe.Decr(ctx, m.totalInodesKey())
						}
						pipe.Del(ctx, m.xattrKey(dino))
						if tattr.Parent == 0 {
							pipe.Del(ctx, m.parentKey(dino))
						}
					}
					if dtyp == TypeDirectory {
						field := dino.String()
						pipe.HDel(ctx, m.dirQuotaKey(), field)
						pipe.HDel(ctx, m.dirQuotaUsedSpaceKey(), field)
						pipe.HDel(ctx, m.dirQuotaUsedInodesKey(), field)
					}
				}
			}
			if parentDst != parentSrc {
				if !parentSrc.IsTrash() && supdate {
					pipe.Set(ctx, m.inodeKey(parentSrc), m.marshal(&sattr), 0)
				}
				if iattr.Parent == 0 {
					pipe.HIncrBy(ctx, m.parentKey(ino), parentDst.String(), 1)
					pipe.HIncrBy(ctx, m.parentKey(ino), parentSrc.String(), -1)
				}
			}
			pipe.Set(ctx, m.inodeKey(ino), m.marshal(&iattr), 0)
			pipe.HSet(ctx, m.entryKey(parentDst), nameDst, buf)
			if dupdate {
				pipe.Set(ctx, m.inodeKey(parentDst), m.marshal(&dattr), 0)
			}
			return nil
		})
		return err
	}, keys...)
	if err == nil && !exchange && trash == 0 {
		if dino > 0 && dtyp == TypeFile && tattr.Nlink == 0 {
			m.fileDeleted(opened, false, dino, tattr.Length)
		}
		m.updateStats(newSpace, newInode)
	}
	return errno(err)
}

func (m *rueidisMeta) doLink(ctx Context, inode, parent Ino, name string, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doLink(ctx, inode, parent, name, attr)
	}

	var newSpace, newInode int64

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		newSpace, newInode = 0, 0

		rs, err := tx.MGet(ctx, m.inodeKey(parent), m.inodeKey(inode)).Result()
		if err != nil {
			return err
		}
		if len(rs) < 2 || rs[0] == nil || rs[1] == nil {
			return rueidiscompat.Nil
		}

		var pattr, iattr Attr
		switch v := rs[0].(type) {
		case string:
			m.parseAttr([]byte(v), &pattr)
		case []byte:
			m.parseAttr(v, &pattr)
		default:
			return fmt.Errorf("unexpected parent attr type %T", rs[0])
		}
		if pattr.Typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if pattr.Parent > TrashInode {
			return syscall.ENOENT
		}
		if st := m.Access(ctx, parent, MODE_MASK_W|MODE_MASK_X, &pattr); st != 0 {
			return st
		}
		if (pattr.Flags & FlagImmutable) != 0 {
			return syscall.EPERM
		}

		if err := tx.Watch(ctx, m.entryKey(parent)).Err(); err != nil {
			return err
		}

		switch v := rs[1].(type) {
		case string:
			m.parseAttr([]byte(v), &iattr)
		case []byte:
			m.parseAttr(v, &iattr)
		default:
			return fmt.Errorf("unexpected inode attr type %T", rs[1])
		}
		if iattr.Typ == TypeDirectory {
			return syscall.EPERM
		}
		if (iattr.Flags&FlagAppend) != 0 || (iattr.Flags&FlagImmutable) != 0 {
			return syscall.EPERM
		}

		now := time.Now()
		var updateParent bool
		if now.Sub(time.Unix(pattr.Mtime, int64(pattr.Mtimensec))) >= m.conf.SkipDirMtime {
			pattr.Mtime = now.Unix()
			pattr.Mtimensec = uint32(now.Nanosecond())
			pattr.Ctime = now.Unix()
			pattr.Ctimensec = uint32(now.Nanosecond())
			updateParent = true
		}

		oldParent := iattr.Parent
		iattr.Parent = 0
		iattr.Ctime = now.Unix()
		iattr.Ctimensec = uint32(now.Nanosecond())
		iattr.Nlink++

		if err := tx.HGet(ctx, m.entryKey(parent), name).Err(); err != nil {
			if err != rueidiscompat.Nil {
				return err
			}
			if m.conf.CaseInsensi && m.resolveCase(ctx, parent, name) != nil {
				return syscall.EEXIST
			}
		} else {
			return syscall.EEXIST
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.HSet(ctx, m.entryKey(parent), name, m.packEntry(iattr.Typ, inode))
			if updateParent {
				pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
			}
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(&iattr), 0)
			if oldParent > 0 {
				pipe.HIncrBy(ctx, m.parentKey(inode), oldParent.String(), 1)
			}
			pipe.HIncrBy(ctx, m.parentKey(inode), parent.String(), 1)
			return nil
		})
		if err == nil && attr != nil {
			*attr = iattr
		}
		return err
	}, m.inodeKey(parent), m.entryKey(parent), m.inodeKey(inode))

	if err == nil {
		m.updateStats(newSpace, newInode)
	}
	return errno(err)
}

func (m *rueidisMeta) doRead(ctx Context, inode Ino, indx uint32) ([]*slice, syscall.Errno) {
	if m.compat == nil {
		return m.redisMeta.doRead(ctx, inode, indx)
	}

	vals, err := m.compat.LRange(ctx, m.chunkKey(inode, indx), 0, -1).Result()
	if err != nil {
		return nil, errno(err)
	}
	return readSlices(vals), 0
}

func (m *rueidisMeta) CopyFileRange(ctx Context, fin Ino, offIn uint64, fout Ino, offOut uint64, size uint64, flags uint32, copied, outLength *uint64) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.CopyFileRange(ctx, fin, offIn, fout, offOut, size, flags, copied, outLength)
	}

	defer m.timeit("CopyFileRange", time.Now())
	f := m.of.find(fout)
	if f != nil {
		f.Lock()
		defer f.Unlock()
	}
	var newLength, newSpace int64
	var sattr, attr Attr
	defer func() { m.of.InvalidateChunk(fout, invalidateAllChunks) }()

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		newLength, newSpace = 0, 0
		rs, err := tx.MGet(ctx, m.inodeKey(fin), m.inodeKey(fout)).Result()
		if err != nil {
			return err
		}
		if len(rs) < 2 || rs[0] == nil || rs[1] == nil {
			return rueidiscompat.Nil
		}

		sattr = Attr{}
		switch v := rs[0].(type) {
		case string:
			m.parseAttr([]byte(v), &sattr)
		case []byte:
			m.parseAttr(v, &sattr)
		default:
			return fmt.Errorf("unexpected source attr type %T", rs[0])
		}
		if sattr.Typ != TypeFile {
			return syscall.EINVAL
		}
		if offIn >= sattr.Length {
			if copied != nil {
				*copied = 0
			}
			return nil
		}

		sizeToCopy := size
		if offIn+sizeToCopy > sattr.Length {
			sizeToCopy = sattr.Length - offIn
		}

		switch v := rs[1].(type) {
		case string:
			m.parseAttr([]byte(v), &attr)
		case []byte:
			m.parseAttr(v, &attr)
		default:
			return fmt.Errorf("unexpected dest attr type %T", rs[1])
		}
		if attr.Typ != TypeFile {
			return syscall.EINVAL
		}
		if (attr.Flags&FlagImmutable) != 0 || (attr.Flags&FlagAppend) != 0 {
			return syscall.EPERM
		}

		newLeng := offOut + sizeToCopy
		if newLeng > attr.Length {
			newLength = int64(newLeng - attr.Length)
			newSpace = align4K(newLeng) - align4K(attr.Length)
			attr.Length = newLeng
		}
		if err := m.checkQuota(ctx, newSpace, 0, attr.Uid, attr.Gid, m.getParentsCompat(ctx, tx, fout, attr.Parent)...); err != 0 {
			return err
		}

		now := time.Now()
		attr.Mtime = now.Unix()
		attr.Mtimensec = uint32(now.Nanosecond())
		attr.Ctime = now.Unix()
		attr.Ctimensec = uint32(now.Nanosecond())
		if outLength != nil {
			*outLength = attr.Length
		}

		var vals [][]string
		for i := offIn / ChunkSize; i <= (offIn+sizeToCopy)/ChunkSize; i++ {
			list, e := tx.LRange(ctx, m.chunkKey(fin, uint32(i)), 0, -1).Result()
			if e != nil {
				return e
			}
			vals = append(vals, list)
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			coff := offIn / ChunkSize * ChunkSize
			for _, sv := range vals {
				slices := readSlices(sv)
				if slices == nil {
					return syscall.EIO
				}
				slices = append([]*slice{{len: ChunkSize}}, slices...)
				cs := buildSlice(slices)
				tpos := coff
				for _, s := range cs {
					pos := tpos
					tpos += uint64(s.Len)
					if pos < offIn+sizeToCopy && pos+uint64(s.Len) > offIn {
						if pos < offIn {
							dec := offIn - pos
							s.Off += uint32(dec)
							pos += dec
							s.Len -= uint32(dec)
						}
						if pos+uint64(s.Len) > offIn+sizeToCopy {
							dec := pos + uint64(s.Len) - (offIn + sizeToCopy)
							s.Len -= uint32(dec)
						}
						doff := pos - offIn + offOut
						indx := uint32(doff / ChunkSize)
						dpos := uint32(doff % ChunkSize)
						if dpos+s.Len > ChunkSize {
							pipe.RPush(ctx, m.chunkKey(fout, indx), marshalSlice(dpos, s.Id, s.Size, s.Off, ChunkSize-dpos))
							if s.Id > 0 {
								pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.Id, s.Size), 1)
							}

							skip := ChunkSize - dpos
							pipe.RPush(ctx, m.chunkKey(fout, indx+1), marshalSlice(0, s.Id, s.Size, s.Off+skip, s.Len-skip))
							if s.Id > 0 {
								pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.Id, s.Size), 1)
							}
						} else {
							pipe.RPush(ctx, m.chunkKey(fout, indx), marshalSlice(dpos, s.Id, s.Size, s.Off, s.Len))
							if s.Id > 0 {
								pipe.HIncrBy(ctx, m.sliceRefs(), m.sliceKey(s.Id, s.Size), 1)
							}
						}
					}
				}
				coff += ChunkSize
			}
			pipe.Set(ctx, m.inodeKey(fout), m.marshal(&attr), 0)
			if newSpace > 0 {
				pipe.IncrBy(ctx, m.usedSpaceKey(), newSpace)
			}
			return nil
		})
		if err == nil && copied != nil {
			*copied = sizeToCopy
		}
		return err
	}, m.inodeKey(fout), m.inodeKey(fin))
	if err == nil {
		m.updateParentStat(ctx, fout, attr.Parent, newLength, newSpace)
	}
	return errno(err)
}

func (m *rueidisMeta) doWrite(ctx Context, inode Ino, indx uint32, off uint32, slice Slice, mtime time.Time, numSlices *int, delta *dirStat, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doWrite(ctx, inode, indx, off, slice, mtime, numSlices, delta, attr)
	}

	return errno(m.txn(ctx, func(tx rueidiscompat.Tx) error {
		*delta = dirStat{}
		*attr = Attr{}
		data, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(data, attr)
		if attr.Typ != TypeFile {
			return syscall.EPERM
		}

		newLength := uint64(indx)*ChunkSize + uint64(off) + uint64(slice.Len)
		if newLength > attr.Length {
			delta.length = int64(newLength - attr.Length)
			delta.space = align4K(newLength) - align4K(attr.Length)
			attr.Length = newLength
		}
		if err := m.checkQuota(ctx, delta.space, 0, attr.Uid, attr.Gid, m.getParentsCompat(ctx, tx, inode, attr.Parent)...); err != 0 {
			return err
		}

		now := time.Now()
		attr.Mtime = mtime.Unix()
		attr.Mtimensec = uint32(mtime.Nanosecond())
		attr.Ctime = now.Unix()
		attr.Ctimensec = uint32(now.Nanosecond())

		var rpush *rueidiscompat.IntCmd
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			rpush = pipe.RPush(ctx, m.chunkKey(inode, indx), marshalSlice(off, slice.Id, slice.Size, slice.Off, slice.Len))
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(attr), 0)
			if delta.space > 0 {
				pipe.IncrBy(ctx, m.usedSpaceKey(), delta.space)
			}
			return nil
		})
		if err == nil {
			val, e := rpush.Result()
			if e != nil {
				return e
			}
			*numSlices = int(val)
		}
		return err
	}, m.inodeKey(inode)))
}

func (m *rueidisMeta) doRmdir(ctx Context, parent Ino, name string, pinode *Ino, oldAttr *Attr, skipCheckTrash ...bool) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doRmdir(ctx, parent, name, pinode, oldAttr, skipCheckTrash...)
	}

	var trash Ino
	if !(len(skipCheckTrash) == 1 && skipCheckTrash[0]) {
		if st := m.checkTrash(parent, &trash); st != 0 {
			return st
		}
	}

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		entryBuf, err := tx.HGet(ctx, m.entryKey(parent), name).Bytes()
		if err == rueidiscompat.Nil && m.conf.CaseInsensi {
			if e := m.resolveCase(ctx, parent, name); e != nil {
				name = string(e.Name)
				entryBuf = m.packEntry(e.Attr.Typ, e.Inode)
				err = nil
			}
		}
		if err != nil {
			return err
		}

		typ, inode := m.parseEntry(entryBuf)
		if typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if pinode != nil {
			*pinode = inode
		}
		if watchErr := tx.Watch(ctx, m.inodeKey(inode), m.entryKey(inode)).Err(); watchErr != nil {
			return watchErr
		}

		rs, err := tx.MGet(ctx, m.inodeKey(parent), m.inodeKey(inode)).Result()
		if err != nil {
			return err
		}
		if len(rs) < 2 || rs[0] == nil {
			return rueidiscompat.Nil
		}

		var pattr Attr
		switch v := rs[0].(type) {
		case string:
			m.parseAttr([]byte(v), &pattr)
		case []byte:
			m.parseAttr(v, &pattr)
		default:
			return fmt.Errorf("unexpected parent attr type %T", rs[0])
		}
		if pattr.Typ != TypeDirectory {
			return syscall.ENOTDIR
		}
		if st := m.Access(ctx, parent, MODE_MASK_W|MODE_MASK_X, &pattr); st != 0 {
			return st
		}
		if (pattr.Flags&FlagAppend) != 0 || (pattr.Flags&FlagImmutable) != 0 {
			return syscall.EPERM
		}

		now := time.Now()
		pattr.Nlink--
		pattr.Mtime = now.Unix()
		pattr.Mtimensec = uint32(now.Nanosecond())
		pattr.Ctime = now.Unix()
		pattr.Ctimensec = uint32(now.Nanosecond())

		cnt, err := tx.HLen(ctx, m.entryKey(inode)).Result()
		if err != nil {
			return err
		}
		if cnt > 0 {
			return syscall.ENOTEMPTY
		}

		var attr Attr
		if rs[1] != nil {
			switch v := rs[1].(type) {
			case string:
				m.parseAttr([]byte(v), &attr)
			case []byte:
				m.parseAttr(v, &attr)
			default:
				return fmt.Errorf("unexpected attr type %T", rs[1])
			}
			if oldAttr != nil {
				*oldAttr = attr
			}
			if ctx.Uid() != 0 && pattr.Mode&01000 != 0 && ctx.Uid() != pattr.Uid && ctx.Uid() != attr.Uid {
				return syscall.EACCES
			}
			if (attr.Flags & FlagSkipTrash) != 0 {
				trash = 0
			}
			if trash > 0 {
				attr.Ctime = now.Unix()
				attr.Ctimensec = uint32(now.Nanosecond())
				attr.Parent = trash
			}
		} else {
			logger.Warnf("no attribute for inode %d (%d, %s)", inode, parent, name)
			trash = 0
		}

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.HDel(ctx, m.entryKey(parent), name)
			if !parent.IsTrash() {
				pipe.Set(ctx, m.inodeKey(parent), m.marshal(&pattr), 0)
			}
			if trash > 0 {
				pipe.Set(ctx, m.inodeKey(inode), m.marshal(&attr), 0)
				pipe.HSet(ctx, m.entryKey(trash), m.trashEntry(parent, inode, name), entryBuf)
			} else {
				pipe.Del(ctx, m.inodeKey(inode))
				pipe.Del(ctx, m.xattrKey(inode))
				pipe.IncrBy(ctx, m.usedSpaceKey(), -align4K(0))
				pipe.Decr(ctx, m.totalInodesKey())
			}

			field := inode.String()
			pipe.HDel(ctx, m.dirDataLengthKey(), field)
			pipe.HDel(ctx, m.dirUsedSpaceKey(), field)
			pipe.HDel(ctx, m.dirUsedInodesKey(), field)
			pipe.HDel(ctx, m.dirQuotaKey(), field)
			pipe.HDel(ctx, m.dirQuotaUsedSpaceKey(), field)
			pipe.HDel(ctx, m.dirQuotaUsedInodesKey(), field)
			return nil
		})
		return err
	}, m.inodeKey(parent), m.entryKey(parent))

	if err == nil && trash == 0 {
		m.updateStats(-align4K(0), -1)
	}
	return errno(err)
}

func (m *rueidisMeta) doTruncate(ctx Context, inode Ino, flags uint8, length uint64, delta *dirStat, attr *Attr, skipPermCheck bool) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doTruncate(ctx, inode, flags, length, delta, attr, skipPermCheck)
	}

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		*delta = dirStat{}
		var current Attr
		data, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(data, &current)
		if current.Typ != TypeFile || current.Flags&(FlagImmutable|FlagAppend) != 0 || (flags == 0 && current.Parent > TrashInode) {
			return syscall.EPERM
		}
		if !skipPermCheck {
			if st := m.Access(ctx, inode, MODE_MASK_W, &current); st != 0 {
				return st
			}
		}
		if length == current.Length {
			*attr = current
			return nil
		}

		left, right := current.Length, length
		if left > right {
			right, left = left, right
		}
		delta.length = int64(length) - int64(current.Length)
		delta.space = align4K(length) - align4K(current.Length)
		if err := m.checkQuota(ctx, delta.space, 0, current.Uid, current.Gid, m.getParentsCompat(ctx, tx, inode, current.Parent)...); err != 0 {
			return err
		}

		var zeroChunks []uint32
		if right > left {
			if (right-left)/ChunkSize >= 10000 {
				pattern := m.prefix + fmt.Sprintf("c%d_*", inode)
				var cursor uint64
				for {
					keys, next, scanErr := tx.Scan(ctx, cursor, pattern, 10000).Result()
					if scanErr != nil {
						return scanErr
					}
					for _, key := range keys {
						parts := strings.Split(key[len(m.prefix):], "_")
						if len(parts) < 2 {
							logger.Errorf("invalid chunk key %s", key)
							continue
						}
						indx, convErr := strconv.Atoi(parts[1])
						if convErr != nil {
							logger.Errorf("parse %s: %v", key, convErr)
							continue
						}
						if uint64(indx) > left/ChunkSize && uint64(indx) < right/ChunkSize {
							zeroChunks = append(zeroChunks, uint32(indx))
						}
					}
					if next == 0 {
						break
					}
					cursor = next
				}
			} else {
				for i := left/ChunkSize + 1; i < right/ChunkSize; i++ {
					zeroChunks = append(zeroChunks, uint32(i))
				}
			}
		}

		current.Length = length
		now := time.Now()
		current.Mtime = now.Unix()
		current.Mtimensec = uint32(now.Nanosecond())
		current.Ctime = now.Unix()
		current.Ctimensec = uint32(now.Nanosecond())
		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(&current), 0)
			if right > left {
				l := uint32(right - left)
				if right > (left/ChunkSize+1)*ChunkSize {
					l = ChunkSize - uint32(left%ChunkSize)
				}
				pipe.RPush(ctx, m.chunkKey(inode, uint32(left/ChunkSize)), marshalSlice(uint32(left%ChunkSize), 0, 0, 0, l))
				buf := marshalSlice(0, 0, 0, 0, ChunkSize)
				for _, indx := range zeroChunks {
					pipe.RPushX(ctx, m.chunkKey(inode, indx), buf)
				}
				if right > (left/ChunkSize+1)*ChunkSize && right%ChunkSize > 0 {
					pipe.RPush(ctx, m.chunkKey(inode, uint32(right/ChunkSize)), marshalSlice(0, 0, 0, 0, uint32(right%ChunkSize)))
				}
			}
			pipe.IncrBy(ctx, m.usedSpaceKey(), delta.space)
			return nil
		})
		if err == nil {
			*attr = current
		}
		return err
	}, m.inodeKey(inode))

	return errno(err)
}

func (m *rueidisMeta) doFallocate(ctx Context, inode Ino, mode uint8, off uint64, size uint64, delta *dirStat, attr *Attr) syscall.Errno {
	if m.compat == nil {
		return m.redisMeta.doFallocate(ctx, inode, mode, off, size, delta, attr)
	}

	err := m.txn(ctx, func(tx rueidiscompat.Tx) error {
		*delta = dirStat{}
		var current Attr
		val, err := tx.Get(ctx, m.inodeKey(inode)).Bytes()
		if err != nil {
			return err
		}
		m.parseAttr(val, &current)
		if current.Typ == TypeFIFO {
			return syscall.EPIPE
		}
		if current.Typ != TypeFile || (current.Flags&FlagImmutable) != 0 {
			return syscall.EPERM
		}
		if st := m.Access(ctx, inode, MODE_MASK_W, &current); st != 0 {
			return st
		}
		if (current.Flags&FlagAppend) != 0 && (mode&^fallocKeepSize) != 0 {
			return syscall.EPERM
		}

		length := current.Length
		if off+size > current.Length && mode&fallocKeepSize == 0 {
			length = off + size
		}
		old := current.Length
		delta.length = int64(length) - int64(old)
		delta.space = align4K(length) - align4K(old)
		if err := m.checkQuota(ctx, delta.space, 0, current.Uid, current.Gid, m.getParentsCompat(ctx, tx, inode, current.Parent)...); err != 0 {
			return err
		}

		current.Length = length
		now := time.Now()
		current.Mtime = now.Unix()
		current.Mtimensec = uint32(now.Nanosecond())
		current.Ctime = now.Unix()
		current.Ctimensec = uint32(now.Nanosecond())

		_, err = tx.TxPipelined(ctx, func(pipe rueidiscompat.Pipeliner) error {
			pipe.Set(ctx, m.inodeKey(inode), m.marshal(&current), 0)
			if mode&(fallocZeroRange|fallocPunchHole) != 0 && off < old {
				end := off + size
				if end > old {
					end = old
				}
				pos := off
				for pos < end {
					indx := uint32(pos / ChunkSize)
					coff := pos % ChunkSize
					l := end - pos
					if coff+l > ChunkSize {
						l = ChunkSize - coff
					}
					pipe.RPush(ctx, m.chunkKey(inode, indx), marshalSlice(uint32(coff), 0, 0, 0, uint32(l)))
					pos += l
				}
			}
			pipe.IncrBy(ctx, m.usedSpaceKey(), align4K(length)-align4K(old))
			return nil
		})
		if err == nil {
			*attr = current
		}
		return err
	}, m.inodeKey(inode))

	return errno(err)
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

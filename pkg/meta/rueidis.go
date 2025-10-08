//go:build !norueidis
// +build !norueidis

package meta

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"time"

	aclAPI "github.com/juicedata/juicefs/pkg/acl"
	"github.com/juicedata/juicefs/pkg/utils"
	errors "github.com/pkg/errors"
	"github.com/redis/rueidis"
	"github.com/redis/rueidis/rueidiscompat"
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
				return errors.Wrap(err, "remove dir stats")
			}
		}
		if !old.UserGroupQuota && format.UserGroupQuota {
			if err := m.compat.Del(ctx,
				m.userQuotaKey(), m.userQuotaUsedSpaceKey(), m.userQuotaUsedInodesKey(),
				m.groupQuotaKey(), m.groupQuotaUsedSpaceKey(), m.groupQuotaUsedInodesKey()).Err(); err != nil {
				return errors.Wrap(err, "remove user group quota")
			}
		}
		if err = format.update(&old, force); err != nil {
			return errors.Wrap(err, "update format")
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

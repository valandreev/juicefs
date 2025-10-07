package meta

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/redis/go-redis/v9"
)

func init() {
	Register("redis-split", newRedisSplitMeta)
}

type redisSplitMeta struct {
	*redisMeta
	masterURI           string
	replicaURI          string
	master              redis.UniversalClient
	replica             redis.UniversalClient
	replicaHealthy      atomic.Bool
	nextReplicaProbe    int64
	replicaProbeMu      sync.Mutex
	healthCheckInterval time.Duration
}

var _ Meta = (*redisSplitMeta)(nil)
var _ engine = (*redisSplitMeta)(nil)

var redisSplitMasterFactory = newRedisMeta

var redisSplitReplicaFactory = func(uri string, conf *Config) (redis.UniversalClient, error) {
	if conf == nil {
		conf = DefaultConf()
	}
	opt, err := redis.ParseURL(uri)
	if err != nil {
		return nil, fmt.Errorf("redis parse %s: %w", uri, err)
	}
	if opt.Password == "" {
		opt.Password = os.Getenv("REDIS_PASSWORD")
	}
	if opt.Password == "" {
		opt.Password = os.Getenv("META_PASSWORD")
	}
	opt.MaxRetries = conf.Retries
	if opt.MaxRetries == 0 {
		opt.MaxRetries = -1
	}
	return redis.NewClient(opt), nil
}

const (
	routeReasonRead               = "read-op"
	routeReasonWrite              = "write-op"
	routeReasonLock               = "lock-op"
	routeReasonDefault            = "default-master"
	routeReasonReplicaUnavailable = "replica-unavailable"
)

var (
	readOps = map[string]struct{}{
		"dogetattr":  {},
		"doreaddir":  {},
		"doreadlink": {},
		"dostatfs":   {},
		"doolookup":  {},
	}
	writeOps = map[string]struct{}{
		"domknod":     {},
		"dounlink":    {},
		"dorename":    {},
		"dormdir":     {},
		"dosetattr":   {},
		"dosetxattr":  {},
		"dosymlink":   {},
		"dolink":      {},
		"dotruncate":  {},
		"dofallocate": {},
		"txn":         {},
	}
	lockOps = map[string]struct{}{
		"flock": {},
		"setlk": {},
		"getlk": {},
	}
)

func parseRedisSplitURL(raw string) (string, string, error) {
	const scheme = "redis-split://"
	if raw == "" {
		return "", "", errors.New("empty redis-split url")
	}
	if !strings.HasPrefix(raw, scheme) {
		return "", "", fmt.Errorf("invalid redis-split url, missing prefix %q", scheme)
	}
	rest := strings.TrimPrefix(raw, scheme)
	masterPart := rest
	queryPart := ""
	if idx := strings.Index(rest, "?"); idx >= 0 {
		masterPart = rest[:idx]
		queryPart = rest[idx+1:]
	}
	masterPart = strings.TrimSpace(masterPart)
	if masterPart == "" {
		return "", "", errors.New("master uri is required")
	}
	if !strings.HasPrefix(masterPart, "redis://") && !strings.HasPrefix(masterPart, "rediss://") {
		return "", "", fmt.Errorf("invalid master uri %q", masterPart)
	}
	var replica string
	if queryPart != "" {
		vals, err := url.ParseQuery(queryPart)
		if err != nil {
			return "", "", fmt.Errorf("invalid query parameters: %w", err)
		}
		replica = strings.TrimSpace(vals.Get("replica"))
	}
	if replica == "" {
		return "", "", errors.New("replica uri is required")
	}
	if !strings.HasPrefix(replica, "redis://") && !strings.HasPrefix(replica, "rediss://") {
		return "", "", fmt.Errorf("invalid replica uri %q", replica)
	}
	return masterPart, replica, nil
}

func newRedisSplitMeta(driver, addr string, conf *Config) (Meta, error) {
	if conf == nil {
		conf = DefaultConf()
	}
	masterURI, replicaURI, err := parseRedisSplitURL(driver + "://" + addr)
	if err != nil {
		return nil, err
	}
	masterDriver, masterAddr, err := splitRedisURI(masterURI)
	if err != nil {
		return nil, err
	}
	meta, err := redisSplitMasterFactory(masterDriver, masterAddr, conf)
	if err != nil {
		return nil, err
	}
	rm, ok := meta.(*redisMeta)
	if !ok {
		return nil, fmt.Errorf("unexpected redis meta type %T", meta)
	}
	replicaClient, err := redisSplitReplicaFactory(replicaURI, conf)
	if err != nil {
		rm.Shutdown()
		return nil, err
	}
	split := &redisSplitMeta{
		redisMeta:           rm,
		masterURI:           masterURI,
		replicaURI:          replicaURI,
		master:              rm.rdb,
		replica:             replicaClient,
		healthCheckInterval: 5 * time.Second,
	}
	if replicaClient != nil {
		split.replicaHealthy.Store(true)
	}
	split.en = split
	return split, nil
}

func splitRedisURI(uri string) (string, string, error) {
	idx := strings.Index(uri, "://")
	if idx <= 0 {
		return "", "", fmt.Errorf("invalid redis uri: %s", uri)
	}
	driver := uri[:idx]
	addr := uri[idx+3:]
	if addr == "" {
		return "", "", fmt.Errorf("empty redis addr: %s", uri)
	}
	return driver, addr, nil
}

func (m *redisSplitMeta) chooseClientForOp(op string, ctx Context) (redis.UniversalClient, string) {
	rawOp := op
	if ctx != nil {
		if method, ok := ctx.Value(txMethodKey{}).(string); ok && method != "" {
			rawOp = method
		}
	}
	normalized := normalizeOpName(rawOp)
	if normalized == "" {
		normalized = normalizeOpName(op)
	}
	if normalized == "" {
		normalized = "unknown"
	}
	if _, ok := readOps[normalized]; ok {
		if m.replica != nil {
			if m.replicaHealthy.Load() || m.attemptProbeReplica() {
				return m.replica, routeReasonRead
			}
		}
		return m.master, routeReasonReplicaUnavailable
	}
	if m.replica == nil {
		return m.master, routeReasonReplicaUnavailable
	}
	return m.master, routeReasonDefault
}

func normalizeOpName(op string) string {
	op = strings.TrimSpace(op)
	if idx := strings.Index(op, ":"); idx >= 0 {
		op = op[:idx]
	}
	op = strings.TrimRightFunc(op, func(r rune) bool { return unicode.IsDigit(r) })
	op = strings.TrimSpace(op)
	return strings.ToLower(op)
}

func (m *redisSplitMeta) Name() string {
	return "redis-split"
}

func (m *redisSplitMeta) doGetAttr(ctx Context, inode Ino, attr *Attr) syscall.Errno {
	client, usedReplica := m.clientForOp("doGetAttr", ctx)
	if client == nil {
		return syscall.EIO
	}
	data, err := client.Get(ctx, m.inodeKey(inode)).Bytes()
	if usedReplica && m.shouldFallbackToMaster(err) {
		m.markReplicaFailed(err)
		client = m.master
		data, err = client.Get(ctx, m.inodeKey(inode)).Bytes()
		usedReplica = false
	} else if usedReplica && err == nil {
		m.onReplicaSuccess()
	}
	if err == nil {
		m.parseAttr(data, attr)
	}
	return errno(err)
}

func (m *redisSplitMeta) clientForOp(op string, ctx Context) (redis.UniversalClient, bool) {
	client, _ := m.chooseClientForOp(op, ctx)
	return client, client != nil && client == m.replica
}

func (m *redisSplitMeta) shouldFallbackToMaster(err error) bool {
	if err == nil {
		return false
	}
	return err != redis.Nil
}

func (m *redisSplitMeta) markReplicaFailed(err error) {
	if m.replica == nil {
		return
	}
	now := time.Now()
	atomic.StoreInt64(&m.nextReplicaProbe, now.Add(m.healthCheckInterval).UnixNano())
	if m.replicaHealthy.Swap(false) {
		logger.Warnf("redis-split: replica %s unavailable: %v; routing reads to master", m.replicaURI, err)
	}
}

func (m *redisSplitMeta) onReplicaSuccess() {
	if m.replica == nil {
		return
	}
	if !m.replicaHealthy.Load() {
		m.replicaHealthy.Store(true)
		atomic.StoreInt64(&m.nextReplicaProbe, time.Now().Add(m.healthCheckInterval).UnixNano())
		logger.Infof("redis-split: replica %s recovered; routing reads back to replica", m.replicaURI)
	}
}

func (m *redisSplitMeta) attemptProbeReplica() bool {
	if m.replica == nil {
		return false
	}
	if m.replicaHealthy.Load() {
		return true
	}
	now := time.Now()
	next := atomic.LoadInt64(&m.nextReplicaProbe)
	if next != 0 && now.UnixNano() < next {
		return false
	}
	m.replicaProbeMu.Lock()
	defer m.replicaProbeMu.Unlock()
	if m.replicaHealthy.Load() {
		return true
	}
	next = atomic.LoadInt64(&m.nextReplicaProbe)
	if next != 0 && now.UnixNano() < next {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.replica.Ping(ctx).Err(); err == nil {
		m.replicaHealthy.Store(true)
		atomic.StoreInt64(&m.nextReplicaProbe, now.Add(m.healthCheckInterval).UnixNano())
		logger.Infof("redis-split: replica %s ping succeeded; resuming read routing", m.replicaURI)
		return true
	}
	atomic.StoreInt64(&m.nextReplicaProbe, now.Add(m.healthCheckInterval).UnixNano())
	return false
}

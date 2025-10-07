package meta

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestParseRedisSplitURL(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantMaster  string
		wantReplica string
		wantErr     bool
	}{
		{
			name:        "valid basic",
			input:       "redis-split://redis://10.0.0.1:6379/1?replica=redis://10.0.0.2:16379/1",
			wantMaster:  "redis://10.0.0.1:6379/1",
			wantReplica: "redis://10.0.0.2:16379/1",
		},
		{
			name:    "missing replica",
			input:   "redis-split://redis://10.0.0.1:6379/1",
			wantErr: true,
		},
		{
			name:    "missing master",
			input:   "redis-split://?replica=redis://10.0.0.2:16379/1",
			wantErr: true,
		},
		{
			name:        "additional params preserved",
			input:       "redis-split://redis://10.0.0.1:6379/1?replica=redis://10.0.0.2:16379/1&foo=bar",
			wantMaster:  "redis://10.0.0.1:6379/1",
			wantReplica: "redis://10.0.0.2:16379/1",
		},
	}

	for _, tt := range tests {
		master, replica, err := parseRedisSplitURL(tt.input)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", tt.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tt.name, err)
		}
		if master != tt.wantMaster {
			t.Fatalf("%s: master mismatch, want %q, got %q", tt.name, tt.wantMaster, master)
		}
		if replica != tt.wantReplica {
			t.Fatalf("%s: replica mismatch, want %q, got %q", tt.name, tt.wantReplica, replica)
		}
	}
}

func TestRegisterRedisSplit(t *testing.T) {
	fakeMaster := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6390"})
	fakeReplica := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6391"})
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		rm := &redisMeta{
			baseMeta: newBaseMeta(addr, conf),
			rdb:      fakeMaster,
		}
		rm.en = rm
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		return fakeReplica, nil
	})
	t.Cleanup(func() {
		_ = fakeMaster.Close()
		_ = fakeReplica.Close()
	})

	creator, ok := metaDrivers["redis-split"]
	if !ok {
		t.Fatalf("redis-split driver not registered")
	}
	conf := &Config{}
	meta, err := creator("redis-split", "redis://10.0.0.1:6379/1?replica=redis://10.0.0.2:16379/1", conf)
	if err != nil {
		t.Fatalf("unexpected error creating redis-split meta: %v", err)
	}
	if meta == nil {
		t.Fatalf("expected meta instance, got nil")
	}
	if _, ok := meta.(*redisSplitMeta); !ok {
		t.Fatalf("expected *redisSplitMeta, got %T", meta)
	}
}

func TestNewRedisSplitClients(t *testing.T) {
	conf := &Config{Retries: 2}
	fakeMaster := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6392"})
	fakeReplica := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6393"})
	var masterDriver, masterAddr, replicaURI string
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		masterDriver = driver
		masterAddr = addr
		rm := &redisMeta{
			baseMeta: newBaseMeta(addr, conf),
			rdb:      fakeMaster,
		}
		rm.en = rm
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		replicaURI = uri
		return fakeReplica, nil
	})
	t.Cleanup(func() {
		_ = fakeMaster.Close()
		_ = fakeReplica.Close()
	})

	meta, err := newRedisSplitMeta("redis-split", "redis://10.0.0.1:6379/0?replica=redis://10.0.0.2:6380/0", conf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	split, ok := meta.(*redisSplitMeta)
	if !ok {
		t.Fatalf("expected *redisSplitMeta, got %T", meta)
	}
	if masterDriver != "redis" {
		t.Fatalf("unexpected master driver: %s", masterDriver)
	}
	if masterAddr != "10.0.0.1:6379/0" {
		t.Fatalf("unexpected master addr: %s", masterAddr)
	}
	if replicaURI != "redis://10.0.0.2:6380/0" {
		t.Fatalf("unexpected replica uri: %s", replicaURI)
	}
	if split.master != fakeMaster {
		t.Fatalf("master client mismatch")
	}
	if split.replica != fakeReplica {
		t.Fatalf("replica client mismatch")
	}
}

func TestNewRedisSplitClients_InvalidReplica(t *testing.T) {
	conf := &Config{}
	fakeMaster := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6394"})
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		rm := &redisMeta{
			baseMeta: newBaseMeta(addr, conf),
			rdb:      fakeMaster,
		}
		rm.en = rm
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		return nil, fmt.Errorf("boom")
	})
	t.Cleanup(func() {
		_ = fakeMaster.Close()
	})

	if _, err := newRedisSplitMeta("redis-split", "redis://10.0.0.1:6379/0?replica=redis://10.0.0.2:6380/0", conf); err == nil {
		t.Fatalf("expected replica factory error")
	}
}

func TestNewRedisSplitMeta_InvalidURI(t *testing.T) {
	conf := &Config{}
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		t.Fatalf("master factory should not be called for invalid URI")
		return nil, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		t.Fatalf("replica factory should not be called for invalid URI")
		return nil, nil
	})
	if _, err := newRedisSplitMeta("redis-split", "redis://10.0.0.1:6379/1", conf); err == nil {
		t.Fatalf("expected error for missing replica")
	}
}

func TestTxnUsesMaster(t *testing.T) {
	conf := &Config{}
	split, master, replica := newTrackingRedisSplit(t, conf)
	ctx := Background().WithValue(txMethodKey{}, "txn")
	if err := split.txn(ctx, func(tx *redis.Tx) error { return nil }, "k1"); err != nil {
		t.Fatalf("txn returned error: %v", err)
	}
	if master.watchCalls != 1 {
		t.Fatalf("expected master Watch calls = 1, got %d", master.watchCalls)
	}
	if len(master.lastKeys) != 1 || master.lastKeys[0] != "k1" {
		t.Fatalf("unexpected master keys: %v", master.lastKeys)
	}
	if replica.watchCalls != 0 {
		t.Fatalf("replica should not receive Watch calls, got %d", replica.watchCalls)
	}
}

func TestChooseClient_ReadOpsToReplica(t *testing.T) {
	split, _, replica := newTestRedisSplit(t, &Config{})
	ctx := Background()
	for _, op := range []string{"doGetAttr", "doReaddir", "doReadlink"} {
		client, reason := split.chooseClientForOp(op, ctx)
		if client != replica {
			t.Fatalf("op %s: expected replica client", op)
		}
		if reason != routeReasonRead {
			t.Fatalf("op %s: expected reason %q, got %q", op, routeReasonRead, reason)
		}
	}
}

func TestChooseClient_WriteOpsToMaster(t *testing.T) {
	split, master, _ := newTestRedisSplit(t, &Config{})
	ctx := Background()
	for _, op := range []string{"doMknod", "doUnlink"} {
		client, reason := split.chooseClientForOp(op, ctx)
		if client != master {
			t.Fatalf("op %s: expected master client", op)
		}
		if reason != routeReasonWrite {
			t.Fatalf("op %s: expected reason %q, got %q", op, routeReasonWrite, reason)
		}
	}
}

func TestChooseClient_LockOpsToMaster(t *testing.T) {
	split, master, _ := newTestRedisSplit(t, &Config{})
	ctx := Background()
	for _, op := range []string{"Setlk", "Flock"} {
		client, reason := split.chooseClientForOp(op, ctx)
		if client != master {
			t.Fatalf("op %s: expected master client", op)
		}
		if reason != routeReasonLock {
			t.Fatalf("op %s: expected reason %q, got %q", op, routeReasonLock, reason)
		}
	}
}

func TestDoGetAttr_RoutesToReplica(t *testing.T) {
	ctx := Background()
	replica := &fakeRedisClient{}
	master := &fakeRedisClient{}
	split := newWrappedRedisSplit(t, master, replica)
	expected := &Attr{
		Typ:    TypeFile,
		Mode:   0644,
		Length: 123,
		Parent: 7,
	}
	replica.getFn = func(ctx context.Context, key string) *redis.StringCmd {
		cmd := redis.NewStringCmd(ctx)
		cmd.SetVal(string(split.marshal(expected)))
		return cmd
	}

	attr := &Attr{}
	if st := split.doGetAttr(ctx, 1, attr); st != 0 {
		t.Fatalf("unexpected status: %d", st)
	}
	if replica.getCalls != 1 || master.getCalls != 0 {
		t.Fatalf("expected replica getCalls=1, master=0; got %d and %d", replica.getCalls, master.getCalls)
	}
	if !attr.Full || attr.Mode != expected.Mode || attr.Typ != expected.Typ || attr.Length != expected.Length || attr.Parent != expected.Parent {
		t.Fatalf("unexpected attr: %+v", attr)
	}
}

func TestDoMknod_RoutesToMaster(t *testing.T) {
	ctx := Background()
	master := &fakeRedisClient{}
	replica := &fakeRedisClient{}
	replica.watchFn = func(context.Context, func(*redis.Tx) error, ...string) error {
		t.Fatalf("replica Watch should not be called")
		return nil
	}
	split := newWrappedRedisSplit(t, master, replica)
	master.watchFn = func(ctx context.Context, fn func(*redis.Tx) error, keys ...string) error {
		return syscall.ENOTDIR
	}

	var ino Ino
	attr := &Attr{}
	st := split.doMknod(ctx, 1, "child", TypeFile, 0644, 0, "", &ino, attr)
	if st != syscall.ENOTDIR {
		t.Fatalf("expected status ENOTDIR, got %v", st)
	}
	if master.watchCalls != 1 {
		t.Fatalf("expected master watchCalls=1, got %d", master.watchCalls)
	}
	if replica.watchCalls != 0 {
		t.Fatalf("expected replica watchCalls=0, got %d", replica.watchCalls)
	}
}

func TestReplicaUnavailable_FallbackToMaster(t *testing.T) {
	ctx := Background()
	master := &fakeRedisClient{}
	replica := &fakeRedisClient{}
	split := newWrappedRedisSplit(t, master, replica)
	split.healthCheckInterval = time.Millisecond * 10

	expected := &Attr{Typ: TypeDirectory, Mode: 0755, Parent: RootInode}
	master.getFn = func(ctx context.Context, key string) *redis.StringCmd {
		cmd := redis.NewStringCmd(ctx)
		cmd.SetVal(string(split.marshal(expected)))
		return cmd
	}
	replica.getFn = func(ctx context.Context, key string) *redis.StringCmd {
		cmd := redis.NewStringCmd(ctx)
		cmd.SetErr(errors.New("dial error"))
		return cmd
	}

	attr := &Attr{}
	if st := split.doGetAttr(ctx, RootInode, attr); st != 0 {
		t.Fatalf("doGetAttr fallback unexpected status: %v", st)
	}
	if master.getCalls != 1 || replica.getCalls != 1 {
		t.Fatalf("expected master=1 replica=1 calls, got master=%d replica=%d", master.getCalls, replica.getCalls)
	}
	if split.replicaHealthy.Load() {
		t.Fatalf("replica should be marked unhealthy after failure")
	}

	replica.getFn = func(ctx context.Context, key string) *redis.StringCmd {
		cmd := redis.NewStringCmd(ctx)
		cmd.SetVal(string(split.marshal(expected)))
		return cmd
	}
	replica.pingFn = func(ctx context.Context) *redis.StatusCmd {
		cmd := redis.NewStatusCmd(ctx)
		cmd.SetVal("PONG")
		return cmd
	}
	atomic.StoreInt64(&split.nextReplicaProbe, time.Now().Add(-time.Millisecond).UnixNano())

	master.getCalls = 0
	replica.getCalls = 0

	if st := split.doGetAttr(ctx, RootInode, attr); st != 0 {
		t.Fatalf("doGetAttr after recovery unexpected status: %v", st)
	}
	if replica.getCalls == 0 {
		t.Fatalf("expected replica to serve read after recovery")
	}
	if master.getCalls != 0 {
		t.Fatalf("expected master not to be used after replica recovery, got %d", master.getCalls)
	}
	if !split.replicaHealthy.Load() {
		t.Fatalf("replica should be healthy after successful ping")
	}
}

func stubRedisSplitFactories(t *testing.T, master func(string, string, *Config) (Meta, error), replica func(string, *Config) (redis.UniversalClient, error)) {
	oldMaster := redisSplitMasterFactory
	oldReplica := redisSplitReplicaFactory
	if master != nil {
		redisSplitMasterFactory = master
	}
	if replica != nil {
		redisSplitReplicaFactory = replica
	}
	t.Cleanup(func() {
		redisSplitMasterFactory = oldMaster
		redisSplitReplicaFactory = oldReplica
	})
}

func newTestRedisSplit(t *testing.T, conf *Config) (*redisSplitMeta, redis.UniversalClient, redis.UniversalClient) {
	if conf == nil {
		conf = &Config{}
	}
	conf.Retries = 1
	fakeMaster := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6400"})
	fakeReplica := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6401"})
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		rm := &redisMeta{
			baseMeta: newBaseMeta(addr, conf),
			rdb:      fakeMaster,
		}
		rm.en = rm
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		return fakeReplica, nil
	})
	t.Cleanup(func() {
		_ = fakeMaster.Close()
		_ = fakeReplica.Close()
	})
	meta, err := newRedisSplitMeta("redis-split", "redis://10.0.0.1:6379/0?replica=redis://10.0.0.2:6380/0", conf)
	if err != nil {
		t.Fatalf("unexpected error creating redis-split meta: %v", err)
	}
	split, ok := meta.(*redisSplitMeta)
	if !ok {
		t.Fatalf("expected *redisSplitMeta, got %T", meta)
	}
	return split, fakeMaster, fakeReplica
}

type trackingClient struct {
	redis.UniversalClient
	watchCalls int
	lastKeys   []string
	pingFn     func(context.Context) *redis.StatusCmd
}

func (c *trackingClient) Watch(ctx context.Context, fn func(*redis.Tx) error, keys ...string) error {
	c.watchCalls++
	c.lastKeys = append([]string(nil), keys...)
	if fn != nil {
		return fn(&redis.Tx{})
	}
	return nil
}

func (c *trackingClient) Close() error { return nil }

func (c *trackingClient) Ping(ctx context.Context) *redis.StatusCmd {
	if c.pingFn != nil {
		return c.pingFn(ctx)
	}
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetVal("PONG")
	return cmd
}

func newTrackingRedisSplit(t *testing.T, conf *Config) (*redisSplitMeta, *trackingClient, *trackingClient) {
	if conf == nil {
		conf = &Config{}
	}
	master := &trackingClient{}
	replica := &trackingClient{}
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		rm := &redisMeta{
			baseMeta: newBaseMeta(addr, conf),
			rdb:      master,
		}
		rm.en = rm
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		return replica, nil
	})
	meta, err := newRedisSplitMeta("redis-split", "redis://10.0.0.1:6379/0?replica=redis://10.0.0.2:6380/0", conf)
	if err != nil {
		t.Fatalf("unexpected error creating redis-split meta: %v", err)
	}
	split, ok := meta.(*redisSplitMeta)
	if !ok {
		t.Fatalf("expected *redisSplitMeta, got %T", meta)
	}
	return split, master, replica
}

type fakeRedisClient struct {
	redis.UniversalClient
	getFn         func(context.Context, string) *redis.StringCmd
	getCalls      int
	watchFn       func(context.Context, func(*redis.Tx) error, ...string) error
	watchCalls    int
	lastWatchKeys []string
	pingFn        func(context.Context) *redis.StatusCmd
	pingCalls     int
}

func (c *fakeRedisClient) Get(ctx context.Context, key string) *redis.StringCmd {
	c.getCalls++
	if c.getFn != nil {
		return c.getFn(ctx, key)
	}
	cmd := redis.NewStringCmd(ctx)
	cmd.SetErr(errors.New("not implemented"))
	return cmd
}

func (c *fakeRedisClient) Watch(ctx context.Context, fn func(*redis.Tx) error, keys ...string) error {
	c.watchCalls++
	c.lastWatchKeys = append([]string(nil), keys...)
	if c.watchFn != nil {
		return c.watchFn(ctx, fn, keys...)
	}
	return errors.New("watch not implemented")
}

func (c *fakeRedisClient) Close() error { return nil }

func (c *fakeRedisClient) Ping(ctx context.Context) *redis.StatusCmd {
	c.pingCalls++
	if c.pingFn != nil {
		return c.pingFn(ctx)
	}
	cmd := redis.NewStatusCmd(ctx)
	cmd.SetErr(errors.New("ping not implemented"))
	return cmd
}

func newWrappedRedisSplit(t *testing.T, master redis.UniversalClient, replica redis.UniversalClient) *redisSplitMeta {
	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		rm := &redisMeta{
			baseMeta: newBaseMeta(addr, conf),
			rdb:      master,
		}
		rm.en = rm
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		return replica, nil
	})
	meta, err := newRedisSplitMeta("redis-split", "redis://10.0.0.1:6379/0?replica=redis://10.0.0.2:6380/0", &Config{})
	if err != nil {
		t.Fatalf("unexpected error creating redis-split meta: %v", err)
	}
	split, ok := meta.(*redisSplitMeta)
	if !ok {
		t.Fatalf("expected *redisSplitMeta, got %T", meta)
	}
	split.master = master
	split.replica = replica
	if replica != nil {
		split.replicaHealthy.Store(true)
	}
	return split
}

package meta

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	defaultRedisSplitMaster  = "redis://100.123.245.11:6379/1"
	defaultRedisSplitReplica = "redis://100.93.213.27:16379/1"
)

func TestRedisSplitIntegration_BasicRouting(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping redis-split integration test in short mode")
	}

	masterURL := integrationEndpoint("JFS_REDIS_SPLIT_MASTER", defaultRedisSplitMaster)
	replicaURL := integrationEndpoint("JFS_REDIS_SPLIT_REPLICA", defaultRedisSplitReplica)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	masterClient, masterHook := newInstrumentedRedisClient(t, masterURL)
	defer masterClient.Close()
	replicaClient, replicaHook := newInstrumentedRedisClient(t, replicaURL)
	defer replicaClient.Close()

	if err := masterClient.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush master db: %v", err)
	}
	time.Sleep(500 * time.Millisecond)

	masterHook.Reset()
	replicaHook.Reset()

	stubRedisSplitFactories(t, func(driver, addr string, conf *Config) (Meta, error) {
		meta, err := newRedisMeta(driver, addr, conf)
		if err != nil {
			return nil, err
		}
		rm, ok := meta.(*redisMeta)
		if !ok {
			return nil, fmt.Errorf("unexpected master meta type %T", meta)
		}
		_ = rm.rdb.Close()
		rm.rdb = masterClient
		return rm, nil
	}, func(uri string, conf *Config) (redis.UniversalClient, error) {
		return replicaClient, nil
	})

	conf := DefaultConf()
	conf.Retries = 2
	addr := fmt.Sprintf("%s?replica=%s", masterURL, replicaURL)
	meta, err := newRedisSplitMeta("redis-split", addr, conf)
	if err != nil {
		t.Fatalf("create redis-split meta: %v", err)
	}
	split, ok := meta.(*redisSplitMeta)
	if !ok {
		t.Fatalf("expected *redisSplitMeta, got %T", meta)
	}
	split.master = masterClient
	split.replica = replicaClient
	if replicaClient != nil {
		split.replicaHealthy.Store(true)
	}
	defer func() {
		_ = split.Reset()
		_ = split.Shutdown()
	}()

	if err := split.Reset(); err != nil {
		t.Fatalf("reset meta: %v", err)
	}

	format := &Format{Name: fmt.Sprintf("redis-split-integration-%d", time.Now().UnixNano())}
	if err := split.Init(format, true); err != nil {
		t.Fatalf("init meta: %v", err)
	}
	if err := split.NewSession(true); err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer split.CloseSession()

	ctxMeta := Background()

	masterHook.Reset()
	replicaHook.Reset()

	var dir Ino
	var attr Attr
	if st := split.Mkdir(ctxMeta, RootInode, "integration", 0755, 0, 0, &dir, &attr); st != 0 {
		t.Fatalf("mkdir on master failed: %v", st)
	}

	if masterHook.Count("watch") == 0 && masterHook.Count("multi") == 0 {
		t.Fatalf("expected master to handle write, counts: %v", masterHook.Snapshot())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for replica getattr; master=%v replica=%v", masterHook.Snapshot(), replicaHook.Snapshot())
		}
		masterHook.Reset()
		replicaHook.Reset()

		st := split.GetAttr(ctxMeta, dir, &attr)
		if st != 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if replicaHook.Count("get") == 0 {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if masterHook.Count("get") != 0 {
			t.Fatalf("expected no master get during getattr, master=%v replica=%v", masterHook.Snapshot(), replicaHook.Snapshot())
		}
		break
	}

	if st := split.Rmdir(ctxMeta, RootInode, "integration"); st != 0 {
		t.Fatalf("cleanup directory failed: %v", st)
	}
}

func integrationEndpoint(envKey, fallback string) string {
	if val := strings.TrimSpace(os.Getenv(envKey)); val != "" {
		return val
	}
	return fallback
}

func newInstrumentedRedisClient(t *testing.T, raw string) (*redis.Client, *countingHook) {
	t.Helper()
	opt, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("parse redis url %q: %v", raw, err)
	}
	if opt.DialTimeout == 0 {
		opt.DialTimeout = 3 * time.Second
	}
	if opt.ReadTimeout == 0 {
		opt.ReadTimeout = 5 * time.Second
	}
	if opt.WriteTimeout == 0 {
		opt.WriteTimeout = 5 * time.Second
	}
	client := redis.NewClient(opt)
	hook := newCountingHook()
	client.AddHook(hook)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Skipf("skip redis-split integration: ping %s failed: %v", raw, err)
	}
	return client, hook
}

type countingHook struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingHook() *countingHook {
	return &countingHook{counts: make(map[string]int)}
}

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.add(cmd.Name())
		return next(ctx, cmd)
	}
}

func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			h.add(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (h *countingHook) add(name string) {
	h.mu.Lock()
	h.counts[strings.ToLower(name)]++
	h.mu.Unlock()
}

func (h *countingHook) Count(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[strings.ToLower(name)]
}

func (h *countingHook) Snapshot() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int, len(h.counts))
	for k, v := range h.counts {
		out[k] = v
	}
	return out
}

func (h *countingHook) Reset() {
	h.mu.Lock()
	h.counts = make(map[string]int)
	h.mu.Unlock()
}

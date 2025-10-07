package meta

import (
	"fmt"
	"testing"

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

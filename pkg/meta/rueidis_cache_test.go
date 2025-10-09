package meta

import (
	"testing"
	"time"
)

func TestRueidisDefaultCacheTTL(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/7"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	if rm.cacheTTL != 100*time.Millisecond {
		t.Errorf("Expected default cache TTL to be 100ms, got %v", rm.cacheTTL)
	}
}

func TestRueidisCustomCacheTTL(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/8?cache-ttl=5s"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	if rm.cacheTTL != 5*time.Second {
		t.Errorf("Expected cache TTL to be 5s, got %v", rm.cacheTTL)
	}
}

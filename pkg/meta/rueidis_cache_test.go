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
	expectedTTL := 14 * 24 * time.Hour // 2 weeks
	if rm.cacheTTL != expectedTTL {
		t.Errorf("Expected default cache TTL to be 2 weeks (%v), got %v", expectedTTL, rm.cacheTTL)
	}

	// Verify broadcast mode is enabled
	if len(rm.option.ClientTrackingOptions) == 0 {
		t.Error("Expected ClientTrackingOptions to be configured for broadcast mode")
	}

	// Check for BCAST option
	hasBcast := false
	for _, opt := range rm.option.ClientTrackingOptions {
		if opt == "BCAST" {
			hasBcast = true
			break
		}
	}
	if !hasBcast {
		t.Error("Expected BCAST option in ClientTrackingOptions")
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

	// Broadcast mode should still be enabled even with custom TTL
	hasBcast := false
	for _, opt := range rm.option.ClientTrackingOptions {
		if opt == "BCAST" {
			hasBcast = true
			break
		}
	}
	if !hasBcast {
		t.Error("Expected BCAST option in ClientTrackingOptions even with custom TTL")
	}
}

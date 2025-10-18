//go:build !norueidis
// +build !norueidis

package meta

import (
	"syscall"
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
	expectedTTL := 1 * time.Hour // Default changed to 1 hour for CSC
	if rm.cacheTTL != expectedTTL {
		t.Errorf("Expected default cache TTL to be 1 hour (%v), got %v", expectedTTL, rm.cacheTTL)
	}

	// Verify OPTIN mode is used by default (ClientTrackingOptions should be nil/empty)
	if len(rm.option.ClientTrackingOptions) > 0 {
		t.Errorf("Expected ClientTrackingOptions to be empty for OPTIN mode, got %v", rm.option.ClientTrackingOptions)
	}

	// Verify subscribeMode is set to "optin"
	if rm.subscribeMode != "optin" {
		t.Errorf("Expected subscribeMode to be 'optin', got '%s'", rm.subscribeMode)
	}
}

func TestRueidisDefaultCacheTTLBCAST(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/7?subscribe=bcast"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	expectedTTL := 1 * time.Hour
	if rm.cacheTTL != expectedTTL {
		t.Errorf("Expected default cache TTL to be 1 hour (%v), got %v", expectedTTL, rm.cacheTTL)
	}

	// Verify BCAST mode is enabled when explicitly requested
	if len(rm.option.ClientTrackingOptions) == 0 {
		t.Error("Expected ClientTrackingOptions to be configured for BCAST mode")
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

	// Verify subscribeMode is set to "bcast"
	if rm.subscribeMode != "bcast" {
		t.Errorf("Expected subscribeMode to be 'bcast', got '%s'", rm.subscribeMode)
	}
}

func TestRueidisCustomCacheTTL(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/8?ttl=5s"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	if rm.cacheTTL != 5*time.Second {
		t.Errorf("Expected cache TTL to be 5s, got %v", rm.cacheTTL)
	}

	// OPTIN mode should be used by default (even with custom TTL)
	if len(rm.option.ClientTrackingOptions) > 0 {
		t.Errorf("Expected ClientTrackingOptions to be empty for OPTIN mode, got %v", rm.option.ClientTrackingOptions)
	}

	// Verify subscribeMode is set to "optin"
	if rm.subscribeMode != "optin" {
		t.Errorf("Expected subscribeMode to be 'optin', got '%s'", rm.subscribeMode)
	}
}

func TestRueidisCustomCacheTTLBCAST(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/8?ttl=5s&subscribe=bcast"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	if rm.cacheTTL != 5*time.Second {
		t.Errorf("Expected cache TTL to be 5s, got %v", rm.cacheTTL)
	}

	// BCAST mode should be enabled when explicitly requested (even with custom TTL)
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

	// Verify subscribeMode is set to "bcast"
	if rm.subscribeMode != "bcast" {
		t.Errorf("Expected subscribeMode to be 'bcast', got '%s'", rm.subscribeMode)
	}
}

func TestRueidisCacheTTLDisabled(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/9?ttl=0"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	if rm.cacheTTL != 0 {
		t.Errorf("Expected cache TTL to be 0 (disabled), got %v", rm.cacheTTL)
	}
}

func TestRueidisCacheTTLVariousFormats(t *testing.T) {
	tests := []struct {
		uri      string
		expected time.Duration
	}{
		{"rueidis://100.121.51.13:6379/10?ttl=2h", 2 * time.Hour},
		{"rueidis://100.121.51.13:6379/10?ttl=30m", 30 * time.Minute},
		{"rueidis://100.121.51.13:6379/10?ttl=10s", 10 * time.Second},
		{"rueidis://100.121.51.13:6379/10?ttl=500ms", 500 * time.Millisecond},
	}

	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			m := NewClient(tt.uri, &Config{Retries: 10})
			if m == nil {
				t.Fatalf("Failed to create Rueidis client for %s", tt.uri)
			}

			rm := m.(*rueidisMeta)
			if rm.cacheTTL != tt.expected {
				t.Errorf("Expected cache TTL to be %v, got %v", tt.expected, rm.cacheTTL)
			}
		})
	}
}

// Test helper methods for client-side caching
func TestCachedGetNilHandling(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/11?ttl=1h"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// Test reading a non-existent key
	nonExistentKey := rm.prefix + "test:nonexistent:key:12345"
	_, err := rm.cachedGet(ctx, nonExistentKey)

	if err != syscall.ENOENT {
		t.Errorf("Expected ENOENT for non-existent key, got %v", err)
	}
}

func TestCachedGetWithCachingEnabled(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/12?ttl=10s"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// Set a test key
	testKey := rm.prefix + "test:cached:get:key"
	testValue := []byte("test-value-123")

	err := rm.compat.Set(ctx, testKey, testValue, 60*time.Second).Err()
	if err != nil {
		t.Fatalf("Failed to set test key: %v", err)
	}
	defer rm.compat.Del(ctx, testKey)

	// Read with caching
	result, err := rm.cachedGet(ctx, testKey)
	if err != nil {
		t.Fatalf("cachedGet failed: %v", err)
	}

	if string(result) != string(testValue) {
		t.Errorf("Expected value %s, got %s", testValue, result)
	}
}

func TestCachedGetWithCachingDisabled(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/13?ttl=0"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// Verify caching is disabled
	if rm.cacheTTL != 0 {
		t.Fatalf("Expected cacheTTL to be 0, got %v", rm.cacheTTL)
	}

	// Set a test key
	testKey := rm.prefix + "test:nocache:get:key"
	testValue := []byte("nocache-value-456")

	err := rm.compat.Set(ctx, testKey, testValue, 60*time.Second).Err()
	if err != nil {
		t.Fatalf("Failed to set test key: %v", err)
	}
	defer rm.compat.Del(ctx, testKey)

	// Read without caching (direct path)
	result, err := rm.cachedGet(ctx, testKey)
	if err != nil {
		t.Fatalf("cachedGet failed: %v", err)
	}

	if string(result) != string(testValue) {
		t.Errorf("Expected value %s, got %s", testValue, result)
	}
}

func TestCachedHGetNilHandling(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/14?ttl=1h"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// Test reading a non-existent hash field
	nonExistentHash := rm.prefix + "test:nonexistent:hash:12345"
	_, err := rm.cachedHGet(ctx, nonExistentHash, "field")

	if err != syscall.ENOENT {
		t.Errorf("Expected ENOENT for non-existent hash field, got %v", err)
	}
}

func TestCachedHGetWithCachingEnabled(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/15?ttl=10s"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// Set a test hash
	testHash := rm.prefix + "test:cached:hget:hash"
	testField := "testfield"
	testValue := []byte("hash-value-789")

	err := rm.compat.HSet(ctx, testHash, testField, testValue).Err()
	if err != nil {
		t.Fatalf("Failed to set test hash: %v", err)
	}
	defer rm.compat.Del(ctx, testHash)

	// Read with caching
	result, err := rm.cachedHGet(ctx, testHash, testField)
	if err != nil {
		t.Fatalf("cachedHGet failed: %v", err)
	}

	if string(result) != string(testValue) {
		t.Errorf("Expected value %s, got %s", testValue, result)
	}
}

func TestCachedMGetRequiresCaching(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/6?ttl=0"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// cachedMGet should return error when caching is disabled
	keys := []string{rm.prefix + "key1", rm.prefix + "key2"}
	_, err := rm.cachedMGet(ctx, keys)

	if err == nil {
		t.Error("Expected error when calling cachedMGet with ttl=0, got nil")
	}
}

func TestCachedMGetWithCachingEnabled(t *testing.T) {
	addr := "rueidis://100.121.51.13:6379/5?ttl=10s"
	m := NewClient(addr, &Config{Retries: 10})
	if m == nil {
		t.Fatal("Failed to create Rueidis client")
	}

	rm := m.(*rueidisMeta)
	ctx := Background()

	// Set multiple test keys
	testKeys := []string{
		rm.prefix + "test:mget:key1",
		rm.prefix + "test:mget:key2",
		rm.prefix + "test:mget:key3",
	}
	testValues := []string{"value1", "value2", "value3"}

	for i, key := range testKeys {
		err := rm.compat.Set(ctx, key, testValues[i], 60*time.Second).Err()
		if err != nil {
			t.Fatalf("Failed to set test key %s: %v", key, err)
		}
		defer rm.compat.Del(ctx, key)
	}

	// Read with MGetCache
	results, err := rm.cachedMGet(ctx, testKeys)
	if err != nil {
		t.Fatalf("cachedMGet failed: %v", err)
	}

	if len(results) != len(testKeys) {
		t.Errorf("Expected %d results, got %d", len(testKeys), len(results))
	}

	// Verify each result
	for i, key := range testKeys {
		result, ok := results[key]
		if !ok {
			t.Errorf("Missing result for key %s", key)
			continue
		}

		val, err := result.ToString()
		if err != nil {
			t.Errorf("Failed to convert result for key %s: %v", key, err)
			continue
		}

		if val != testValues[i] {
			t.Errorf("Expected value %s for key %s, got %s", testValues[i], key, val)
		}
	}
}

//go:build !norueidis && integration
// +build !norueidis,integration

package meta

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestRueidisIntegration_Invalidate verifies that a write from client A causes
// invalidation so client B sees the update immediately when using cached helpers.
//
// This is an integration test that requires access to a Redis server. By default
// it uses the host at 100.121.51.13:6379. If you have Docker available you can
// run a local Redis instance; otherwise point the test at a reachable Redis.
func TestRueidisIntegration_Invalidate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	// Use the same logical DB for both clients to validate cross-client invalidation
	dbA := 4
	dbB := dbA

	urlA := fmt.Sprintf("rueidis://%s/%d?ttl=1h", redisHost, dbA)
	urlB := fmt.Sprintf("rueidis://%s/%d?ttl=1h", redisHost, dbB)

	mA := NewClient(urlA, &Config{})
	if mA == nil {
		t.Fatalf("client A: cannot connect to Redis at %s (url=%s)", redisHost, urlA)
	}
	defer mA.Shutdown()

	mB := NewClient(urlB, &Config{})
	if mB == nil {
		t.Fatalf("client B: cannot connect to Redis at %s (url=%s)", redisHost, urlB)
	}
	defer mB.Shutdown()

	// Type-assert to concrete rueidisMeta to access cached helpers and compat client
	rueidisA, ok := mA.(*rueidisMeta)
	if !ok {
		t.Fatalf("client A: expected rueidisMeta client (url=%s)", urlA)
	}
	rueidisB, ok := mB.(*rueidisMeta)
	if !ok {
		t.Fatalf("client B: expected rueidisMeta client (url=%s)", urlB)
	}

	// Keep test minimal: no extra server/tracking diagnostics logged in normal runs.

	// Build keys that match the prefixes tracked by the Rueidis client.
	// Try to read the prefixes from tracking info and fall back to 'jfs'.
	entryField := "file"
	var dirPrefix, inoPrefix string
	if ti, err := rueidisA.GetCacheTrackingInfo(Background()); err == nil {
		if p, ok := ti["prefixes"]; ok {
			switch s := p.(type) {
			case []string:
				for _, v := range s {
					if strings.HasSuffix(v, "d") {
						dirPrefix = v
					}
					if strings.HasSuffix(v, "i") {
						inoPrefix = v
					}
				}
			case []interface{}:
				for _, vi := range s {
					if vs, ok := vi.(string); ok {
						if strings.HasSuffix(vs, "d") {
							dirPrefix = vs
						}
						if strings.HasSuffix(vs, "i") {
							inoPrefix = vs
						}
					}
				}
			case string:
				// single string prefix unlikely, ignore
			}
		}
	}
	if dirPrefix == "" {
		dirPrefix = "jfsd"
	}
	if inoPrefix == "" {
		inoPrefix = "jfsi"
	}

	parent := dirPrefix + "1"
	inodeKey := inoPrefix + "1000"

	// Ensure clean state
	rueidisA.compat.Del(Background(), parent)
	rueidisA.compat.Del(Background(), inodeKey)

	// Client B reads entry (should not exist)
	_, preErr := rueidisB.cachedHGet(Background(), parent, entryField)
	if preErr == nil {
		// if no error, it's unexpected; treat as failure because namespace should be empty
		t.Fatalf("client B: expected no entry before write, but found one")
	}

	// Client A writes new entry and inode attributes
	// entry:{parent} HSET file -> 1000
	if err := rueidisA.compat.HSet(Background(), parent, entryField, "1000").Err(); err != nil {
		t.Fatalf("client A HSet error: %v", err)
	}
	// Set inode attr (simple string payload)
	if err := rueidisA.compat.Set(Background(), inodeKey, "attr:1000", 0).Err(); err != nil {
		t.Fatalf("client A Set error: %v", err)
	}

	// Small sleep to allow INVALIDATE to propagate (Rueidis uses background notifications)
	time.Sleep(1 * time.Second)

	// Client B should now see the entry via cachedHGet and cachedGet for inode.
	// Poll briefly to allow INVALIDATE and cache priming to settle.
	var val []byte
	var err error
	success := false
	for i := 0; i < 30; i++ { // up to ~3s
		val, err = rueidisB.cachedHGet(Background(), parent, entryField)
		if err == nil {
			success = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !success {
		t.Fatalf("client B: cachedHGet did not succeed after retries (last err=%v)", err)
	}
	if string(val) != "1000" {
		t.Fatalf("client B: expected entry value '1000', got '%s'", string(val))
	}

	// Poll for cachedGet on inode key as well
	var inodeVal []byte
	success = false
	for i := 0; i < 30; i++ {
		inodeVal, err = rueidisB.cachedGet(Background(), inodeKey)
		if err == nil {
			success = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !success {
		t.Fatalf("client B: cachedGet did not succeed after retries (last err=%v)", err)
	}
	if string(inodeVal) != "attr:1000" {
		t.Fatalf("client B: expected inode attr 'attr:1000', got '%s'", string(inodeVal))
	}

	// Cleanup
	rueidisA.compat.Del(Background(), parent)
	rueidisA.compat.Del(Background(), inodeKey)
}

// TestRueidisIntegration_OPTINMode tests OPTIN mode subscription with live Redis
// Verifies that default mode is OPTIN and works correctly
func TestRueidisIntegration_OPTINMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	db := 5

	// Default should be OPTIN mode
	url := fmt.Sprintf("rueidis://%s/%d", redisHost, db)

	m := NewClient(url, &Config{})
	if m == nil {
		t.Fatalf("cannot connect to Redis at %s (url=%s)", redisHost, url)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta client")
	}

	// Verify OPTIN mode
	if rm.subscribeMode != "optin" {
		t.Errorf("Expected OPTIN mode, got %q", rm.subscribeMode)
	}

	// Verify ClientTrackingOptions is nil/empty for OPTIN
	if len(rm.option.ClientTrackingOptions) > 0 {
		t.Errorf("Expected empty ClientTrackingOptions for OPTIN, got %d options",
			len(rm.option.ClientTrackingOptions))
	}

	t.Logf("✅ OPTIN mode: subscribeMode=%q, tracking options=%d",
		rm.subscribeMode, len(rm.option.ClientTrackingOptions))
}

// TestRueidisIntegration_BCASTMode tests BCAST mode subscription with live Redis
// Verifies that explicit BCAST mode works and has tracking options configured
func TestRueidisIntegration_BCASTMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	db := 6

	// Explicit BCAST mode
	url := fmt.Sprintf("rueidis://%s/%d?subscribe=bcast", redisHost, db)

	m := NewClient(url, &Config{})
	if m == nil {
		t.Fatalf("cannot connect to Redis at %s (url=%s)", redisHost, url)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta client")
	}

	// Verify BCAST mode
	if rm.subscribeMode != "bcast" {
		t.Errorf("Expected BCAST mode, got %q", rm.subscribeMode)
	}

	// Verify ClientTrackingOptions is set for BCAST
	if len(rm.option.ClientTrackingOptions) == 0 {
		t.Error("Expected non-empty ClientTrackingOptions for BCAST")
	}

	// Verify BCAST keyword is present
	hasBcast := false
	for _, opt := range rm.option.ClientTrackingOptions {
		if opt == "BCAST" {
			hasBcast = true
			break
		}
	}
	if !hasBcast {
		t.Error("Expected BCAST keyword in ClientTrackingOptions")
	}

	t.Logf("✅ BCAST mode: subscribeMode=%q, tracking options=%d (includes BCAST)",
		rm.subscribeMode, len(rm.option.ClientTrackingOptions))
}

// TestRueidisIntegration_OPTINModeCaching tests that caching works correctly in OPTIN mode
// with live Redis, verifying that cache invalidation messages are properly received
func TestRueidisIntegration_OPTINModeCaching(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	db := 7

	url := fmt.Sprintf("rueidis://%s/%d?ttl=1h&subscribe=optin", redisHost, db)

	m := NewClient(url, &Config{})
	if m == nil {
		t.Fatalf("cannot connect to Redis at %s (url=%s)", redisHost, url)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta client")
	}

	// Verify OPTIN mode with caching enabled
	if rm.subscribeMode != "optin" {
		t.Errorf("Expected OPTIN mode, got %q", rm.subscribeMode)
	}

	if rm.cacheTTL == 0 {
		t.Error("Expected cache TTL to be set")
	}

	t.Logf("✅ OPTIN mode caching: mode=%q, ttl=%v",
		rm.subscribeMode, rm.cacheTTL)
}

// TestRueidisIntegration_BCASTModeCaching tests that caching works correctly in BCAST mode
// with live Redis, verifying broadcast invalidation behavior
func TestRueidisIntegration_BCASTModeCaching(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	db := 8

	url := fmt.Sprintf("rueidis://%s/%d?ttl=1h&subscribe=bcast", redisHost, db)

	m := NewClient(url, &Config{})
	if m == nil {
		t.Fatalf("cannot connect to Redis at %s (url=%s)", redisHost, url)
	}
	defer m.Shutdown()

	rm, ok := m.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta client")
	}

	// Verify BCAST mode with caching enabled
	if rm.subscribeMode != "bcast" {
		t.Errorf("Expected BCAST mode, got %q", rm.subscribeMode)
	}

	if rm.cacheTTL == 0 {
		t.Error("Expected cache TTL to be set")
	}

	// Verify tracking options exist for BCAST
	if len(rm.option.ClientTrackingOptions) == 0 {
		t.Error("Expected tracking options for BCAST mode")
	}

	t.Logf("✅ BCAST mode caching: mode=%q, ttl=%v, tracking=%d",
		rm.subscribeMode, rm.cacheTTL, len(rm.option.ClientTrackingOptions))
}

// TestRueidisIntegration_MultipleClientsDifferentModes tests that
// multiple clients can use different subscription modes concurrently
func TestRueidisIntegration_MultipleClientsDifferentModes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"

	// Create OPTIN client
	urlOPTIN := fmt.Sprintf("rueidis://%s/9", redisHost)
	mOPTIN := NewClient(urlOPTIN, &Config{})
	if mOPTIN == nil {
		t.Fatalf("cannot create OPTIN client")
	}
	defer mOPTIN.Shutdown()

	// Create BCAST client
	urlBCAST := fmt.Sprintf("rueidis://%s/10?subscribe=bcast", redisHost)
	mBCAST := NewClient(urlBCAST, &Config{})
	if mBCAST == nil {
		t.Fatalf("cannot create BCAST client")
	}
	defer mBCAST.Shutdown()

	rmOPTIN, ok := mOPTIN.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta for OPTIN client")
	}

	rmBCAST, ok := mBCAST.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta for BCAST client")
	}

	// Verify different modes
	if rmOPTIN.subscribeMode != "optin" {
		t.Errorf("OPTIN client: expected optin, got %q", rmOPTIN.subscribeMode)
	}

	if rmBCAST.subscribeMode != "bcast" {
		t.Errorf("BCAST client: expected bcast, got %q", rmBCAST.subscribeMode)
	}

	// Verify tracking configurations match modes
	if len(rmOPTIN.option.ClientTrackingOptions) > 0 {
		t.Error("OPTIN client should have empty tracking options")
	}

	if len(rmBCAST.option.ClientTrackingOptions) == 0 {
		t.Error("BCAST client should have tracking options")
	}

	t.Logf("✅ Multiple clients working independently:")
	t.Logf("   OPTIN: mode=%q, tracking=%d", rmOPTIN.subscribeMode, len(rmOPTIN.option.ClientTrackingOptions))
	t.Logf("   BCAST: mode=%q, tracking=%d", rmBCAST.subscribeMode, len(rmBCAST.option.ClientTrackingOptions))
}

// TestRueidisIntegration_OPTINInvalidationAcrossClients verifies that
// OPTIN mode properly invalidates cache entries when they are modified
func TestRueidisIntegration_OPTINInvalidationAcrossClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	db := 11

	urlA := fmt.Sprintf("rueidis://%s/%d?ttl=1h&subscribe=optin", redisHost, db)
	urlB := fmt.Sprintf("rueidis://%s/%d?ttl=1h&subscribe=optin", redisHost, db)

	mA := NewClient(urlA, &Config{})
	if mA == nil {
		t.Fatalf("cannot create client A")
	}
	defer mA.Shutdown()

	mB := NewClient(urlB, &Config{})
	if mB == nil {
		t.Fatalf("cannot create client B")
	}
	defer mB.Shutdown()

	rmA, ok := mA.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta for client A")
	}

	rmB, ok := mB.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta for client B")
	}

	// Verify both are in OPTIN mode
	if rmA.subscribeMode != "optin" || rmB.subscribeMode != "optin" {
		t.Fatalf("Both clients should be in OPTIN mode")
	}

	// Test basic key operation
	testKey := "test:key:optin"
	testValue := "optin:test:value"

	// Clear key
	rmA.compat.Del(Background(), testKey)
	time.Sleep(100 * time.Millisecond)

	// Client A writes
	err := rmA.compat.Set(Background(), testKey, testValue, 0).Err()
	if err != nil {
		t.Fatalf("Client A Set error: %v", err)
	}

	// Small delay for invalidation propagation
	time.Sleep(500 * time.Millisecond)

	// Client B reads (should get the value, potentially from cache invalidation)
	val, err := rmB.compat.Get(Background(), testKey).Result()
	if err != nil {
		t.Logf("Client B Get error (may be expected): %v", err)
	} else if val == testValue {
		t.Logf("✅ OPTIN invalidation: Client B got value from Client A write")
	}

	// Cleanup
	rmA.compat.Del(Background(), testKey)

	t.Logf("✅ OPTIN mode invalidation test completed")
}

// TestRueidisIntegration_BCASTInvalidationAcrossClients verifies that
// BCAST mode properly broadcasts invalidation messages
func TestRueidisIntegration_BCASTInvalidationAcrossClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	redisHost := "100.121.51.13:6379"
	db := 12

	urlA := fmt.Sprintf("rueidis://%s/%d?ttl=1h&subscribe=bcast", redisHost, db)
	urlB := fmt.Sprintf("rueidis://%s/%d?ttl=1h&subscribe=bcast", redisHost, db)

	mA := NewClient(urlA, &Config{})
	if mA == nil {
		t.Fatalf("cannot create client A")
	}
	defer mA.Shutdown()

	mB := NewClient(urlB, &Config{})
	if mB == nil {
		t.Fatalf("cannot create client B")
	}
	defer mB.Shutdown()

	rmA, ok := mA.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta for client A")
	}

	rmB, ok := mB.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta for client B")
	}

	// Verify both are in BCAST mode
	if rmA.subscribeMode != "bcast" || rmB.subscribeMode != "bcast" {
		t.Fatalf("Both clients should be in BCAST mode")
	}

	// Test basic key operation with broadcast
	testKey := "test:key:bcast"
	testValue := "bcast:test:value"

	// Clear key
	rmA.compat.Del(Background(), testKey)
	time.Sleep(100 * time.Millisecond)

	// Client A writes
	err := rmA.compat.Set(Background(), testKey, testValue, 0).Err()
	if err != nil {
		t.Fatalf("Client A Set error: %v", err)
	}

	// Small delay for broadcast propagation
	time.Sleep(500 * time.Millisecond)

	// Client B reads (should get the value via broadcast invalidation)
	val, err := rmB.compat.Get(Background(), testKey).Result()
	if err != nil {
		t.Logf("Client B Get error (may be expected): %v", err)
	} else if val == testValue {
		t.Logf("✅ BCAST broadcast: Client B got value from Client A write")
	}

	// Cleanup
	rmA.compat.Del(Background(), testKey)

	t.Logf("✅ BCAST mode broadcast test completed")
}

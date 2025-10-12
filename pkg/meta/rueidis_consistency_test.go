//go:build !norueidis
// +build !norueidis

package meta

import (
	"syscall"
	"testing"
	"time"
)

// TestRueidis_SameClientWriteReadConsistency verifies that a single client
// sees its own writes immediately through cached helpers.
// This tests the basic scenario: write → read on the same client connection.
func TestRueidis_SameClientWriteReadConsistency(t *testing.T) {
	m, closeFn := newTestRueidisClient(t, "rueidis://100.121.51.13:6379/14?ttl=1h")
	defer closeFn()

	rueidis, ok := m.(*rueidisMeta)
	if !ok {
		t.Fatalf("expected rueidisMeta, got %T", m)
	}

	// Write an inode attribute using direct write (simulating Create/SetAttr operation)
	testInode := Ino(99999)
	testKey := rueidis.inodeKey(testInode)
	testValue := []byte("test-attr-data")

	// Write via compat (simulating what write operations do)
	err := rueidis.compat.Set(Background(), testKey, testValue, 0).Err()
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read back via cachedGet (what read operations use)
	readValue, err := rueidis.cachedGet(Background(), testKey)
	if err != nil {
		t.Fatalf("cachedGet failed after write: %v", err)
	}

	if string(readValue) != string(testValue) {
		t.Errorf("same-client read mismatch: wrote %q, read %q", testValue, readValue)
	}

	// Test directory entry write→read
	parentInode := Ino(1)
	entryKey := rueidis.entryKey(parentInode)
	entryField := "testfile"
	entryValue := "t1_99999" // type=1 (file), inode=99999

	err = rueidis.compat.HSet(Background(), entryKey, entryField, entryValue).Err()
	if err != nil {
		t.Fatalf("HSet failed: %v", err)
	}

	readEntry, err := rueidis.cachedHGet(Background(), entryKey, entryField)
	if err != nil {
		t.Fatalf("cachedHGet failed after HSet: %v", err)
	}

	if string(readEntry) != entryValue {
		t.Errorf("same-client entry read mismatch: wrote %q, read %q", entryValue, readEntry)
	}

	// Cleanup
	rueidis.compat.Del(Background(), testKey, entryKey)
}

// TestRueidis_CrossClientWriteReadConsistency verifies that client A's writes
// are immediately visible to client B through cached helpers.
// This is the CRITICAL test - this scenario is currently failing in production.
func TestRueidis_CrossClientWriteReadConsistency(t *testing.T) {
	// Use separate DB to avoid collision with other tests
	url := "rueidis://100.121.51.13:6379/13?ttl=1h"

	clientA, closeA := newTestRueidisClient(t, url)
	defer closeA()
	clientB, closeB := newTestRueidisClient(t, url)
	defer closeB()

	rueidisA, ok := clientA.(*rueidisMeta)
	if !ok {
		t.Fatalf("client A: expected rueidisMeta, got %T", clientA)
	}
	rueidisB, ok := clientB.(*rueidisMeta)
	if !ok {
		t.Fatalf("client B: expected rueidisMeta, got %T", clientB)
	}

	// Use unique inode/entry to avoid test interference
	testInode := Ino(88888)
	testKey := rueidisA.inodeKey(testInode)
	testValue := []byte("cross-client-test-data")

	// Ensure clean state
	rueidisA.compat.Del(Background(), testKey)
	time.Sleep(100 * time.Millisecond) // Allow invalidation to propagate

	// Client B: attempt read before write (should return ENOENT)
	_, err := rueidisB.cachedGet(Background(), testKey)
	if err != syscall.ENOENT {
		t.Logf("pre-write read returned: %v (expected ENOENT)", err)
	}

	// Client A: write the key
	err = rueidisA.compat.Set(Background(), testKey, testValue, 0).Err()
	if err != nil {
		t.Fatalf("client A write failed: %v", err)
	}
	t.Logf("client A wrote key=%s value=%s", testKey, testValue)

	// Give BCAST invalidation time to propagate
	time.Sleep(200 * time.Millisecond)

	// Client B: read via cachedGet (THIS IS THE FAILING SCENARIO)
	// With proper BCAST, client B should receive INVALIDATE message and fetch fresh data
	var readValue []byte
	success := false
	for attempt := 0; attempt < 50; attempt++ {
		readValue, err = rueidisB.cachedGet(Background(), testKey)
		if err == nil && string(readValue) == string(testValue) {
			success = true
			t.Logf("client B read succeeded on attempt %d", attempt)
			break
		}
		if err != nil {
			t.Logf("attempt %d: cachedGet error: %v", attempt, err)
		} else {
			t.Logf("attempt %d: cachedGet returned wrong value: %q (expected %q)", attempt, readValue, testValue)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !success {
		// Debug: try direct read to verify data exists in Redis
		directValue, directErr := rueidisB.compat.Get(Background(), testKey).Bytes()
		t.Errorf("FAILED: client B cachedGet did not see client A's write after 5s")
		t.Errorf("  Last cachedGet error: %v", err)
		t.Errorf("  Last cachedGet value: %q", readValue)
		t.Errorf("  Direct compat.Get value: %q (err=%v)", directValue, directErr)

		// Check tracking info
		if ti, tiErr := rueidisB.GetCacheTrackingInfo(Background()); tiErr == nil {
			t.Errorf("  Client B tracking info: %+v", ti)
		}
	}

	// Test directory entry cross-client consistency
	parentInode := Ino(2)
	entryKey := rueidisA.entryKey(parentInode)
	entryField := "crossfile"
	entryValue := "t1_88888"

	rueidisA.compat.Del(Background(), entryKey)
	time.Sleep(100 * time.Millisecond)

	// Client A writes entry
	err = rueidisA.compat.HSet(Background(), entryKey, entryField, entryValue).Err()
	if err != nil {
		t.Fatalf("client A HSet failed: %v", err)
	}
	t.Logf("client A wrote entry: %s[%s]=%s", entryKey, entryField, entryValue)

	time.Sleep(200 * time.Millisecond)

	// Client B reads entry
	success = false
	var readEntry []byte
	for attempt := 0; attempt < 50; attempt++ {
		readEntry, err = rueidisB.cachedHGet(Background(), entryKey, entryField)
		if err == nil && string(readEntry) == entryValue {
			success = true
			t.Logf("client B HGet succeeded on attempt %d", attempt)
			break
		}
		if err != nil {
			t.Logf("attempt %d: cachedHGet error: %v", attempt, err)
		} else {
			t.Logf("attempt %d: cachedHGet returned: %q (expected %q)", attempt, readEntry, entryValue)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if !success {
		directEntry, directErr := rueidisB.compat.HGet(Background(), entryKey, entryField).Bytes()
		t.Errorf("FAILED: client B cachedHGet did not see client A's HSet after 5s")
		t.Errorf("  Last cachedHGet error: %v", err)
		t.Errorf("  Last cachedHGet value: %q", readEntry)
		t.Errorf("  Direct compat.HGet value: %q (err=%v)", directEntry, directErr)
	}

	// Cleanup
	rueidisA.compat.Del(Background(), testKey, entryKey)
}

// TestRueidis_LargeTTLConsistency verifies that even with a large TTL (1h default),
// writes are immediately visible via invalidation.
func TestRueidis_LargeTTLConsistency(t *testing.T) {
	// Connect with explicit 1h TTL (default)
	clientA, closeA := newTestRueidisClient(t, "rueidis://100.121.51.13:6379/12?ttl=1h")
	defer closeA()
	clientB, closeB := newTestRueidisClient(t, "rueidis://100.121.51.13:6379/12?ttl=1h")
	defer closeB()

	rueidisA := clientA.(*rueidisMeta)
	rueidisB := clientB.(*rueidisMeta)

	testInode := Ino(77777)
	testKey := rueidisA.inodeKey(testInode)
	testValue := []byte("large-ttl-test")

	rueidisA.compat.Del(Background(), testKey)
	time.Sleep(100 * time.Millisecond)

	// Write from A
	err := rueidisA.compat.Set(Background(), testKey, testValue, 0).Err()
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	// Read from B with 1h TTL - should still see immediate update via invalidation
	time.Sleep(200 * time.Millisecond)

	readValue, err := rueidisB.cachedGet(Background(), testKey)
	if err != nil {
		// Try polling
		for attempt := 0; attempt < 30; attempt++ {
			readValue, err = rueidisB.cachedGet(Background(), testKey)
			if err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	if err != nil {
		directValue, _ := rueidisB.compat.Get(Background(), testKey).Bytes()
		t.Errorf("FAILED: 1h TTL test - cached read failed: %v", err)
		t.Errorf("  Direct read value: %q", directValue)
		t.Errorf("  Client B should see write immediately via BCAST invalidation regardless of TTL")
	} else if string(readValue) != string(testValue) {
		t.Errorf("value mismatch with 1h TTL: got %q, want %q", readValue, testValue)
	}

	// Cleanup
	rueidisA.compat.Del(Background(), testKey)
}

// Helper to create a test Rueidis client
func newTestRueidisClient(t *testing.T, url string) (Meta, func()) {
	m := NewClient(url, &Config{})
	if m == nil {
		t.Fatalf("failed to create Rueidis client with url=%s", url)
	}
	return m, func() {
		m.Shutdown()
	}
}

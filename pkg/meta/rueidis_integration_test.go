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

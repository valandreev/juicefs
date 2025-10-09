//go:build !norueidis
// +build !norueidis

package meta

import (
	"testing"
)

// TestRueidisCounterInitialization verifies that counters are properly initialized
// when formatting a new filesystem
func TestRueidisCounterInitialization(t *testing.T) {
	if testing.Short() {
		t.Skip("Skip counter init test in short mode")
	}

	redisURI := "rueidis://100.121.51.13:6379/14" // Use dedicated test DB
	m := NewClient(redisURI, &Config{})
	rm := m.(*rueidisMeta)

	// Clear database
	if err := rm.Reset(); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	// Format filesystem
	format := &Format{
		Name:      "counter-test",
		BlockSize: 4096,
	}
	if err := rm.doInit(format, true); err != nil {
		t.Fatalf("doInit failed: %v", err)
	}

	// Check raw Redis values
	ctx := Background()
	val, err := rm.compat.Get(ctx, rm.counterKey("nextChunk")).Int64()
	if err != nil {
		t.Fatalf("Get nextChunk raw failed: %v", err)
	}
	t.Logf("Raw Redis nextChunk = %d (expected 0)", val)

	val2, err := rm.compat.Get(ctx, rm.counterKey("nextInode")).Int64()
	if err != nil {
		t.Fatalf("Get nextInode raw failed: %v", err)
	}
	t.Logf("Raw Redis nextInode = %d (expected 1)", val2)

	// Check through getCounter
	nextChunk, err := rm.getCounter("nextChunk")
	if err != nil {
		t.Fatalf("getCounter(nextChunk) failed: %v", err)
	}
	t.Logf("getCounter nextChunk = %d (expected 1, from 0+1)", nextChunk)

	if nextChunk != 1 {
		t.Errorf("nextChunk should be 1 after init, got %d", nextChunk)
	}

	nextInode, err := rm.getCounter("nextInode")
	if err != nil {
		t.Fatalf("getCounter(nextInode) failed: %v", err)
	}
	t.Logf("getCounter nextInode = %d (expected 2, from 1+1)", nextInode)

	if nextInode != 2 {
		t.Errorf("nextInode should be 2 after init (root inode exists), got %d", nextInode)
	}
}

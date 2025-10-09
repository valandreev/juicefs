//go:build !norueidis
// +build !norueidis

package meta

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRueidisFlockBasic tests basic BSD file lock (Flock) operations.
func TestRueidisFlockBasic(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/8"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a test file
	var fileIno Ino
	attr := &Attr{}
	st := m.Mknod(ctx, RootInode, "lockfile", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod failed")
	defer m.Unlink(ctx, RootInode, "lockfile")

	owner1 := uint64(1001)
	owner2 := uint64(1002)

	// Test 1: Acquire read lock
	st = m.Flock(ctx, fileIno, owner1, F_RDLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to acquire read lock")
	t.Logf("✓ Acquired read lock for owner %d", owner1)

	// Test 2: Another read lock should succeed (multiple readers allowed)
	st = m.Flock(ctx, fileIno, owner2, F_RDLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to acquire second read lock")
	t.Logf("✓ Acquired second read lock for owner %d", owner2)

	// Test 3: Write lock should fail while read locks exist (non-blocking)
	st = m.Flock(ctx, fileIno, owner1, F_WRLCK, false)
	require.Equal(t, syscall.EAGAIN, st, "Write lock should fail with EAGAIN")
	t.Logf("✓ Write lock correctly blocked by read locks")

	// Test 4: Release read locks
	st = m.Flock(ctx, fileIno, owner1, F_UNLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to release read lock")
	st = m.Flock(ctx, fileIno, owner2, F_UNLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to release second read lock")
	t.Logf("✓ Released all read locks")

	// Test 5: Now write lock should succeed
	st = m.Flock(ctx, fileIno, owner1, F_WRLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to acquire write lock")
	t.Logf("✓ Acquired write lock for owner %d", owner1)

	// Test 6: Another write lock should fail (exclusive)
	st = m.Flock(ctx, fileIno, owner2, F_WRLCK, false)
	require.Equal(t, syscall.EAGAIN, st, "Second write lock should fail")
	t.Logf("✓ Write lock is exclusive")

	// Test 7: Read lock should also fail while write lock exists
	st = m.Flock(ctx, fileIno, owner2, F_RDLCK, false)
	require.Equal(t, syscall.EAGAIN, st, "Read lock should fail while write lock exists")
	t.Logf("✓ Read lock blocked by write lock")

	// Test 8: Release write lock
	st = m.Flock(ctx, fileIno, owner1, F_UNLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to release write lock")
	t.Logf("✓ Released write lock")
}

// TestRueidisFlockConcurrent tests concurrent lock acquisition with blocking.
func TestRueidisFlockConcurrent(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/9"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a test file
	var fileIno Ino
	attr := &Attr{}
	st := m.Mknod(ctx, RootInode, "concurrent_lock", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod failed")
	defer m.Unlink(ctx, RootInode, "concurrent_lock")

	// Test concurrent write lock acquisition
	const numGoroutines = 5
	var successCount int32
	var wg sync.WaitGroup

	// First, acquire a write lock
	owner0 := uint64(2000)
	st = m.Flock(ctx, fileIno, owner0, F_WRLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to acquire initial write lock")

	// Start goroutines that will try to acquire write lock (non-blocking)
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			owner := uint64(2001 + id)
			localCtx := Background()
			st := m.Flock(localCtx, fileIno, owner, F_WRLCK, false)
			if st == 0 {
				atomic.AddInt32(&successCount, 1)
				// Release immediately
				m.Flock(localCtx, fileIno, owner, F_UNLCK, false)
			}
		}(i)
	}

	// Give goroutines time to try acquiring locks
	time.Sleep(100 * time.Millisecond)

	// They should all fail while we hold the lock
	assert.Equal(t, int32(0), atomic.LoadInt32(&successCount), "No goroutine should acquire lock while it's held")

	// Release the lock
	st = m.Flock(ctx, fileIno, owner0, F_UNLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to release lock")

	wg.Wait()

	t.Logf("✓ Concurrent lock test completed: %d/%d goroutines acquired lock after release", successCount, numGoroutines)
}

// TestRueidisPlock tests POSIX byte-range locks.
func TestRueidisPlock(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/0"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a test file
	var fileIno Ino
	attr := &Attr{}
	st := m.Mknod(ctx, RootInode, "plock_file", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod failed")
	defer m.Unlink(ctx, RootInode, "plock_file")

	owner1 := uint64(3001)
	owner2 := uint64(3002)
	pid1 := uint32(1001)
	pid2 := uint32(1002)

	// Test 1: Lock bytes 0-99 for reading
	st = m.Setlk(ctx, fileIno, owner1, false, F_RDLCK, 0, 99, pid1)
	require.Equal(t, syscall.Errno(0), st, "Failed to set read lock on bytes 0-99")
	t.Logf("✓ Set read lock on bytes 0-99")

	// Test 2: Another read lock on overlapping range should succeed
	st = m.Setlk(ctx, fileIno, owner2, false, F_RDLCK, 50, 149, pid2)
	require.Equal(t, syscall.Errno(0), st, "Failed to set overlapping read lock")
	t.Logf("✓ Set overlapping read lock on bytes 50-149")

	// Test 3: Write lock on overlapping range should fail
	st = m.Setlk(ctx, fileIno, owner2, false, F_WRLCK, 75, 124, pid2)
	require.Equal(t, syscall.EAGAIN, st, "Write lock should fail on overlapping range")
	t.Logf("✓ Write lock correctly blocked by read locks")

	// Test 4: Write lock on non-overlapping range should succeed
	st = m.Setlk(ctx, fileIno, owner2, false, F_WRLCK, 200, 299, pid2)
	require.Equal(t, syscall.Errno(0), st, "Failed to set write lock on non-overlapping range")
	t.Logf("✓ Set write lock on bytes 200-299")

	// Test 5: Test Getlk - check for conflicting locks
	ltype := uint32(F_WRLCK)
	start := uint64(50)
	end := uint64(60)
	checkPid := uint32(0)
	st = m.Getlk(ctx, fileIno, owner2, &ltype, &start, &end, &checkPid)
	require.Equal(t, syscall.Errno(0), st, "Getlk failed")
	assert.Equal(t, uint32(F_RDLCK), ltype, "Expected conflicting read lock")
	t.Logf("✓ Getlk found conflicting read lock: type=%d, range=%d-%d", ltype, start, end)

	// Test 6: Unlock bytes 0-99
	st = m.Setlk(ctx, fileIno, owner1, false, F_UNLCK, 0, 99, pid1)
	require.Equal(t, syscall.Errno(0), st, "Failed to unlock bytes 0-99")
	t.Logf("✓ Unlocked bytes 0-99")

	// Test 7: Now Getlk should find owner2's read lock
	ltype = uint32(F_WRLCK)
	start = uint64(50)
	end = uint64(60)
	st = m.Getlk(ctx, fileIno, owner1, &ltype, &start, &end, &checkPid)
	require.Equal(t, syscall.Errno(0), st, "Getlk failed")
	assert.Equal(t, uint32(F_RDLCK), ltype, "Expected owner2's read lock")
	t.Logf("✓ Getlk found owner2's read lock after owner1 unlocked")

	// Test 8: Cleanup - unlock all
	st = m.Setlk(ctx, fileIno, owner2, false, F_UNLCK, 50, 149, pid2)
	require.Equal(t, syscall.Errno(0), st, "Failed to unlock bytes 50-149")
	st = m.Setlk(ctx, fileIno, owner2, false, F_UNLCK, 200, 299, pid2)
	require.Equal(t, syscall.Errno(0), st, "Failed to unlock bytes 200-299")
	t.Logf("✓ All locks released")
}

// TestRueidisListLocks tests the ListLocks functionality.
func TestRueidisListLocks(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/14"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a test file
	var fileIno Ino
	attr := &Attr{}
	st := m.Mknod(ctx, RootInode, "list_locks_file", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod failed")
	defer m.Unlink(ctx, RootInode, "list_locks_file")

	owner1 := uint64(4001)
	owner2 := uint64(4002)
	owner3 := uint64(4003)
	pid1 := uint32(1001)
	pid2 := uint32(1002)

	// Set multiple locks
	st = m.Flock(ctx, fileIno, owner1, F_RDLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to set flock")

	st = m.Setlk(ctx, fileIno, owner2, false, F_RDLCK, 0, 99, pid1)
	require.Equal(t, syscall.Errno(0), st, "Failed to set plock 1")

	st = m.Setlk(ctx, fileIno, owner3, false, F_WRLCK, 200, 299, pid2)
	require.Equal(t, syscall.Errno(0), st, "Failed to set plock 2")

	// List all locks
	plocks, flocks, err := m.ListLocks(context.Background(), fileIno)
	require.NoError(t, err, "ListLocks failed")

	t.Logf("Found %d POSIX locks and %d BSD locks", len(plocks), len(flocks))

	// Verify we have the expected locks
	assert.GreaterOrEqual(t, len(flocks), 1, "Expected at least 1 BSD lock")
	assert.GreaterOrEqual(t, len(plocks), 2, "Expected at least 2 POSIX locks")

	// Verify flock details
	found := false
	for _, fl := range flocks {
		if fl.Type == "R" {
			found = true
			t.Logf("✓ Found BSD read lock: sid=%d", fl.Sid)
		}
	}
	assert.True(t, found, "Should find BSD read lock")

	// Verify plock details
	foundRead := false
	foundWrite := false
	for _, pl := range plocks {
		if pl.Type == F_RDLCK && pl.Start == 0 && pl.End == 99 {
			foundRead = true
			t.Logf("✓ Found POSIX read lock: bytes 0-99, pid=%d", pl.Pid)
		}
		if pl.Type == F_WRLCK && pl.Start == 200 && pl.End == 299 {
			foundWrite = true
			t.Logf("✓ Found POSIX write lock: bytes 200-299, pid=%d", pl.Pid)
		}
	}
	assert.True(t, foundRead, "Should find POSIX read lock")
	assert.True(t, foundWrite, "Should find POSIX write lock")

	// Cleanup
	m.Flock(ctx, fileIno, owner1, F_UNLCK, false)
	m.Setlk(ctx, fileIno, owner2, false, F_UNLCK, 0, 99, pid1)
	m.Setlk(ctx, fileIno, owner3, false, F_UNLCK, 200, 299, pid2)
}

// TestRueidisLockBlocking tests blocking lock behavior.
func TestRueidisLockBlocking(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/15"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a test file
	var fileIno Ino
	attr := &Attr{}
	st := m.Mknod(ctx, RootInode, "blocking_lock", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod failed")
	defer m.Unlink(ctx, RootInode, "blocking_lock")

	owner1 := uint64(5001)
	owner2 := uint64(5002)

	// Acquire write lock with owner1
	st = m.Flock(ctx, fileIno, owner1, F_WRLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to acquire write lock")
	t.Logf("✓ Owner1 acquired write lock")

	// Start goroutine that will try to acquire write lock with blocking
	lockAcquired := make(chan bool, 1)
	go func() {
		localCtx := Background()
		// This should block until owner1 releases the lock
		st := m.Flock(localCtx, fileIno, owner2, F_WRLCK, true)
		lockAcquired <- (st == 0)
	}()

	// Give blocking call time to start
	time.Sleep(50 * time.Millisecond)

	// Lock should not be acquired yet
	select {
	case acquired := <-lockAcquired:
		t.Fatalf("Lock acquired too early: %v", acquired)
	default:
		t.Logf("✓ Owner2 is blocking as expected")
	}

	// Release the lock
	time.Sleep(50 * time.Millisecond)
	st = m.Flock(ctx, fileIno, owner1, F_UNLCK, false)
	require.Equal(t, syscall.Errno(0), st, "Failed to release write lock")
	t.Logf("✓ Owner1 released write lock")

	// Now the blocking call should succeed
	select {
	case acquired := <-lockAcquired:
		assert.True(t, acquired, "Owner2 should have acquired lock")
		t.Logf("✓ Owner2 acquired lock after owner1 released")
	case <-time.After(2 * time.Second):
		t.Fatal("Timeout waiting for lock acquisition")
	}

	// Cleanup
	m.Flock(ctx, fileIno, owner2, F_UNLCK, false)
}

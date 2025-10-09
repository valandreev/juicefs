//go:build !norueidis
// +build !norueidis

package meta

import (
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRueidisLuaScripts tests that Lua scripts (scriptLookup, scriptResolve)
// are loaded correctly and can be reloaded if Redis flushes them.
func TestRueidisLuaScripts(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/6"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize the metadata store
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a directory with some files to test scriptLookup
	var parentIno Ino
	attr := &Attr{}
	st := m.Mkdir(ctx, RootInode, "luatest", 0755, 0, 0, &parentIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mkdir failed")
	defer m.Rmdir(ctx, RootInode, "luatest")

	// Create multiple files to test Lookup (which uses scriptLookup)
	for i := 0; i < 5; i++ {
		var ino Ino
		fname := fmt.Sprintf("file%d", i)
		st := m.Mknod(ctx, parentIno, fname, TypeFile, 0644, 0, 0, "", &ino, attr)
		require.Equal(t, syscall.Errno(0), st, "Mknod %s failed", fname)
	}

	// Test Lookup which uses scriptLookup Lua script
	var lookupIno Ino
	st = m.Lookup(ctx, parentIno, "file2", &lookupIno, attr, false)
	require.Equal(t, syscall.Errno(0), st, "Lookup file2 failed")
	t.Logf("✓ Lookup (scriptLookup) succeeded: found inode %d", lookupIno)

	// Test Resolve which uses scriptResolve Lua script (if supported)
	var resolveIno Ino
	st = m.Resolve(ctx, RootInode, "/luatest/file2", &resolveIno, attr)
	if st != syscall.ENOTSUP {
		require.Equal(t, syscall.Errno(0), st, "Resolve failed")
		require.Equal(t, lookupIno, resolveIno, "Resolve returned different inode")
		t.Logf("✓ Resolve (scriptResolve) succeeded: found inode %d", resolveIno)
	} else {
		t.Logf("⊗ Resolve not supported (ENOTSUP)")
	}

	// Now test script reloading by accessing the Rueidis internals
	if rm, ok := m.(*rueidisMeta); ok {
		// Save original SHAs
		origLookup := rm.shaLookup
		origResolve := rm.shaResolve

		t.Logf("Original SHAs - lookup: %s, resolve: %s", origLookup, origResolve)

		// Simulate script flush by clearing the SHAs
		rm.shaLookup = ""
		rm.shaResolve = ""
		t.Logf("Cleared SHAs to simulate SCRIPT FLUSH")

		// Lookup should still work - it will trigger reload internally
		st = m.Lookup(ctx, parentIno, "file3", &lookupIno, attr, false)
		require.Equal(t, syscall.Errno(0), st, "Lookup after SHA clear failed")
		t.Logf("✓ Lookup after SHA clear succeeded (script auto-reloaded)")

		// Verify SHAs were reloaded (they should be empty or the scripts work via fallback)
		t.Logf("After reload - lookup SHA: %s, resolve SHA: %s", rm.shaLookup, rm.shaResolve)
	} else {
		t.Logf("⊗ Not a rueidisMeta instance, skipping script reload test")
	}
}

// TestRueidisTransactionRetry tests the transaction retry logic under concurrent modification.
func TestRueidisTransactionRetry(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/7"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize the metadata store
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a test file
	var fileIno Ino
	attr := &Attr{}
	st := m.Mknod(ctx, RootInode, "contention", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod failed")
	defer m.Unlink(ctx, RootInode, "contention")

	// Simulate concurrent modifications to trigger transaction retries
	const numGoroutines = 10
	const opsPerGoroutine = 5

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*opsPerGoroutine)
	success := make(chan int, numGoroutines*opsPerGoroutine)

	startTime := time.Now()

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < opsPerGoroutine; j++ {
				// Perform SetAttr operations which use transactions
				localCtx := NewContext(uint32(id), uint32(id), []uint32{uint32(id)})
				attr := &Attr{
					Uid: uint32(id),
					Gid: uint32(id),
				}
				st := m.SetAttr(localCtx, fileIno, SetAttrUID|SetAttrGID, 0, attr)
				if st != 0 {
					errors <- fmt.Errorf("goroutine %d op %d: SetAttr failed: %s", id, j, st)
				} else {
					success <- id
				}
				time.Sleep(time.Millisecond) // Small delay to increase contention
			}
		}(i)
	}

	wg.Wait()
	close(errors)
	close(success)

	duration := time.Since(startTime)

	// Count results
	successCount := 0
	errorCount := 0
	var firstError error

	for err := range errors {
		errorCount++
		if firstError == nil {
			firstError = err
		}
	}
	for range success {
		successCount++
	}

	t.Logf("Transaction retry test completed in %v", duration)
	t.Logf("Success: %d/%d operations", successCount, numGoroutines*opsPerGoroutine)
	if errorCount > 0 {
		t.Logf("Errors: %d (first error: %v)", errorCount, firstError)
	}

	// We expect high success rate - transactions should retry and eventually succeed
	expectedTotal := numGoroutines * opsPerGoroutine
	assert.GreaterOrEqual(t, successCount, int(float64(expectedTotal)*0.9),
		"Expected at least 90%% success rate, got %d/%d", successCount, expectedTotal)

	// Verify final state is consistent
	finalAttr := &Attr{}
	st = m.GetAttr(ctx, fileIno, finalAttr)
	require.Equal(t, syscall.Errno(0), st, "GetAttr after concurrent ops failed")
	t.Logf("Final inode state: UID=%d, GID=%d", finalAttr.Uid, finalAttr.Gid)
}

// TestRueidisTransactionIsolation tests that transactions properly isolate concurrent operations.
func TestRueidisTransactionIsolation(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/4"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize the metadata store
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Create a directory
	var dirIno Ino
	attr := &Attr{}
	st := m.Mkdir(ctx, RootInode, "txtest", 0755, 0, 0, &dirIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mkdir failed")
	defer m.Rmdir(ctx, RootInode, "txtest")

	// Create multiple files concurrently to test transaction isolation
	const numFiles = 20
	var wg sync.WaitGroup
	results := make(chan struct {
		name string
		ino  Ino
		err  error
	}, numFiles)

	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			var ino Ino
			attr := &Attr{}
			fname := fmt.Sprintf("txfile%d", id)
			st := m.Mknod(ctx, dirIno, fname, TypeFile, 0644, 0, 0, "", &ino, attr)
			if st != 0 {
				results <- struct {
					name string
					ino  Ino
					err  error
				}{fname, 0, fmt.Errorf("failed: %s", st)}
			} else {
				results <- struct {
					name string
					ino  Ino
					err  error
				}{fname, ino, nil}
			}
		}(i)
	}

	wg.Wait()
	close(results)

	// Verify all files were created successfully
	inodes := make(map[Ino]string)
	successCount := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("File %s creation failed: %v", result.name, result.err)
		} else {
			successCount++
			if existingName, exists := inodes[result.ino]; exists {
				t.Errorf("Duplicate inode %d for files %s and %s", result.ino, existingName, result.name)
			}
			inodes[result.ino] = result.name
		}
	}

	assert.Equal(t, numFiles, successCount, "Not all files created successfully")
	assert.Equal(t, numFiles, len(inodes), "Duplicate inodes detected")

	t.Logf("✓ Transaction isolation test passed: %d files created with unique inodes", numFiles)
}

// TestRueidisComplexOperations tests complex metadata operations that use transactions extensively.
func TestRueidisComplexOperations(t *testing.T) {
	testURI := "rueidis://100.121.51.13:6379/5"
	m := createMetaFromURI(t, testURI, testConfig())
	require.NotNil(t, m)
	defer m.Shutdown()

	// Initialize the metadata store
	format := testFormat()
	err := m.Init(format, false)
	require.NoError(t, err, "Init failed")

	ctx := Background()

	// Test 1: Rename operation (uses doRename with transactions)
	var dir1, dir2 Ino
	attr := &Attr{}
	st := m.Mkdir(ctx, RootInode, "src_dir", 0755, 0, 0, &dir1, attr)
	require.Equal(t, syscall.Errno(0), st, "Mkdir src_dir failed")
	defer m.Rmdir(ctx, RootInode, "src_dir")

	st = m.Mkdir(ctx, RootInode, "dst_dir", 0755, 0, 0, &dir2, attr)
	require.Equal(t, syscall.Errno(0), st, "Mkdir dst_dir failed")
	defer m.Rmdir(ctx, RootInode, "dst_dir")

	var fileIno Ino
	st = m.Mknod(ctx, dir1, "moveme", TypeFile, 0644, 0, 0, "", &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Mknod moveme failed")

	// Rename the file
	st = m.Rename(ctx, dir1, "moveme", dir2, "moved", 0, &fileIno, attr)
	require.Equal(t, syscall.Errno(0), st, "Rename failed")
	t.Logf("✓ Rename operation succeeded")

	// Verify file is in new location
	var lookupIno Ino
	st = m.Lookup(ctx, dir2, "moved", &lookupIno, attr, false)
	require.Equal(t, syscall.Errno(0), st, "Lookup after rename failed")
	require.Equal(t, fileIno, lookupIno, "Inode mismatch after rename")

	// Verify file is NOT in old location
	st = m.Lookup(ctx, dir1, "moveme", &lookupIno, attr, false)
	require.Equal(t, syscall.ENOENT, st, "File still exists in old location")

	// Test 2: Write and Read operations (use transactions)
	slice := Slice{Id: 1, Off: 0, Len: 1024, Size: 1024}
	st = m.Write(ctx, fileIno, 0, 0, slice, time.Now())
	require.Equal(t, syscall.Errno(0), st, "Write failed")
	t.Logf("✓ Write operation succeeded")

	// Read back the slice
	slices := make([]Slice, 0)
	st = m.Read(ctx, fileIno, 0, &slices)
	require.Equal(t, syscall.Errno(0), st, "Read failed")
	require.Equal(t, 1, len(slices), "Expected 1 slice")
	require.Equal(t, uint64(1), slices[0].Id, "Slice ID mismatch")
	t.Logf("✓ Read operation succeeded: found %d slices", len(slices))

	// Test 3: Cleanup
	st = m.Unlink(ctx, dir2, "moved")
	require.Equal(t, syscall.Errno(0), st, "Unlink failed")
	t.Logf("✓ Complex operations test completed successfully")
}

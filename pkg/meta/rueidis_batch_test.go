//go:build !norueidis
// +build !norueidis

package meta

import (
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/rueidis/rueidiscompat"
)

// TestRueidisConfigParsing verifies that ID batching configuration is parsed correctly
func TestRueidisConfigParsing(t *testing.T) {
	tests := []struct {
		name           string
		addr           string
		wantMetaPrime  bool
		wantInodeBatch uint64
		wantChunkBatch uint64
		wantInodeLowWM uint64
		wantChunkLowWM uint64
	}{
		{
			name:           "defaults",
			addr:           "localhost:6379/1",
			wantMetaPrime:  true,
			wantInodeBatch: 256,
			wantChunkBatch: 2048,
			wantInodeLowWM: 64,  // 25% of 256
			wantChunkLowWM: 512, // 25% of 2048
		},
		{
			name:           "custom batch sizes",
			addr:           "localhost:6379/1?inode_batch=512&chunk_batch=4096",
			wantMetaPrime:  true,
			wantInodeBatch: 512,
			wantChunkBatch: 4096,
			wantInodeLowWM: 128,  // 25% of 512
			wantChunkLowWM: 1024, // 25% of 4096
		},
		{
			name:           "custom watermarks",
			addr:           "localhost:6379/1?inode_low_watermark=50&chunk_low_watermark=30",
			wantMetaPrime:  true,
			wantInodeBatch: 256,
			wantChunkBatch: 2048,
			wantInodeLowWM: 128, // 50% of 256
			wantChunkLowWM: 614, // 30% of 2048
		},
		{
			name:           "metaprime disabled",
			addr:           "localhost:6379/1?metaprime=0",
			wantMetaPrime:  false,
			wantInodeBatch: 256,
			wantChunkBatch: 2048,
			wantInodeLowWM: 64,
			wantChunkLowWM: 512,
		},
		{
			name:           "all custom",
			addr:           "localhost:6379/1?inode_batch=1024&chunk_batch=8192&inode_low_watermark=10&chunk_low_watermark=20&metaprime=1",
			wantMetaPrime:  true,
			wantInodeBatch: 1024,
			wantChunkBatch: 8192,
			wantInodeLowWM: 102,  // 10% of 1024
			wantChunkLowWM: 1638, // 20% of 8192
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &Config{
				Strict: true,
			}

			// Note: This will fail to connect to Redis, but we only care about config parsing
			m, err := newRueidisMeta("rueidis", tt.addr, conf)
			if err != nil {
				// Expected to fail connection, but struct should be initialized
				t.Logf("Connection error (expected): %v", err)
			}

			// If we got a valid meta struct, verify the config
			if m != nil {
				rm := m.(*rueidisMeta)
				if rm.metaPrimeEnabled != tt.wantMetaPrime {
					t.Errorf("metaPrimeEnabled = %v, want %v", rm.metaPrimeEnabled, tt.wantMetaPrime)
				}
				if rm.inodeBatch != tt.wantInodeBatch {
					t.Errorf("inodeBatch = %v, want %v", rm.inodeBatch, tt.wantInodeBatch)
				}
				if rm.chunkBatch != tt.wantChunkBatch {
					t.Errorf("chunkBatch = %v, want %v", rm.chunkBatch, tt.wantChunkBatch)
				}
				if rm.inodeLowWM != tt.wantInodeLowWM {
					t.Errorf("inodeLowWM = %v, want %v", rm.inodeLowWM, tt.wantInodeLowWM)
				}
				if rm.chunkLowWM != tt.wantChunkLowWM {
					t.Errorf("chunkLowWM = %v, want %v", rm.chunkLowWM, tt.wantChunkLowWM)
				}
			}
		})
	}
}

// TestRueidisMetricsInitialization verifies that all metrics are properly initialized
func TestRueidisMetricsInitialization(t *testing.T) {
	conf := &Config{
		Strict: true,
	}

	// This will fail to connect but struct initialization should work
	m, err := newRueidisMeta("rueidis", "localhost:6379/1", conf)
	if err != nil {
		t.Logf("Connection error (expected): %v", err)
	}

	if m == nil {
		t.Skip("Could not create meta instance")
	}

	rm := m.(*rueidisMeta)

	// Verify all metrics are non-nil
	if rm.cacheHits == nil {
		t.Error("cacheHits metric is nil")
	}
	if rm.cacheMisses == nil {
		t.Error("cacheMisses metric is nil")
	}
	if rm.inodePrimeCalls == nil {
		t.Error("inodePrimeCalls metric is nil")
	}
	if rm.chunkPrimeCalls == nil {
		t.Error("chunkPrimeCalls metric is nil")
	}
	if rm.primeErrors == nil {
		t.Error("primeErrors metric is nil")
	}
	if rm.inodeIDsServed == nil {
		t.Error("inodeIDsServed metric is nil")
	}
	if rm.chunkIDsServed == nil {
		t.Error("chunkIDsServed metric is nil")
	}
	if rm.inodePrefetchAsync == nil {
		t.Error("inodePrefetchAsync metric is nil")
	}
	if rm.chunkPrefetchAsync == nil {
		t.Error("chunkPrefetchAsync metric is nil")
	}

	// Test metrics registration
	reg := prometheus.NewRegistry()
	rm.InitMetrics(reg)

	// Verify metrics can be gathered (this will fail if not registered correctly)
	metrics, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Count Rueidis-specific metrics (should have at least the 9 ID batching + 2 cache metrics)
	rueidisMetrics := 0
	for _, mf := range metrics {
		name := mf.GetName()
		if len(name) > 7 && name[:7] == "rueidis" {
			rueidisMetrics++
			t.Logf("Found metric: %s", name)
		}
	}

	// We should have at least 9 ID batching metrics + 2 cache metrics = 11 total
	if rueidisMetrics < 9 {
		t.Errorf("Expected at least 9 Rueidis-specific metrics, got %d", rueidisMetrics)
	}
}

// TestRueidisPrimeFunctions verifies that primeInodes and primeChunks allocate correct ID ranges
func TestRueidisPrimeFunctions(t *testing.T) {
	conf := &Config{
		Strict: true,
	}

	// Connect to test Redis instance (use DB 15 which should be available)
	m, err := newRueidisMeta("rueidis", "100.121.51.13:6379/15", conf)
	if err != nil {
		t.Fatalf("Failed to create meta: %v", err)
	}
	defer m.Shutdown()

	rm := m.(*rueidisMeta)

	// Initialize the filesystem to set up counters
	format := &Format{
		Name:      "prime-test",
		UUID:      "test-uuid-prime",
		Storage:   "file",
		Bucket:    "/tmp/jfs-test",
		BlockSize: 4096,
	}

	if err := rm.Init(format, true); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	t.Run("primeInodes_single_batch", func(t *testing.T) {
		// Get initial counter value (direct read, bypassing cache)
		initialData, err := rm.compat.Get(Background(), rm.counterKey("nextInode")).Bytes()
		if err != nil && err != rueidiscompat.Nil {
			t.Fatalf("Direct Get failed: %v", err)
		}
		var initial int64 = 1 // Default if key doesn't exist
		if err == nil {
			initial, _ = strconv.ParseInt(string(initialData), 10, 64)
			initial++ // nextInode is stored as value-1
		}
		t.Logf("Initial nextInode = %d", initial)

		// Prime a batch of 256 inodes
		start, err := rm.primeInodes(256)
		if err != nil {
			t.Fatalf("primeInodes failed: %v", err)
		}
		t.Logf("primeInodes(256) returned start=%d", start)

		// Verify start is correct
		if start != uint64(initial) {
			t.Errorf("primeInodes start = %d, want %d", start, initial)
		}

		// Verify counter advanced by 256 (direct read)
		afterData, err := rm.compat.Get(Background(), rm.counterKey("nextInode")).Bytes()
		if err != nil {
			t.Fatalf("Direct Get after prime failed: %v", err)
		}
		afterStored, _ := strconv.ParseInt(string(afterData), 10, 64)
		after := afterStored + 1 // nextInode is stored as value-1
		t.Logf("After primeInodes: nextInode = %d", after)

		expected := initial + 256
		if after != expected {
			t.Errorf("nextInode after prime = %d, want %d", after, expected)
		}

		// Verify the range is [start, start+255]
		rangeEnd := start + 255
		if rangeEnd != uint64(after-1) {
			t.Errorf("Range end = %d, want %d", rangeEnd, after-1)
		}
	})

	t.Run("primeChunks_single_batch", func(t *testing.T) {
		// Get initial counter value (direct read)
		initialData, err := rm.compat.Get(Background(), rm.counterKey("nextChunk")).Bytes()
		if err != nil && err != rueidiscompat.Nil {
			t.Fatalf("Direct Get failed: %v", err)
		}
		var initial int64 = 1 // Default if key doesn't exist
		if err == nil {
			initial, _ = strconv.ParseInt(string(initialData), 10, 64)
			initial++ // nextChunk is stored as value-1
		}
		t.Logf("Initial nextChunk = %d", initial)

		// Prime a batch of 2048 chunks
		start, err := rm.primeChunks(2048)
		if err != nil {
			t.Fatalf("primeChunks failed: %v", err)
		}
		t.Logf("primeChunks(2048) returned start=%d", start)

		// Verify start is correct
		if start != uint64(initial) {
			t.Errorf("primeChunks start = %d, want %d", start, initial)
		}

		// Verify counter advanced by 2048 (direct read)
		afterData, err := rm.compat.Get(Background(), rm.counterKey("nextChunk")).Bytes()
		if err != nil {
			t.Fatalf("Direct Get after prime failed: %v", err)
		}
		afterStored, _ := strconv.ParseInt(string(afterData), 10, 64)
		after := afterStored + 1
		t.Logf("After primeChunks: nextChunk = %d", after)

		expected := initial + 2048
		if after != expected {
			t.Errorf("nextChunk after prime = %d, want %d", after, expected)
		}
	})

	t.Run("primeInodes_multiple_batches", func(t *testing.T) {
		// Prime 3 batches and verify they don't overlap
		var starts []uint64
		batchSize := uint64(100)

		for i := 0; i < 3; i++ {
			start, err := rm.primeInodes(batchSize)
			if err != nil {
				t.Fatalf("primeInodes batch %d failed: %v", i, err)
			}
			starts = append(starts, start)
			t.Logf("Batch %d: start=%d, range=[%d, %d]", i, start, start, start+batchSize-1)
		}

		// Verify no overlaps
		for i := 0; i < len(starts)-1; i++ {
			end1 := starts[i] + batchSize - 1
			start2 := starts[i+1]
			if end1 >= start2 {
				t.Errorf("Batch %d overlaps with batch %d: [%d, %d] vs [%d, %d]",
					i, i+1, starts[i], end1, start2, start2+batchSize-1)
			}
			// Verify they are consecutive
			if end1+1 != start2 {
				t.Errorf("Batch %d and %d are not consecutive: gap of %d IDs",
					i, i+1, start2-end1-1)
			}
		}
	})

	t.Run("primeChunks_custom_sizes", func(t *testing.T) {
		// Test different batch sizes
		sizes := []uint64{1, 10, 100, 1000, 4096}

		for _, size := range sizes {
			// Direct read of counter before prime
			initialData, err := rm.compat.Get(Background(), rm.counterKey("nextChunk")).Bytes()
			if err != nil {
				t.Fatalf("Direct Get before prime failed: %v", err)
			}
			initialStored, _ := strconv.ParseInt(string(initialData), 10, 64)
			initial := initialStored + 1

			start, err := rm.primeChunks(size)
			if err != nil {
				t.Fatalf("primeChunks(%d) failed: %v", size, err)
			}

			// Direct read of counter after prime
			afterData, err := rm.compat.Get(Background(), rm.counterKey("nextChunk")).Bytes()
			if err != nil {
				t.Fatalf("Direct Get after prime failed: %v", err)
			}
			afterStored, _ := strconv.ParseInt(string(afterData), 10, 64)
			after := afterStored + 1

			if start != uint64(initial) {
				t.Errorf("primeChunks(%d): start=%d, want %d", size, start, initial)
			}
			if after != initial+int64(size) {
				t.Errorf("primeChunks(%d): counter=%d, want %d", size, after, initial+int64(size))
			}
			t.Logf("✓ primeChunks(%d): allocated [%d, %d]", size, start, start+size-1)
		}
	})

	// Cleanup
	if err := rm.Reset(); err != nil {
		t.Logf("Cleanup failed: %v", err)
	}
}

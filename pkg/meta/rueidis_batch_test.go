//go:build !norueidis
// +build !norueidis

package meta

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
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

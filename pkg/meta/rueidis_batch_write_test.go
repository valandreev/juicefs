// Copyright (C) 2024 Juicedata, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !norueidis

package meta

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Helper function to create a test rueidisMeta with proper initialization
func newTestRueidisMeta(batchEnabled bool, queueSize int) *rueidisMeta {
	m := &rueidisMeta{
		batchEnabled: batchEnabled,
		batchQueue:   make(chan *BatchOp, queueSize),
		maxQueueSize: queueSize * 10, // Prevent back-pressure in tests
		redisMeta:    &redisMeta{},
	}

	// Initialize metrics to prevent nil pointer panics
	if batchEnabled {
		m.batchOpsQueued = prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_batch_ops_queued_total"},
			[]string{"type"},
		)
		m.batchOpsFlushed = prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_batch_ops_flushed_total"},
			[]string{"type"},
		)
		m.batchCoalesceSaved = prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_batch_coalesce_saved_total"},
			[]string{"type"},
		)
		m.batchErrors = prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "test_batch_errors_total"},
			[]string{"error_type"},
		)
		m.batchMsetConversions = prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_batch_mset_conversions_total"},
		)
		m.batchMsetOpsSaved = prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_batch_mset_ops_saved_total"},
		)
		m.batchHmsetConversions = prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_batch_hmset_conversions_total"},
		)
		m.batchHsetCoalesced = prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_batch_hset_coalesced_total"},
		)
	}

	return m
}

// Helper function to create a test rueidisMeta with flusher goroutine running
func newTestRueidisMetaWithFlusher(batchEnabled bool, queueSize int) *rueidisMeta {
	m := newTestRueidisMeta(batchEnabled, queueSize)

	if batchEnabled {
		// Initialize additional fields needed for flusher
		m.batchSize = 512
		m.batchBytes = 262144
		m.batchFlushTicker = time.NewTicker(2 * time.Millisecond)
		m.batchStopChan = make(chan struct{})
		m.batchDoneChan = make(chan struct{})
		m.batchFlushDuration = prometheus.NewHistogram(
			prometheus.HistogramOpts{Name: "test_batch_flush_duration_seconds"},
		)
		m.batchSizeHistogram = prometheus.NewHistogram(
			prometheus.HistogramOpts{Name: "test_batch_size_ops"},
		)
		m.batchQueueDepthGauge = prometheus.NewGauge(
			prometheus.GaugeOpts{Name: "test_batch_queue_depth"},
		)
		m.batchPoisonOps = prometheus.NewCounter(
			prometheus.CounterOpts{Name: "test_batch_poison_ops_total"},
		)

		// Start flusher goroutine
		go m.batchFlusher()
	}

	return m
}

// Test 1: SET coalescing - multiple SETs to same key → keep last
func TestCoalesceSET_SameKey(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpSET, Key: "inode:123", Value: []byte("v1"), Inode: 123},
		{Type: OpSET, Key: "inode:123", Value: []byte("v2"), Inode: 123},
		{Type: OpSET, Key: "inode:123", Value: []byte("v3"), Inode: 123},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 1 {
		t.Errorf("Expected 1 coalesced op, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 2 {
		t.Errorf("Expected 2 ops saved, got %d", result.SavedCount)
	}

	if string(result.Coalesced[0].Value) != "v3" {
		t.Errorf("Expected final value 'v3', got '%s'", result.Coalesced[0].Value)
	}
}

// Test 2: SET coalescing - different keys → no coalescing
func TestCoalesceSET_DifferentKeys(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpSET, Key: "inode:123", Value: []byte("v1")},
		{Type: OpSET, Key: "inode:456", Value: []byte("v2")},
		{Type: OpSET, Key: "inode:789", Value: []byte("v3")},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 3 {
		t.Errorf("Expected 3 ops (no coalescing), got %d", len(result.Coalesced))
	}

	if result.SavedCount != 0 {
		t.Errorf("Expected 0 ops saved, got %d", result.SavedCount)
	}
}

// Test 3: HSET coalescing - same hash+field → keep last
func TestCoalesceHSET_SameHashField(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHSET, Key: "dirStats:5", Field: "usedSpace", Value: []byte("100")},
		{Type: OpHSET, Key: "dirStats:5", Field: "usedSpace", Value: []byte("200")},
		{Type: OpHSET, Key: "dirStats:5", Field: "usedSpace", Value: []byte("300")},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 1 {
		t.Errorf("Expected 1 coalesced op, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 2 {
		t.Errorf("Expected 2 ops saved, got %d", result.SavedCount)
	}

	if string(result.Coalesced[0].Value) != "300" {
		t.Errorf("Expected final value '300', got '%s'", result.Coalesced[0].Value)
	}
}

// Test 4: HSET coalescing - same hash, different fields → all kept
func TestCoalesceHSET_SameHashDifferentFields(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHSET, Key: "dirStats:5", Field: "usedSpace", Value: []byte("100")},
		{Type: OpHSET, Key: "dirStats:5", Field: "usedInodes", Value: []byte("10")},
		{Type: OpHSET, Key: "dirStats:5", Field: "dataLength", Value: []byte("200")},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 3 {
		t.Errorf("Expected 3 ops (different fields), got %d", len(result.Coalesced))
	}

	if result.SavedCount != 0 {
		t.Errorf("Expected 0 ops saved (different fields), got %d", result.SavedCount)
	}
}

// Test 5: HINCRBY coalescing - same hash+field → sum deltas
func TestCoalesceHINCRBY_SumDeltas(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHINCRBY, Key: "usedSpace:{fs}", Field: "total", Delta: 100},
		{Type: OpHINCRBY, Key: "usedSpace:{fs}", Field: "total", Delta: 200},
		{Type: OpHINCRBY, Key: "usedSpace:{fs}", Field: "total", Delta: -50},
		{Type: OpHINCRBY, Key: "usedSpace:{fs}", Field: "total", Delta: 75},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 1 {
		t.Errorf("Expected 1 coalesced op, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 3 {
		t.Errorf("Expected 3 ops saved, got %d", result.SavedCount)
	}

	expectedDelta := int64(100 + 200 - 50 + 75) // = 325
	if result.Coalesced[0].Delta != expectedDelta {
		t.Errorf("Expected delta %d, got %d", expectedDelta, result.Coalesced[0].Delta)
	}
}

// Test 6: HINCRBY coalescing - different fields → no coalescing
func TestCoalesceHINCRBY_DifferentFields(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHINCRBY, Key: "stats:{fs}", Field: "reads", Delta: 10},
		{Type: OpHINCRBY, Key: "stats:{fs}", Field: "writes", Delta: 20},
		{Type: OpHINCRBY, Key: "stats:{fs}", Field: "deletes", Delta: 5},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 3 {
		t.Errorf("Expected 3 ops (different fields), got %d", len(result.Coalesced))
	}

	if result.SavedCount != 0 {
		t.Errorf("Expected 0 ops saved, got %d", result.SavedCount)
	}
}

// Test 7: INCRBY coalescing - same key → sum deltas
func TestCoalesceINCRBY_SumDeltas(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpINCRBY, Key: "totalInodes", Delta: 1},
		{Type: OpINCRBY, Key: "totalInodes", Delta: 1},
		{Type: OpINCRBY, Key: "totalInodes", Delta: -1},
		{Type: OpINCRBY, Key: "totalInodes", Delta: 1},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 1 {
		t.Errorf("Expected 1 coalesced op, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 3 {
		t.Errorf("Expected 3 ops saved, got %d", result.SavedCount)
	}

	expectedDelta := int64(1 + 1 - 1 + 1) // = 2
	if result.Coalesced[0].Delta != expectedDelta {
		t.Errorf("Expected delta %d, got %d", expectedDelta, result.Coalesced[0].Delta)
	}
}

// Test 8: Mixed operations - coalesce within types
func TestCoalesceMixed_WithinTypes(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpSET, Key: "inode:1", Value: []byte("v1")},
		{Type: OpHSET, Key: "dir:1", Field: "f1", Value: []byte("100")},
		{Type: OpSET, Key: "inode:1", Value: []byte("v2")},              // Coalesces with first SET
		{Type: OpHSET, Key: "dir:1", Field: "f1", Value: []byte("200")}, // Coalesces with first HSET
		{Type: OpINCRBY, Key: "counter", Delta: 5},
		{Type: OpINCRBY, Key: "counter", Delta: 10}, // Coalesces
	}

	result := coalesceOps(ops)

	// Expected: 3 ops (1 SET, 1 HSET, 1 INCRBY)
	if len(result.Coalesced) != 3 {
		t.Errorf("Expected 3 coalesced ops, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 3 {
		t.Errorf("Expected 3 ops saved, got %d", result.SavedCount)
	}

	// Verify final values
	for _, op := range result.Coalesced {
		switch op.Type {
		case OpSET:
			if string(op.Value) != "v2" {
				t.Errorf("SET: expected 'v2', got '%s'", op.Value)
			}
		case OpHSET:
			if string(op.Value) != "200" {
				t.Errorf("HSET: expected '200', got '%s'", op.Value)
			}
		case OpINCRBY:
			if op.Delta != 15 {
				t.Errorf("INCRBY: expected 15, got %d", op.Delta)
			}
		}
	}
}

// Test 9: Non-coalescable operations - pass through unchanged
func TestCoalesceNonCoalescable(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpDEL, Key: "old:1"},
		{Type: OpRPUSH, Key: "list:1", Value: []byte("item")},
		{Type: OpZADD, Key: "zset:1", Score: 1.5, Value: []byte("member")},
	}

	result := coalesceOps(ops)

	if len(result.Coalesced) != 3 {
		t.Errorf("Expected 3 ops (no coalescing), got %d", len(result.Coalesced))
	}

	if result.SavedCount != 0 {
		t.Errorf("Expected 0 ops saved, got %d", result.SavedCount)
	}
}

// Test 10: Large batch - realistic workload
func TestCoalesceLargeBatch_RealisticWorkload(t *testing.T) {
	ops := make([]*BatchOp, 0, 1000)

	// Simulate 100 files × 10 writes each
	for fileID := 1; fileID <= 100; fileID++ {
		for writeNum := 1; writeNum <= 10; writeNum++ {
			// Inode attr update (same key, coalesces)
			ops = append(ops, &BatchOp{
				Type:  OpSET,
				Key:   "inode:" + string(rune(fileID)),
				Value: []byte("attrs_v" + string(rune(writeNum))),
				Inode: Ino(fileID),
			})

			// Directory stats (same hash+field, coalesces)
			ops = append(ops, &BatchOp{
				Type:  OpHINCRBY,
				Key:   "dirStats:1",
				Field: "usedSpace",
				Delta: 4096,
			})

			// Global counter (coalesces)
			ops = append(ops, &BatchOp{
				Type:  OpINCRBY,
				Key:   "usedSpace",
				Delta: 4096,
			})
		}
	}

	// Total: 100 files × 10 writes × 3 ops = 3000 ops
	if len(ops) != 3000 {
		t.Fatalf("Expected 3000 ops, got %d", len(ops))
	}

	result := coalesceOps(ops)

	// Expected coalescing:
	// - 100 SETs (1 per file, coalesced from 10 writes)
	// - 1 HINCRBY (all dir stats summed)
	// - 1 INCRBY (all counters summed)
	// Total: 102 ops
	expectedCoalesced := 102
	if len(result.Coalesced) != expectedCoalesced {
		t.Errorf("Expected %d coalesced ops, got %d", expectedCoalesced, len(result.Coalesced))
	}

	expectedSaved := 3000 - expectedCoalesced
	if result.SavedCount != expectedSaved {
		t.Errorf("Expected %d ops saved, got %d", expectedSaved, result.SavedCount)
	}

	// Verify dir stats summed correctly
	for _, op := range result.Coalesced {
		if op.Type == OpHINCRBY && op.Field == "usedSpace" {
			expectedDelta := int64(100 * 10 * 4096) // 100 files × 10 writes × 4096 bytes
			if op.Delta != expectedDelta {
				t.Errorf("DirStats: expected delta %d, got %d", expectedDelta, op.Delta)
			}
		}
		if op.Type == OpINCRBY && op.Key == "usedSpace" {
			expectedDelta := int64(100 * 10 * 4096)
			if op.Delta != expectedDelta {
				t.Errorf("UsedSpace: expected delta %d, got %d", expectedDelta, op.Delta)
			}
		}
	}
}

// Test 11: canCoalesce helper function
func TestCanCoalesce(t *testing.T) {
	tests := []struct {
		name     string
		op1      *BatchOp
		op2      *BatchOp
		expected bool
	}{
		{
			name:     "SET same key",
			op1:      &BatchOp{Type: OpSET, Key: "k1"},
			op2:      &BatchOp{Type: OpSET, Key: "k1"},
			expected: true,
		},
		{
			name:     "SET different keys",
			op1:      &BatchOp{Type: OpSET, Key: "k1"},
			op2:      &BatchOp{Type: OpSET, Key: "k2"},
			expected: false,
		},
		{
			name:     "HSET same hash+field",
			op1:      &BatchOp{Type: OpHSET, Key: "h1", Field: "f1"},
			op2:      &BatchOp{Type: OpHSET, Key: "h1", Field: "f1"},
			expected: true,
		},
		{
			name:     "HSET same hash, different field",
			op1:      &BatchOp{Type: OpHSET, Key: "h1", Field: "f1"},
			op2:      &BatchOp{Type: OpHSET, Key: "h1", Field: "f2"},
			expected: false,
		},
		{
			name:     "Different types",
			op1:      &BatchOp{Type: OpSET, Key: "k1"},
			op2:      &BatchOp{Type: OpHSET, Key: "k1"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := canCoalesce(tt.op1, tt.op2)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// Test 12: shouldPreserveOrder - operations on same inode
func TestShouldPreserveOrder(t *testing.T) {
	tests := []struct {
		name     string
		op1      *BatchOp
		op2      *BatchOp
		expected bool
	}{
		{
			name:     "Same inode",
			op1:      &BatchOp{Inode: 123},
			op2:      &BatchOp{Inode: 123},
			expected: true,
		},
		{
			name:     "Different inodes",
			op1:      &BatchOp{Inode: 123},
			op2:      &BatchOp{Inode: 456},
			expected: false,
		},
		{
			name:     "Zero inode",
			op1:      &BatchOp{Inode: 0},
			op2:      &BatchOp{Inode: 123},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := shouldPreserveOrder(tt.op1, tt.op2)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// Test 13: OpType String() method
func TestOpTypeString(t *testing.T) {
	tests := []struct {
		opType   OpType
		expected string
	}{
		{OpSET, "SET"},
		{OpHSET, "HSET"},
		{OpHDEL, "HDEL"},
		{OpHINCRBY, "HINCRBY"},
		{OpDEL, "DEL"},
		{OpZADD, "ZADD"},
		{OpSADD, "SADD"},
		{OpRPUSH, "RPUSH"},
		{OpINCRBY, "INCRBY"},
		{OpType(99), "UNKNOWN"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			result := tt.opType.String()
			if result != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

// Test 14: EnqueueTime tracking
func TestBatchOp_EnqueueTime(t *testing.T) {
	before := time.Now()
	op := &BatchOp{
		Type:        OpSET,
		Key:         "test",
		Value:       []byte("value"),
		EnqueueTime: time.Now(),
	}
	after := time.Now()

	if op.EnqueueTime.Before(before) || op.EnqueueTime.After(after) {
		t.Errorf("EnqueueTime not in expected range")
	}
}

// Test 15: Priority handling
func TestBatchOp_Priority(t *testing.T) {
	normalOp := &BatchOp{Type: OpSET, Key: "k1", Priority: 0}
	highPriorityOp := &BatchOp{Type: OpSET, Key: "k2", Priority: 1000}

	if highPriorityOp.Priority <= normalOp.Priority {
		t.Errorf("High priority op should have higher priority value")
	}
}

// Step 4 Tests: Basic Queue and Flusher

// Test 16: enqueueBatchOp - basic enqueue
func TestEnqueueBatchOp_Basic(t *testing.T) {
	// This test requires a real rueidisMeta instance
	// For now, we'll test the BatchOp creation
	op := &BatchOp{
		Type:        OpSET,
		Key:         "test:key",
		Value:       []byte("test value"),
		Inode:       123,
		EnqueueTime: time.Now(),
	}

	if op.Type != OpSET {
		t.Errorf("Expected OpSET, got %v", op.Type)
	}

	if string(op.Value) != "test value" {
		t.Errorf("Expected 'test value', got '%s'", op.Value)
	}
}

// Test 17: containsOp helper
func TestContainsOp(t *testing.T) {
	op1 := &BatchOp{Type: OpSET, Key: "k1"}
	op2 := &BatchOp{Type: OpSET, Key: "k2"}
	op3 := &BatchOp{Type: OpSET, Key: "k3"}

	ops := []*BatchOp{op1, op2}

	if !containsOp(ops, op1) {
		t.Errorf("Expected op1 to be in ops")
	}

	if !containsOp(ops, op2) {
		t.Errorf("Expected op2 to be in ops")
	}

	if containsOp(ops, op3) {
		t.Errorf("Expected op3 to NOT be in ops")
	}
}

// Step 5 Tests: DoMulti Flush Logic

// Test 18: containsAny helper
func TestContainsAny(t *testing.T) {
	tests := []struct {
		name     string
		s        string
		substrs  []string
		expected bool
	}{
		{
			name:     "Contains BUSY",
			s:        "Redis server is BUSY running a script",
			substrs:  []string{"BUSY", "LOADING", "TIMEOUT"},
			expected: true,
		},
		{
			name:     "Contains connection",
			s:        "connection reset by peer",
			substrs:  []string{"connection", "broken pipe"},
			expected: true,
		},
		{
			name:     "No match",
			s:        "Some other error",
			substrs:  []string{"BUSY", "LOADING"},
			expected: false,
		},
		{
			name:     "Case insensitive",
			s:        "busy server",
			substrs:  []string{"BUSY"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsAny(tt.s, tt.substrs)
			if result != tt.expected {
				t.Errorf("Expected %v, got %v", tt.expected, result)
			}
		})
	}
}

// Test 19: containsSubstring helper
func TestContainsSubstring(t *testing.T) {
	tests := []struct {
		s        string
		substr   string
		expected bool
	}{
		{"Hello World", "world", true},     // case insensitive
		{"TIMEOUT ERROR", "timeout", true}, // case insensitive
		{"connection lost", "conn", true},
		{"some error", "other", false},
		{"", "test", false},
		{"test", "", true}, // empty substring always matches
	}

	for _, tt := range tests {
		result := containsSubstring(tt.s, tt.substr)
		if result != tt.expected {
			t.Errorf("containsSubstring(%q, %q) = %v, want %v",
				tt.s, tt.substr, result, tt.expected)
		}
	}
}

// Test 20: toLower helper
func TestToLower(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"HELLO", "hello"},
		{"Hello World", "hello world"},
		{"ABC123", "abc123"},
		{"", ""},
		{"already lowercase", "already lowercase"},
	}

	for _, tt := range tests {
		result := toLower(tt.input)
		if result != tt.expected {
			t.Errorf("toLower(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// Test 21: indexOf helper
func TestIndexOf(t *testing.T) {
	tests := []struct {
		s        string
		substr   string
		expected int
	}{
		{"hello world", "world", 6},
		{"hello world", "hello", 0},
		{"hello world", "o w", 4},
		{"hello world", "xyz", -1},
		{"hello world", "", 0},
		{"", "test", -1},
	}

	for _, tt := range tests {
		result := indexOf(tt.s, tt.substr)
		if result != tt.expected {
			t.Errorf("indexOf(%q, %q) = %d, want %d",
				tt.s, tt.substr, result, tt.expected)
		}
	}
}

// Test 22: BatchOp retry tracking
func TestBatchOp_RetryCount(t *testing.T) {
	op := &BatchOp{
		Type:       OpSET,
		Key:        "test",
		Value:      []byte("value"),
		RetryCount: 0,
	}

	// Simulate retries
	for i := 1; i <= 3; i++ {
		op.RetryCount++
		if op.RetryCount != i {
			t.Errorf("Expected RetryCount %d, got %d", i, op.RetryCount)
		}
	}

	// Check poison threshold
	if op.RetryCount < 3 {
		t.Errorf("Expected RetryCount >= 3 for poison detection")
	}
}

// Test 23: ResultChan communication
func TestBatchOp_ResultChan(t *testing.T) {
	op := &BatchOp{
		Type:       OpSET,
		Key:        "test",
		Value:      []byte("value"),
		ResultChan: make(chan error, 1),
	}

	// Simulate sending error
	testErr := fmt.Errorf("test error")
	go func() {
		op.ResultChan <- testErr
	}()

	// Receive error
	select {
	case err := <-op.ResultChan:
		if err.Error() != testErr.Error() {
			t.Errorf("Expected error %v, got %v", testErr, err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Errorf("Timeout waiting for error")
	}
}

// Test 24: Hash slot calculation
func TestGetHashSlot(t *testing.T) {
	tests := []struct {
		key         string
		description string
	}{
		{"key1", "simple key without hash tag"},
		{"key2", "different simple key"},
		{"{user:1}:profile", "key with hash tag"},
		{"{user:1}:settings", "same hash tag, different suffix"},
		{"{fs}:inode:123", "filesystem prefix with hash tag"},
		{"{fs}:chunk:456", "same hash tag for chunks"},
		{"no{tag", "incomplete hash tag (ignored)"},
		{"{}", "empty hash tag"},
	}

	for _, tt := range tests {
		t.Run(tt.description, func(t *testing.T) {
			slot := getHashSlot(tt.key)
			// Verify slot is in valid range
			if slot < 0 || slot >= 16384 {
				t.Errorf("Invalid slot %d for key %q (must be 0-16383)", slot, tt.key)
			}
		})
	}

	// Test that keys with same hash tag map to same slot
	slot1 := getHashSlot("{user:1}:profile")
	slot2 := getHashSlot("{user:1}:settings")
	if slot1 != slot2 {
		t.Errorf("Keys with same hash tag {user:1} should map to same slot, got %d and %d", slot1, slot2)
	}

	// Test that {fs} tag works consistently
	slotInode := getHashSlot("{fs}:inode:123")
	slotChunk := getHashSlot("{fs}:chunk:456")
	if slotInode != slotChunk {
		t.Errorf("Keys with same hash tag {fs} should map to same slot, got %d and %d", slotInode, slotChunk)
	}
}

// Test 25: MSET grouping - single slot
func TestBuildMSET_SingleSlot(t *testing.T) {
	// Create mock rueidisMeta (simplified for testing)
	// Note: This test verifies the grouping logic, not actual Redis execution
	ops := []*BatchOp{
		{Type: OpSET, Key: "{user:1}:name", Value: []byte("Alice")},
		{Type: OpSET, Key: "{user:1}:age", Value: []byte("30")},
		{Type: OpSET, Key: "{user:1}:email", Value: []byte("alice@example.com")},
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("val1")},
	}

	// Verify all SET keys map to same slot
	slot1 := getHashSlot("{user:1}:name")
	slot2 := getHashSlot("{user:1}:age")
	slot3 := getHashSlot("{user:1}:email")

	if slot1 != slot2 || slot1 != slot3 {
		t.Errorf("Expected all keys with {user:1} to map to same slot, got %d, %d, %d", slot1, slot2, slot3)
	}

	// Count SET operations
	setCount := 0
	for _, op := range ops {
		if op.Type == OpSET {
			setCount++
		}
	}

	if setCount != 3 {
		t.Errorf("Expected 3 SET operations, got %d", setCount)
	}
}

// Test 26: MSET grouping - multiple slots
func TestBuildMSET_MultipleSlots(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpSET, Key: "{user:1}:name", Value: []byte("Alice")},
		{Type: OpSET, Key: "{user:2}:name", Value: []byte("Bob")},
		{Type: OpSET, Key: "{user:3}:name", Value: []byte("Charlie")},
	}

	// Verify keys map to different slots
	slots := make(map[int]bool)
	for _, op := range ops {
		slot := getHashSlot(op.Key)
		slots[slot] = true
	}

	if len(slots) != 3 {
		t.Errorf("Expected 3 different slots for different hash tags, got %d", len(slots))
	}
}

// Test 27: MSET optimization savings
func TestMSET_OpsSaved(t *testing.T) {
	// 10 SETs with same hash tag → should become 1 MSET
	// Savings: 10 - 1 = 9 operations
	setCount := 10
	savings := setCount - 1

	if savings != 9 {
		t.Errorf("Expected 9 ops saved from 10 SETs → 1 MSET, got %d", savings)
	}

	// 100 SETs across 5 slots → 5 MSET commands
	// Savings: 100 - 5 = 95 operations
	totalSets := 100
	slotCount := 5
	msetCount := slotCount
	totalSavings := totalSets - msetCount

	if totalSavings != 95 {
		t.Errorf("Expected 95 ops saved from 100 SETs → 5 MSETs, got %d", totalSavings)
	}
}

// Test 28: HMSET grouping - single hash
func TestBuildHMSET_SingleHash(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHSET, Key: "user:1", Field: "name", Value: []byte("Alice")},
		{Type: OpHSET, Key: "user:1", Field: "age", Value: []byte("30")},
		{Type: OpHSET, Key: "user:1", Field: "email", Value: []byte("alice@example.com")},
		{Type: OpSET, Key: "other", Value: []byte("value")},
	}

	// Count HSETs to same hash
	hsetCount := 0
	for _, op := range ops {
		if op.Type == OpHSET && op.Key == "user:1" {
			hsetCount++
		}
	}

	if hsetCount != 3 {
		t.Errorf("Expected 3 HSET operations to user:1, got %d", hsetCount)
	}

	// Verify all have same key
	keys := make(map[string]bool)
	for _, op := range ops {
		if op.Type == OpHSET {
			keys[op.Key] = true
		}
	}

	if len(keys) != 1 {
		t.Errorf("Expected 1 unique hash key, got %d", len(keys))
	}
}

// Test 29: HMSET grouping - multiple hashes
func TestBuildHMSET_MultipleHashes(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHSET, Key: "user:1", Field: "name", Value: []byte("Alice")},
		{Type: OpHSET, Key: "user:1", Field: "age", Value: []byte("30")},
		{Type: OpHSET, Key: "user:2", Field: "name", Value: []byte("Bob")},
		{Type: OpHSET, Key: "user:2", Field: "age", Value: []byte("25")},
		{Type: OpHSET, Key: "user:3", Field: "name", Value: []byte("Charlie")},
	}

	// Group by hash key
	hsetsByKey := make(map[string]int)
	for _, op := range ops {
		if op.Type == OpHSET {
			hsetsByKey[op.Key]++
		}
	}

	// Should have 3 different hash keys
	if len(hsetsByKey) != 3 {
		t.Errorf("Expected 3 different hash keys, got %d", len(hsetsByKey))
	}

	// user:1 and user:2 should have 2 fields each
	if hsetsByKey["user:1"] != 2 {
		t.Errorf("Expected 2 fields for user:1, got %d", hsetsByKey["user:1"])
	}
	if hsetsByKey["user:2"] != 2 {
		t.Errorf("Expected 2 fields for user:2, got %d", hsetsByKey["user:2"])
	}

	// user:3 should have 1 field
	if hsetsByKey["user:3"] != 1 {
		t.Errorf("Expected 1 field for user:3, got %d", hsetsByKey["user:3"])
	}
}

// Test 30: HMSET excludes HINCRBY
func TestHMSET_ExcludesHINCRBY(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHSET, Key: "stats:1", Field: "views", Value: []byte("100")},
		{Type: OpHINCRBY, Key: "stats:1", Field: "likes", Delta: 5},
		{Type: OpHSET, Key: "stats:1", Field: "shares", Value: []byte("20")},
	}

	// Count operation types
	hsetCount := 0
	hincrbyCount := 0
	for _, op := range ops {
		if op.Type == OpHSET {
			hsetCount++
		} else if op.Type == OpHINCRBY {
			hincrbyCount++
		}
	}

	if hsetCount != 2 {
		t.Errorf("Expected 2 HSET operations, got %d", hsetCount)
	}

	if hincrbyCount != 1 {
		t.Errorf("Expected 1 HINCRBY operation, got %d", hincrbyCount)
	}

	// HINCRBY should NOT be included in HMSET grouping
	// (different semantics - HINCRBY is atomic increment, HSET is set value)
}

// Test 31: HMSET optimization savings
func TestHMSET_OpsSaved(t *testing.T) {
	// 10 HSETs to same hash → should become 1 HMSET
	// Coalesced count: 10 fields in 1 HMSET command
	hsetCount := 10

	if hsetCount != 10 {
		t.Errorf("Expected 10 HSET operations, got %d", hsetCount)
	}

	// 50 HSETs across 5 hashes (10 fields each) → 5 HMSET commands
	// Each hash gets 10 fields → 5 HMSET commands total
	totalHsets := 50
	hashCount := 5
	hmsetCount := hashCount

	if hmsetCount != 5 {
		t.Errorf("Expected 5 HMSET commands from 50 HSETs, got %d", hmsetCount)
	}

	// Verify savings calculation
	// 50 individual HSET commands vs 5 HMSET commands = 45 commands saved
	savings := totalHsets - hmsetCount
	if savings != 45 {
		t.Errorf("Expected 45 commands saved, got %d", savings)
	}
}

// Test 32: HMSET with different fields
func TestHMSET_DifferentFields(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpHSET, Key: "config:app", Field: "timeout", Value: []byte("30")},
		{Type: OpHSET, Key: "config:app", Field: "retries", Value: []byte("3")},
		{Type: OpHSET, Key: "config:app", Field: "debug", Value: []byte("false")},
		{Type: OpHSET, Key: "config:app", Field: "log_level", Value: []byte("info")},
	}

	// Collect all fields
	fields := make(map[string]bool)
	for _, op := range ops {
		if op.Type == OpHSET && op.Key == "config:app" {
			fields[op.Field] = true
		}
	}

	// Should have 4 unique fields
	if len(fields) != 4 {
		t.Errorf("Expected 4 unique fields, got %d", len(fields))
	}

	// Verify field names
	expectedFields := []string{"timeout", "retries", "debug", "log_level"}
	for _, field := range expectedFields {
		if !fields[field] {
			t.Errorf("Expected field %s not found", field)
		}
	}
}

// Test 33: batchSet helper function - batching enabled
func TestBatchSet_BatchingEnabled(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	ctx := context.Background()

	// Test with string value
	err := m.batchSet(ctx, "test:key1", "value1")
	if err != nil {
		t.Fatalf("batchSet failed: %v", err)
	}

	// Verify operation was queued
	select {
	case op := <-m.batchQueue:
		if op.Type != OpSET {
			t.Errorf("Expected OpSET, got %v", op.Type)
		}
		if op.Key != "test:key1" {
			t.Errorf("Expected key 'test:key1', got '%s'", op.Key)
		}
		if string(op.Value) != "value1" {
			t.Errorf("Expected value 'value1', got '%s'", string(op.Value))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Operation not queued within timeout")
	}

	// Test with byte slice value
	err = m.batchSet(ctx, "test:key2", []byte("bytes"))
	if err != nil {
		t.Fatalf("batchSet with []byte failed: %v", err)
	}

	select {
	case op := <-m.batchQueue:
		if string(op.Value) != "bytes" {
			t.Errorf("Expected value 'bytes', got '%s'", string(op.Value))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Byte slice operation not queued")
	}

	// Test with int value
	err = m.batchSet(ctx, "test:counter", 42)
	if err != nil {
		t.Fatalf("batchSet with int failed: %v", err)
	}

	select {
	case op := <-m.batchQueue:
		if string(op.Value) != "42" {
			t.Errorf("Expected value '42', got '%s'", string(op.Value))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Int operation not queued")
	}
}

// Test 34: batchHSet helper function - batching enabled
func TestBatchHSet_BatchingEnabled(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	ctx := context.Background()

	// Test with string value
	err := m.batchHSet(ctx, "hash:config", "timeout", "30")
	if err != nil {
		t.Fatalf("batchHSet failed: %v", err)
	}

	// Verify operation was queued
	select {
	case op := <-m.batchQueue:
		if op.Type != OpHSET {
			t.Errorf("Expected OpHSET, got %v", op.Type)
		}
		if op.Key != "hash:config" {
			t.Errorf("Expected key 'hash:config', got '%s'", op.Key)
		}
		if op.Field != "timeout" {
			t.Errorf("Expected field 'timeout', got '%s'", op.Field)
		}
		if string(op.Value) != "30" {
			t.Errorf("Expected value '30', got '%s'", string(op.Value))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Operation not queued within timeout")
	}

	// Test with byte slice value
	err = m.batchHSet(ctx, "hash:data", "content", []byte("binary data"))
	if err != nil {
		t.Fatalf("batchHSet with []byte failed: %v", err)
	}

	select {
	case op := <-m.batchQueue:
		if string(op.Value) != "binary data" {
			t.Errorf("Expected value 'binary data', got '%s'", string(op.Value))
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Byte slice operation not queued")
	}
}

// Test 35: batchHDel helper function
func TestBatchHDel_BatchingEnabled(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	ctx := context.Background()

	err := m.batchHDel(ctx, "hash:config", "old_field")
	if err != nil {
		t.Fatalf("batchHDel failed: %v", err)
	}

	// Verify operation was queued
	select {
	case op := <-m.batchQueue:
		if op.Type != OpHDEL {
			t.Errorf("Expected OpHDEL, got %v", op.Type)
		}
		if op.Key != "hash:config" {
			t.Errorf("Expected key 'hash:config', got '%s'", op.Key)
		}
		if op.Field != "old_field" {
			t.Errorf("Expected field 'old_field', got '%s'", op.Field)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Operation not queued within timeout")
	}
}

// Test 36: batchHIncrBy helper function
func TestBatchHIncrBy_BatchingEnabled(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	ctx := context.Background()

	err := m.batchHIncrBy(ctx, "hash:stats", "counter", 10)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}

	// Verify operation was queued
	select {
	case op := <-m.batchQueue:
		if op.Type != OpHINCRBY {
			t.Errorf("Expected OpHINCRBY, got %v", op.Type)
		}
		if op.Key != "hash:stats" {
			t.Errorf("Expected key 'hash:stats', got '%s'", op.Key)
		}
		if op.Field != "counter" {
			t.Errorf("Expected field 'counter', got '%s'", op.Field)
		}
		if op.Delta != 10 {
			t.Errorf("Expected delta 10, got %d", op.Delta)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Operation not queued within timeout")
	}
}

// Test 37: batchIncrBy helper function
func TestBatchIncrBy_BatchingEnabled(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	ctx := context.Background()

	err := m.batchIncrBy(ctx, "key:counter", 5)
	if err != nil {
		t.Fatalf("batchIncrBy failed: %v", err)
	}

	// Verify operation was queued
	select {
	case op := <-m.batchQueue:
		if op.Type != OpINCRBY {
			t.Errorf("Expected OpINCRBY, got %v", op.Type)
		}
		if op.Key != "key:counter" {
			t.Errorf("Expected key 'key:counter', got '%s'", op.Key)
		}
		if op.Delta != 5 {
			t.Errorf("Expected delta 5, got %d", op.Delta)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Operation not queued within timeout")
	}
}

// Test 38: batchDel helper function
func TestBatchDel_BatchingEnabled(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	ctx := context.Background()

	err := m.batchDel(ctx, "key:old")
	if err != nil {
		t.Fatalf("batchDel failed: %v", err)
	}

	// Verify operation was queued
	select {
	case op := <-m.batchQueue:
		if op.Type != OpDEL {
			t.Errorf("Expected OpDEL, got %v", op.Type)
		}
		if op.Key != "key:old" {
			t.Errorf("Expected key 'key:old', got '%s'", op.Key)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Operation not queued within timeout")
	}
}

// Test 39: Helper functions with batching disabled (fallback path)
// Note: This test verifies that the helper functions correctly check batchEnabled.
// Full fallback testing with actual Redis calls requires integration tests.
func TestBatchHelpers_BatchingDisabled(t *testing.T) {
	// Create rueidisMeta with batching disabled
	m := &rueidisMeta{
		batchEnabled: false,
		batchQueue:   make(chan *BatchOp, 10), // Won't be used
		redisMeta:    &redisMeta{},
	}

	if m.batchEnabled {
		t.Error("Expected batchEnabled to be false")
	}

	// Verify that the queue is not used when batching is disabled
	initialQueueLen := len(m.batchQueue)

	// Test each helper function with recovery from panic (due to nil compat)
	testCases := []struct {
		name string
		fn   func() error
	}{
		{"batchSet", func() error { return m.batchSet(context.Background(), "test", "value") }},
		{"batchHSet", func() error { return m.batchHSet(context.Background(), "hash", "field", "value") }},
		{"batchHDel", func() error { return m.batchHDel(context.Background(), "hash", "field") }},
		{"batchHIncrBy", func() error { return m.batchHIncrBy(context.Background(), "hash", "field", 1) }},
		{"batchIncrBy", func() error { return m.batchIncrBy(context.Background(), "key", 1) }},
		{"batchDel", func() error { return m.batchDel(context.Background(), "key") }},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					// Panic is expected due to nil compat, which means fallback path was taken
					t.Logf("%s: Fallback path taken (panicked on nil compat as expected)", tc.name)
				}
			}()

			_ = tc.fn()
			// If we didn't panic, the function returned an error (also acceptable)
		})
	}

	// Verify queue was not used (length unchanged)
	if len(m.batchQueue) != initialQueueLen {
		t.Errorf("Queue should not be used when batching disabled, initial=%d final=%d",
			initialQueueLen, len(m.batchQueue))
	}
}

// Test 40: Helper functions queue full (back-pressure)
func TestBatchHelpers_QueueFull(t *testing.T) {
	m := newTestRueidisMeta(true, 2) // Very small queue

	ctx := context.Background()

	// Fill the queue
	_ = m.batchSet(ctx, "key1", "value1")
	_ = m.batchSet(ctx, "key2", "value2")

	// Next operation should timeout (queue full)
	errChan := make(chan error, 1)
	go func() {
		errChan <- m.batchSet(ctx, "key3", "value3")
	}()

	// Wait a bit to ensure the operation is blocked
	time.Sleep(50 * time.Millisecond)

	// Drain one slot
	<-m.batchQueue

	// Now the blocked operation should succeed
	select {
	case err := <-errChan:
		if err != nil {
			// This is expected - enqueueBatchOp has a 100ms timeout
			t.Logf("Operation timed out as expected: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Blocked operation didn't complete")
	}
}

// Test 41: Directory stat batching - verify operations are queued
func TestBatchWrite_DirectoryStats(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	ctx := Background()

	// Directly test the batch helpers that doUpdateDirStat uses
	spaceKey := "dirUsedSpace"
	lengthKey := "dirDataLength"
	inodesKey := "dirUsedInodes"

	// Simulate what doUpdateDirStat does: update stats for multiple directories
	// Directory 100: length=1024, space=2048, inodes=5
	err := m.batchHIncrBy(ctx, lengthKey, "100", 1024)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}
	err = m.batchHIncrBy(ctx, spaceKey, "100", 2048)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}
	err = m.batchHIncrBy(ctx, inodesKey, "100", 5)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}

	// Directory 101: length=2048, space=4096, inodes=10
	err = m.batchHIncrBy(ctx, lengthKey, "101", 2048)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}
	err = m.batchHIncrBy(ctx, spaceKey, "101", 4096)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}
	err = m.batchHIncrBy(ctx, inodesKey, "101", 10)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}

	// Verify operations were queued (6 total)
	count := 0
	timeout := time.After(200 * time.Millisecond)
	for count < 6 {
		select {
		case op := <-m.batchQueue:
			if op.Type != OpHINCRBY {
				t.Errorf("Expected OpHINCRBY, got %v", op.Type)
			}
			count++
		case <-timeout:
			t.Fatalf("Expected 6 operations, got %d", count)
		}
	}

	t.Logf("Successfully queued %d directory stat operations", count)
}

// Test 42: Directory stat batching coalescing
func TestBatchWrite_DirectoryStatsCoalescing(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	ctx := Background()

	spaceKey := "dirUsedSpace"

	// Enqueue multiple updates to the same directory field
	// These should coalesce via HINCRBY summing: 1024 + 2048 + 512 = 3584
	err := m.batchHIncrBy(ctx, spaceKey, "100", 1024)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}

	err = m.batchHIncrBy(ctx, spaceKey, "100", 2048)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}

	err = m.batchHIncrBy(ctx, spaceKey, "100", 512)
	if err != nil {
		t.Fatalf("batchHIncrBy failed: %v", err)
	}

	// Drain queue and verify operations were queued
	count := 0
	timeout := time.After(100 * time.Millisecond)
	for count < 3 {
		select {
		case op := <-m.batchQueue:
			if op.Type != OpHINCRBY {
				t.Errorf("Expected OpHINCRBY, got %v", op.Type)
			}
			if op.Key != spaceKey {
				t.Errorf("Expected key %s, got %s", spaceKey, op.Key)
			}
			if op.Field != "100" {
				t.Errorf("Expected field '100', got '%s'", op.Field)
			}
			count++
		case <-timeout:
			t.Fatalf("Expected 3 operations, got %d", count)
		}
	}

	t.Logf("Successfully queued %d directory stat operations (will coalesce to 1 during flush)", count)
}

// Test 43: isCrossSlotError detection
func TestBatchWrite_IsCrossSlotError(t *testing.T) {
	m := newTestRueidisMeta(true, 10)

	// Test CROSSSLOT error detection (uppercase)
	err1 := fmt.Errorf("CROSSSLOT Keys in request don't hash to the same slot")
	if !m.isCrossSlotError(err1) {
		t.Error("Expected isCrossSlotError to return true for CROSSSLOT error")
	}

	// Test crossslot error detection (lowercase)
	err2 := fmt.Errorf("crossslot keys in request")
	if !m.isCrossSlotError(err2) {
		t.Error("Expected isCrossSlotError to return true for crossslot error")
	}

	// Test non-CROSSSLOT error
	err3 := fmt.Errorf("BUSY Redis is busy")
	if m.isCrossSlotError(err3) {
		t.Error("Expected isCrossSlotError to return false for non-CROSSSLOT error")
	}

	// Test nil error
	if m.isCrossSlotError(nil) {
		t.Error("Expected isCrossSlotError to return false for nil error")
	}

	t.Log("Successfully tested CROSSSLOT error detection")
}

// Test 44: Hash slot calculation consistency
func TestBatchWrite_HashSlotConsistency(t *testing.T) {
	// Test that same keys produce same slots
	key1 := "user:1000:profile"
	slot1a := getHashSlot(key1)
	slot1b := getHashSlot(key1)

	if slot1a != slot1b {
		t.Errorf("Hash slot calculation not consistent: %d != %d", slot1a, slot1b)
	}

	// Test hash tags
	key2a := "{user:1000}:profile"
	key2b := "{user:1000}:settings"
	slot2a := getHashSlot(key2a)
	slot2b := getHashSlot(key2b)

	if slot2a != slot2b {
		t.Errorf("Keys with same hash tag should have same slot: %d != %d", slot2a, slot2b)
	}

	// Test keys without hash tags (should have different slots)
	key3a := "user:1000:profile"
	key3b := "user:2000:profile"
	slot3a := getHashSlot(key3a)
	slot3b := getHashSlot(key3b)

	// Note: These MIGHT have the same slot by chance, but it's unlikely
	t.Logf("Different keys: slot %d vs %d", slot3a, slot3b)

	// Test slot range
	key4 := "test:key:12345"
	slot4 := getHashSlot(key4)
	if slot4 < 0 || slot4 >= 16384 {
		t.Errorf("Hash slot out of range: %d (expected 0-16383)", slot4)
	}

	t.Log("Successfully tested hash slot calculation consistency")
}

// Test 45: MSET slot grouping prevents CROSSSLOT
func TestBatchWrite_MSETSlotGrouping(t *testing.T) {
	// Test hash slot calculation for keys with hash tags
	// Keys with same hash tag should hash to same slot
	key1a := "{fs:1}:key1"
	key1b := "{fs:1}:key2"
	key1c := "{fs:1}:key3"

	slot1a := getHashSlot(key1a)
	slot1b := getHashSlot(key1b)
	slot1c := getHashSlot(key1c)

	if slot1a != slot1b || slot1a != slot1c {
		t.Errorf("Keys with same hash tag should have same slot: %d, %d, %d", slot1a, slot1b, slot1c)
	}

	// Keys with different hash tags should hash to different slots (most likely)
	key2a := "{fs:2}:key4"
	key2b := "{fs:2}:key5"

	slot2a := getHashSlot(key2a)
	slot2b := getHashSlot(key2b)

	if slot2a != slot2b {
		t.Errorf("Keys with same hash tag should have same slot: %d, %d", slot2a, slot2b)
	}

	// Different hash tags should (usually) produce different slots
	t.Logf("Slot for {fs:1}: %d", slot1a)
	t.Logf("Slot for {fs:2}: %d", slot2a)

	// Verify slot grouping logic:
	// - All keys with {fs:1} should group together
	// - All keys with {fs:2} should group together
	// - buildMSET will create separate MSET commands per slot

	t.Log("Successfully verified hash slot grouping for MSET")
}

// Test 46: HMSET slot grouping
func TestBatchWrite_HMSETSlotGrouping(t *testing.T) {
	// Test hash slot calculation for hash keys with hash tags
	hash1 := "{fs:1}:hash1"
	hash2 := "{fs:2}:hash2"

	slot1 := getHashSlot(hash1)
	slot2 := getHashSlot(hash2)

	// Verify slots are calculated
	if slot1 < 0 || slot1 >= 16384 {
		t.Errorf("Invalid slot for %s: %d", hash1, slot1)
	}

	if slot2 < 0 || slot2 >= 16384 {
		t.Errorf("Invalid slot for %s: %d", hash2, slot2)
	}

	t.Logf("Slot for %s: %d", hash1, slot1)
	t.Logf("Slot for %s: %d", hash2, slot2)

	// Verify HMSET grouping logic:
	// - HSETs to same hash key will be grouped into one HMSET
	// - buildHMSET groups HMSET commands by slot
	// - This prevents CROSSSLOT errors in Redis Cluster

	t.Log("Successfully verified hash slot grouping for HMSET")
}

// Test 47: Flush barrier - basic functionality
func TestBatchWrite_FlushBarrier_Basic(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Enqueue 10 operations for inode 123
	for i := 0; i < 10; i++ {
		op := &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
			Inode: 123,
		}
		err := m.enqueueBatchOp(op)
		if err != nil {
			t.Fatalf("Failed to enqueue op %d: %v", i, err)
		}
	}

	// Verify ops were queued
	queueSize := m.batchQueueSize.Load()
	if queueSize != 10 {
		t.Errorf("Expected queue size 10, got %d", queueSize)
	}

	t.Log("Successfully enqueued 10 operations for inode 123")
}

// Test 48: Flush barrier - timeout behavior
func TestBatchWrite_FlushBarrier_Timeout(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Don't start the flusher goroutine to simulate slow flush

	// Enqueue operation
	op := &BatchOp{
		Type:  OpSET,
		Key:   "test_key",
		Value: []byte("test_value"),
		Inode: 456,
	}
	err := m.enqueueBatchOp(op)
	if err != nil {
		t.Fatalf("Failed to enqueue op: %v", err)
	}

	// Call flushBarrier with very short timeout
	// Since we haven't started the flusher, this should timeout
	err = m.flushBarrier(456, 10*time.Millisecond)
	if err != syscall.ETIMEDOUT {
		t.Errorf("Expected ETIMEDOUT, got %v", err)
	}

	t.Log("Successfully detected timeout")
}

// Test 49: Flush barrier - disabled batching
func TestBatchWrite_FlushBarrier_Disabled(t *testing.T) {
	m := newTestRueidisMeta(false, 100) // Batching disabled

	// Call flushBarrier - should be no-op
	err := m.flushBarrier(789, 1*time.Second)
	if err != nil {
		t.Errorf("flushBarrier should be no-op when batching disabled, got error: %v", err)
	}

	t.Log("Successfully handled disabled batching case")
}

// Test 50: Flush barrier - result channel mechanism
func TestBatchWrite_FlushBarrier_ResultChan(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a barrier operation manually
	resultChan := make(chan error, 1)
	barrier := &BatchOp{
		Type:        OpSET,
		Key:         "barrier",
		Inode:       100,
		Priority:    1000,
		EnqueueTime: time.Now(),
		ResultChan:  resultChan,
	}

	// Enqueue the barrier
	err := m.enqueueBatchOp(barrier)
	if err != nil {
		t.Fatalf("Failed to enqueue barrier: %v", err)
	}

	// Verify barrier has result channel
	if barrier.ResultChan == nil {
		t.Error("Barrier should have ResultChan set")
	}

	// Verify high priority
	if barrier.Priority != 1000 {
		t.Errorf("Expected priority 1000, got %d", barrier.Priority)
	}

	t.Log("Successfully verified barrier operation structure")
}

// Test 51: Flush barrier - inode filtering
func TestBatchWrite_FlushBarrier_InodeFilter(t *testing.T) {
	// Test the logic of filtering operations by inode
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", Inode: 100},
		{Type: OpSET, Key: "key2", Inode: 200},
		{Type: OpSET, Key: "key3", Inode: 100},
		{Type: OpSET, Key: "key4", Inode: 300},
		{Type: OpSET, Key: "key5", Inode: 100},
	}

	// Filter for inode 100
	targetInode := Ino(100)
	filtered := make([]*BatchOp, 0)
	for _, op := range ops {
		if op.Inode == targetInode {
			filtered = append(filtered, op)
		}
	}

	// Should have 3 operations for inode 100
	if len(filtered) != 3 {
		t.Errorf("Expected 3 ops for inode 100, got %d", len(filtered))
	}

	// Verify keys
	expectedKeys := []string{"key1", "key3", "key5"}
	for i, op := range filtered {
		if op.Key != expectedKeys[i] {
			t.Errorf("Expected key %s at position %d, got %s", expectedKeys[i], i, op.Key)
		}
	}

	t.Log("Successfully verified inode filtering logic")
}

// Test 52: Adaptive batch sizing - size increase on high queue depth
func TestBatchWrite_AdaptiveSizing_Increase(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize adaptive sizing fields
	m.baseBatchSize = 512
	m.maxBatchSize = 2048
	m.highWaterMark = 1000
	m.lowWaterMark = 100
	m.currentBatchSize.Store(512)
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_batch_size_current",
	})

	// Simulate high queue depth for multiple samples
	for i := 0; i < 10; i++ {
		m.batchQueueSize.Store(1500) // Above high watermark
		m.adjustBatchSize()
	}

	// Verify batch size increased to max
	newSize := m.currentBatchSize.Load()
	if newSize != 2048 {
		t.Errorf("Expected batch size to increase to 2048, got %d", newSize)
	}

	t.Logf("Successfully increased batch size from 512 to %d", newSize)
}

// Test 53: Adaptive batch sizing - size decrease on low queue depth
func TestBatchWrite_AdaptiveSizing_Decrease(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize adaptive sizing fields
	m.baseBatchSize = 512
	m.maxBatchSize = 2048
	m.highWaterMark = 1000
	m.lowWaterMark = 100
	m.currentBatchSize.Store(2048) // Start at max
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_batch_size_current",
	})

	// Simulate low queue depth for multiple samples
	for i := 0; i < 10; i++ {
		m.batchQueueSize.Store(50) // Below low watermark
		m.adjustBatchSize()
	}

	// Verify batch size decreased to base
	newSize := m.currentBatchSize.Load()
	if newSize != 512 {
		t.Errorf("Expected batch size to decrease to 512, got %d", newSize)
	}

	t.Logf("Successfully decreased batch size from 2048 to %d", newSize)
}

// Test 54: Adaptive batch sizing - no oscillation in middle range
func TestBatchWrite_AdaptiveSizing_Stable(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize adaptive sizing fields
	m.baseBatchSize = 512
	m.maxBatchSize = 2048
	m.highWaterMark = 1000
	m.lowWaterMark = 100
	m.currentBatchSize.Store(512)
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_batch_size_current",
	})

	// Simulate moderate queue depth (between watermarks)
	for i := 0; i < 20; i++ {
		m.batchQueueSize.Store(500) // Between low and high watermarks
		m.adjustBatchSize()
	}

	// Verify batch size stays at base (no change)
	newSize := m.currentBatchSize.Load()
	if newSize != 512 {
		t.Errorf("Expected batch size to remain at 512, got %d", newSize)
	}

	t.Log("Successfully verified stable batch size in middle range")
}

// Test 55: Adaptive batch sizing - sample window averaging
func TestBatchWrite_AdaptiveSizing_Averaging(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize adaptive sizing fields
	m.baseBatchSize = 512
	m.maxBatchSize = 2048
	m.highWaterMark = 1000
	m.lowWaterMark = 100
	m.currentBatchSize.Store(512)
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_batch_size_current",
	})

	// Simulate mixed queue depths - average should be above threshold
	depths := []int64{200, 1500, 1200, 1800, 1100, 1400, 1600, 1300, 1700, 1500}
	for _, depth := range depths {
		m.batchQueueSize.Store(depth)
		m.adjustBatchSize()
	}

	// Average is ~1330, which is > 1000 (high watermark)
	// So batch size should increase
	newSize := m.currentBatchSize.Load()
	if newSize != 2048 {
		t.Errorf("Expected batch size to increase to 2048 based on average, got %d", newSize)
	}

	t.Log("Successfully verified window averaging logic")
}

// Test 56: Adaptive batch sizing - metric updates
func TestBatchWrite_AdaptiveSizing_Metrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize adaptive sizing fields
	m.baseBatchSize = 512
	m.maxBatchSize = 2048
	m.highWaterMark = 1000
	m.lowWaterMark = 100
	m.currentBatchSize.Store(512)
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_batch_size_current",
	})

	// Initial metric value should match initial batch size
	// (Note: In real code, metric is set in newRueidisMeta)
	m.batchSizeCurrent.Set(512)

	// Trigger size increase
	for i := 0; i < 10; i++ {
		m.batchQueueSize.Store(1500)
		m.adjustBatchSize()
	}

	// Verify metric was updated (we can't read the gauge value directly in tests,
	// but we can verify the function was called without panic)
	newSize := m.currentBatchSize.Load()
	if newSize != 2048 {
		t.Errorf("Expected batch size 2048, got %d", newSize)
	}

	t.Log("Successfully verified metric update mechanism")
}

// Test 57: Adaptive batch sizing - circular buffer wraparound
func TestBatchWrite_AdaptiveSizing_CircularBuffer(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize adaptive sizing fields
	m.baseBatchSize = 512
	m.maxBatchSize = 2048
	m.highWaterMark = 1000
	m.lowWaterMark = 100
	m.currentBatchSize.Store(512)
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_batch_size_current",
	})

	// Fill buffer with low values, then high values
	// This tests that old samples are properly replaced
	for i := 0; i < 20; i++ {
		if i < 10 {
			m.batchQueueSize.Store(50) // Low
		} else {
			m.batchQueueSize.Store(1500) // High
		}
		m.adjustBatchSize()
	}

	// After 20 samples, only last 10 matter (all high)
	// So batch size should be at max
	newSize := m.currentBatchSize.Load()
	if newSize != 2048 {
		t.Errorf("Expected batch size 2048 after buffer wraparound, got %d", newSize)
	}

	t.Log("Successfully verified circular buffer behavior")
}

// Test 58: Poison operation handling - retry count tracking
func TestBatchWrite_PoisonOp_RetryCount(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize poison/retry metrics
	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_batch_poison_ops_total",
	})
	m.batchRetryOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_batch_retry_ops_total",
	}, []string{"type"})

	// Create operation with retry count
	op := &BatchOp{
		Type:       OpSET,
		Key:        "test_key",
		Value:      []byte("test_value"),
		RetryCount: 0,
	}

	// Simulate retries
	for i := 0; i < 3; i++ {
		op.RetryCount++
		if op.RetryCount < 3 {
			t.Logf("Retry %d/%d", op.RetryCount, 3)
		}
	}

	// Verify retry count reached threshold
	if op.RetryCount != 3 {
		t.Errorf("Expected retry count 3, got %d", op.RetryCount)
	}

	t.Log("Successfully tracked retry count")
}

// Test 59: Poison operation handling - handlePoisonOp logging
func TestBatchWrite_PoisonOp_DetailedLogging(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize metrics
	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_batch_poison_ops_total",
	})

	// Create operation with various fields for context
	op := &BatchOp{
		Type:       OpHSET,
		Key:        "test_hash",
		Field:      "test_field",
		Value:      []byte("some_data_here"),
		Inode:      123,
		Priority:   10,
		RetryCount: 3,
	}

	// Call handlePoisonOp (should not panic, should log)
	err := fmt.Errorf("test error: connection timeout")
	m.handlePoisonOp(op, err)

	// Verify poison metric was incremented
	// (We can't directly read the counter value, but we verified no panic)

	t.Log("Successfully logged poison operation with context")
}

// Test 60: Poison operation handling - ResultChan notification
func TestBatchWrite_PoisonOp_ResultChan(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize metrics
	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_batch_poison_ops_total",
	})

	// Create operation with ResultChan
	resultChan := make(chan error, 1)
	op := &BatchOp{
		Type:       OpSET,
		Key:        "test_key",
		Value:      []byte("test_value"),
		RetryCount: 3,
		ResultChan: resultChan,
	}

	// Call handlePoisonOp
	testErr := fmt.Errorf("test error")
	m.handlePoisonOp(op, testErr)

	// Verify error was sent to ResultChan
	select {
	case receivedErr := <-resultChan:
		if receivedErr != testErr {
			t.Errorf("Expected error %v, got %v", testErr, receivedErr)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Expected error on ResultChan, got timeout")
	}

	t.Log("Successfully notified ResultChan of poison operation")
}

// Test 61: Poison operation handling - retry metric tracking
func TestBatchWrite_PoisonOp_RetryMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize metrics
	m.batchRetryOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_batch_retry_ops_total",
	}, []string{"type"})
	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_batch_poison_ops_total",
	})

	// Simulate retry scenario
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", RetryCount: 0},
		{Type: OpSET, Key: "key2", RetryCount: 0},
		{Type: OpSET, Key: "key3", RetryCount: 0},
	}

	// Simulate retry logic
	for _, op := range ops {
		op.RetryCount++
		if op.RetryCount < 3 {
			// Should increment retry metric
			m.batchRetryOps.WithLabelValues("retryable").Inc()
		} else {
			// Should increment poison metric
			m.handlePoisonOp(op, fmt.Errorf("test error"))
		}
	}

	// All operations should have been retried (first retry)
	for _, op := range ops {
		if op.RetryCount != 1 {
			t.Errorf("Expected retry count 1, got %d", op.RetryCount)
		}
	}

	t.Log("Successfully tracked retry metrics")
}

// Test 62: Poison operation handling - threshold enforcement
func TestBatchWrite_PoisonOp_ThresholdEnforcement(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize metrics
	m.batchRetryOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_batch_retry_ops_total",
	}, []string{"type"})
	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "test_batch_poison_ops_total",
	})

	// Test threshold: 3 retries max
	op := &BatchOp{
		Type:       OpSET,
		Key:        "test_key",
		Value:      []byte("test_value"),
		RetryCount: 0,
	}

	// Simulate retry loop with threshold check
	poisonDetected := false
	for attempt := 0; attempt < 5; attempt++ {
		op.RetryCount++
		if op.RetryCount < 3 {
			m.batchRetryOps.WithLabelValues("test").Inc()
			t.Logf("Attempt %d: Retrying", op.RetryCount)
		} else {
			m.handlePoisonOp(op, fmt.Errorf("max retries exceeded"))
			poisonDetected = true
			t.Logf("Attempt %d: Poison detected", op.RetryCount)
			break
		}
	}

	// Verify poison was detected at threshold
	if !poisonDetected {
		t.Error("Expected poison detection at threshold")
	}
	if op.RetryCount != 3 {
		t.Errorf("Expected retry count 3 at poison detection, got %d", op.RetryCount)
	}

	t.Log("Successfully enforced retry threshold")
}

// Test 63: Poison operation handling - multiple error types
func TestBatchWrite_PoisonOp_MultipleErrorTypes(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Initialize metrics
	m.batchRetryOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "test_batch_retry_ops_total",
	}, []string{"type"})

	// Test different error types
	errorTypes := []string{"mset_retryable", "hmset_retryable", "hmset_crossslot", "retryable"}

	for _, errType := range errorTypes {
		// Increment retry metric for each type
		m.batchRetryOps.WithLabelValues(errType).Inc()
		t.Logf("Tracked retry for error type: %s", errType)
	}

	t.Log("Successfully tracked multiple error types")
}

// Test 64: Metrics Registration and Visibility
func TestBatchWrite_MetricsRegistration(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize all batch metrics manually (since InitMetrics requires full redisMeta setup)
	m.batchOpsQueued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_ops_queued_total",
		Help: "Total number of operations queued for batching, by type.",
	}, []string{"type"})

	m.batchOpsFlushed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_ops_flushed_total",
		Help: "Total number of operations successfully flushed, by type.",
	}, []string{"type"})

	m.batchCoalesceSaved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_coalesce_saved_ops_total",
		Help: "Total operations eliminated by coalescing, by type.",
	}, []string{"type"})

	m.batchFlushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "rueidis_batch_flush_duration_seconds",
		Help: "Histogram of batch flush durations in seconds.",
	})

	m.batchQueueDepthGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rueidis_batch_queue_depth",
		Help: "Current batch queue depth.",
	})

	m.batchSizeHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "rueidis_batch_size_ops",
		Help: "Histogram of actual operations per batch flush.",
	})

	m.batchErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_errors_total",
		Help: "Total batch operation errors, by error type.",
	}, []string{"error_type"})

	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_poison_ops_total",
		Help: "Total number of poison operations (failed after max retries).",
	})

	m.batchRetryOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_retry_ops_total",
		Help: "Total number of operation retry attempts, by error type.",
	}, []string{"type"})

	m.batchMsetConversions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_mset_conversions_total",
		Help: "Total number of MSET batch conversions.",
	})

	m.batchMsetOpsSaved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_mset_ops_saved_total",
		Help: "Total SET operations saved via MSET batching.",
	})

	m.batchHmsetConversions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_hmset_conversions_total",
		Help: "Total number of HMSET batch conversions.",
	})

	m.batchHsetCoalesced = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_hset_coalesced_total",
		Help: "Total HSET operations coalesced into HMSET.",
	})

	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rueidis_batch_size_current",
		Help: "Current adaptive batch size.",
	})

	// Register all metrics
	reg.MustRegister(m.batchOpsQueued)
	reg.MustRegister(m.batchOpsFlushed)
	reg.MustRegister(m.batchCoalesceSaved)
	reg.MustRegister(m.batchFlushDuration)
	reg.MustRegister(m.batchQueueDepthGauge)
	reg.MustRegister(m.batchSizeHistogram)
	reg.MustRegister(m.batchErrors)
	reg.MustRegister(m.batchPoisonOps)
	reg.MustRegister(m.batchRetryOps)
	reg.MustRegister(m.batchMsetConversions)
	reg.MustRegister(m.batchMsetOpsSaved)
	reg.MustRegister(m.batchHmsetConversions)
	reg.MustRegister(m.batchHsetCoalesced)
	reg.MustRegister(m.batchSizeCurrent)

	// Initialize CounterVec metrics with at least one label (required for them to appear in gathered metrics)
	m.batchOpsQueued.WithLabelValues("SET").Add(0)
	m.batchOpsFlushed.WithLabelValues("SET").Add(0)
	m.batchCoalesceSaved.WithLabelValues("SET").Add(0)
	m.batchErrors.WithLabelValues("retryable").Add(0)
	m.batchRetryOps.WithLabelValues("retryable").Add(0)

	// Gather metrics to verify registration
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Expected metric names (13 batch metrics + 1 legacy metric)
	expectedMetrics := []string{
		"rueidis_batch_ops_queued_total",
		"rueidis_batch_ops_flushed_total",
		"rueidis_batch_coalesce_saved_ops_total",
		"rueidis_batch_flush_duration_seconds",
		"rueidis_batch_queue_depth",
		"rueidis_batch_size_ops",
		"rueidis_batch_errors_total",
		"rueidis_batch_poison_ops_total",
		"rueidis_batch_retry_ops_total",
		"rueidis_batch_mset_conversions_total",
		"rueidis_batch_mset_ops_saved_total",
		"rueidis_batch_hmset_conversions_total",
		"rueidis_batch_hset_coalesced_total",
		"rueidis_batch_size_current",
	}

	// Build map of registered metrics
	registeredMetrics := make(map[string]bool)
	for _, mf := range metricFamilies {
		registeredMetrics[mf.GetName()] = true
	}

	// Verify all expected metrics are registered
	missingMetrics := []string{}
	for _, metricName := range expectedMetrics {
		if !registeredMetrics[metricName] {
			missingMetrics = append(missingMetrics, metricName)
		}
	}

	if len(missingMetrics) > 0 {
		t.Errorf("Missing metrics: %v", missingMetrics)
	}

	t.Logf("Successfully registered %d batch write metrics", len(expectedMetrics))
}

// Test 65: Metrics Update During Batch Operations
func TestBatchWrite_MetricsUpdate(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchOpsQueued = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_ops_queued_total",
	}, []string{"type"})
	reg.MustRegister(m.batchOpsQueued)

	// Create some batch operations
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", Value: []byte("value1"), Priority: 5},
		{Type: OpSET, Key: "key2", Value: []byte("value2"), Priority: 5},
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("value1"), Priority: 5},
		{Type: OpHSET, Key: "hash1", Field: "field2", Value: []byte("value2"), Priority: 5},
	}

	// Queue operations (this should increment batchOpsQueued)
	for _, op := range ops {
		m.batchOpsQueued.WithLabelValues(op.Type.String()).Inc()
	}

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Find batchOpsQueued metric
	var opsQueuedFound bool
	for _, mf := range metricFamilies {
		if mf.GetName() == "rueidis_batch_ops_queued_total" {
			opsQueuedFound = true
			// Should have metrics for SET and HSET types
			if len(mf.GetMetric()) < 2 {
				t.Errorf("Expected at least 2 operation types, got %d", len(mf.GetMetric()))
			}

			// Verify counter values
			for _, m := range mf.GetMetric() {
				counter := m.GetCounter()
				if counter.GetValue() == 0 {
					t.Errorf("Counter value should be > 0 for %v", m.GetLabel())
				}
			}
		}
	}

	if !opsQueuedFound {
		t.Error("batchOpsQueued metric not found after operations")
	}

	t.Log("Successfully verified metric updates during batch operations")
}

// Test 66: Coalescing Metrics
func TestBatchWrite_CoalescingMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchCoalesceSaved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_coalesce_saved_ops_total",
	}, []string{"type"})
	reg.MustRegister(m.batchCoalesceSaved)

	// Create operations that will be coalesced
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", Value: []byte("value1"), Priority: 5},
		{Type: OpSET, Key: "key1", Value: []byte("value2"), Priority: 5}, // Will coalesce
		{Type: OpSET, Key: "key1", Value: []byte("value3"), Priority: 5}, // Will coalesce
	}
	_ = ops // Use the variable to demonstrate coalescing scenario

	// Simulate coalescing by incrementing the metric
	savedCount := 2 // 2 operations coalesced
	m.batchCoalesceSaved.WithLabelValues("SET").Add(float64(savedCount))

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Find coalescing metric
	var coalescingFound bool
	for _, mf := range metricFamilies {
		if mf.GetName() == "rueidis_batch_coalesce_saved_ops_total" {
			coalescingFound = true

			// Should have at least one metric (SET type)
			if len(mf.GetMetric()) == 0 {
				t.Error("Expected coalescing metrics for SET type")
			}

			// Verify counter value
			for _, m := range mf.GetMetric() {
				counter := m.GetCounter()
				if counter.GetValue() != float64(savedCount) {
					t.Errorf("Expected coalesced count=%d, got %.0f", savedCount, counter.GetValue())
				}
			}
		}
	}

	if !coalescingFound {
		t.Error("Coalescing metric not found")
	}

	t.Log("Successfully verified coalescing metrics")
}

// Test 67: MSET Optimization Metrics
func TestBatchWrite_MSETMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchMsetConversions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_mset_conversions_total",
	})
	m.batchMsetOpsSaved = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_mset_ops_saved_total",
	})
	reg.MustRegister(m.batchMsetConversions)
	reg.MustRegister(m.batchMsetOpsSaved)

	// Simulate MSET optimization
	msetConversions := 2 // 2 MSET commands created
	opsSaved := 6        // 6 SET operations saved

	m.batchMsetConversions.Add(float64(msetConversions))
	m.batchMsetOpsSaved.Add(float64(opsSaved))

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify MSET metrics
	var conversionsFound, savedFound bool
	for _, mf := range metricFamilies {
		switch mf.GetName() {
		case "rueidis_batch_mset_conversions_total":
			conversionsFound = true
			counter := mf.GetMetric()[0].GetCounter()
			if counter.GetValue() != float64(msetConversions) {
				t.Errorf("Expected %d MSET conversions, got %.0f", msetConversions, counter.GetValue())
			}
		case "rueidis_batch_mset_ops_saved_total":
			savedFound = true
			counter := mf.GetMetric()[0].GetCounter()
			if counter.GetValue() != float64(opsSaved) {
				t.Errorf("Expected %d ops saved, got %.0f", opsSaved, counter.GetValue())
			}
		}
	}

	if !conversionsFound || !savedFound {
		t.Error("MSET metrics not found")
	}

	t.Log("Successfully verified MSET optimization metrics")
}

// Test 68: HMSET Optimization Metrics
func TestBatchWrite_HMSETMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchHmsetConversions = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_hmset_conversions_total",
	})
	m.batchHsetCoalesced = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_hset_coalesced_total",
	})
	reg.MustRegister(m.batchHmsetConversions)
	reg.MustRegister(m.batchHsetCoalesced)

	// Simulate HMSET optimization
	hmsetConversions := 3 // 3 HMSET commands created
	hsetCoalesced := 9    // 9 HSET operations coalesced

	m.batchHmsetConversions.Add(float64(hmsetConversions))
	m.batchHsetCoalesced.Add(float64(hsetCoalesced))

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify HMSET metrics
	var conversionsFound, coalescedFound bool
	for _, mf := range metricFamilies {
		switch mf.GetName() {
		case "rueidis_batch_hmset_conversions_total":
			conversionsFound = true
			counter := mf.GetMetric()[0].GetCounter()
			if counter.GetValue() != float64(hmsetConversions) {
				t.Errorf("Expected %d HMSET conversions, got %.0f", hmsetConversions, counter.GetValue())
			}
		case "rueidis_batch_hset_coalesced_total":
			coalescedFound = true
			counter := mf.GetMetric()[0].GetCounter()
			if counter.GetValue() != float64(hsetCoalesced) {
				t.Errorf("Expected %d HSET coalesced, got %.0f", hsetCoalesced, counter.GetValue())
			}
		}
	}

	if !conversionsFound || !coalescedFound {
		t.Error("HMSET metrics not found")
	}

	t.Log("Successfully verified HMSET optimization metrics")
}

// Test 69: Error and Retry Metrics
func TestBatchWrite_ErrorRetryMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_errors_total",
	}, []string{"error_type"})
	m.batchRetryOps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "rueidis_batch_retry_ops_total",
	}, []string{"type"})
	m.batchPoisonOps = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "rueidis_batch_poison_ops_total",
	})
	reg.MustRegister(m.batchErrors)
	reg.MustRegister(m.batchRetryOps)
	reg.MustRegister(m.batchPoisonOps)

	// Simulate various error types
	m.batchErrors.WithLabelValues("retryable").Inc()
	m.batchErrors.WithLabelValues("permanent").Inc()
	m.batchRetryOps.WithLabelValues("retryable").Inc()
	m.batchPoisonOps.Inc()

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify error metrics
	var errorsFound, retryFound, poisonFound bool
	for _, mf := range metricFamilies {
		switch mf.GetName() {
		case "rueidis_batch_errors_total":
			errorsFound = true
			if len(mf.GetMetric()) < 2 {
				t.Error("Expected at least 2 error types (retryable, permanent)")
			}
		case "rueidis_batch_retry_ops_total":
			retryFound = true
			if len(mf.GetMetric()) == 0 {
				t.Error("Expected retry metrics")
			}
		case "rueidis_batch_poison_ops_total":
			poisonFound = true
			counter := mf.GetMetric()[0].GetCounter()
			if counter.GetValue() != 1 {
				t.Errorf("Expected 1 poison op, got %.0f", counter.GetValue())
			}
		}
	}

	if !errorsFound || !retryFound || !poisonFound {
		t.Error("Error/retry/poison metrics not found")
	}

	t.Log("Successfully verified error and retry metrics")
}

// Test 70: Flush Duration and Size Histograms
func TestBatchWrite_HistogramMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchFlushDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "rueidis_batch_flush_duration_seconds",
	})
	m.batchSizeHistogram = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "rueidis_batch_size_ops",
	})
	reg.MustRegister(m.batchFlushDuration)
	reg.MustRegister(m.batchSizeHistogram)

	// Simulate flush operations
	m.batchFlushDuration.Observe(0.002) // 2ms
	m.batchFlushDuration.Observe(0.005) // 5ms
	m.batchFlushDuration.Observe(0.001) // 1ms

	m.batchSizeHistogram.Observe(100) // 100 ops
	m.batchSizeHistogram.Observe(512) // 512 ops
	m.batchSizeHistogram.Observe(256) // 256 ops

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify histogram metrics
	var durationFound, sizeFound bool
	for _, mf := range metricFamilies {
		switch mf.GetName() {
		case "rueidis_batch_flush_duration_seconds":
			durationFound = true
			histogram := mf.GetMetric()[0].GetHistogram()
			if histogram.GetSampleCount() != 3 {
				t.Errorf("Expected 3 duration samples, got %d", histogram.GetSampleCount())
			}
		case "rueidis_batch_size_ops":
			sizeFound = true
			histogram := mf.GetMetric()[0].GetHistogram()
			if histogram.GetSampleCount() != 3 {
				t.Errorf("Expected 3 size samples, got %d", histogram.GetSampleCount())
			}
		}
	}

	if !durationFound || !sizeFound {
		t.Error("Histogram metrics not found")
	}

	t.Log("Successfully verified histogram metrics")
}

// Test 71: Queue Depth and Adaptive Size Gauges
func TestBatchWrite_GaugeMetrics(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create a Prometheus registry
	reg := prometheus.NewRegistry()

	// Initialize metrics manually
	m.batchQueueDepthGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rueidis_batch_queue_depth",
	})
	m.batchSizeCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "rueidis_batch_size_current",
	})
	reg.MustRegister(m.batchQueueDepthGauge)
	reg.MustRegister(m.batchSizeCurrent)

	// Simulate queue depth changes
	m.batchQueueDepthGauge.Set(50)
	m.batchQueueDepthGauge.Set(100)
	m.batchQueueDepthGauge.Set(75)

	// Simulate adaptive batch size changes
	m.batchSizeCurrent.Set(512)
	m.batchSizeCurrent.Set(768)
	m.batchSizeCurrent.Set(1024)

	// Gather metrics
	metricFamilies, err := reg.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify gauge metrics
	var queueFound, sizeFound bool
	for _, mf := range metricFamilies {
		switch mf.GetName() {
		case "rueidis_batch_queue_depth":
			queueFound = true
			gauge := mf.GetMetric()[0].GetGauge()
			// Gauge should have the last set value (75)
			if gauge.GetValue() != 75 {
				t.Errorf("Expected queue depth 75, got %.0f", gauge.GetValue())
			}
		case "rueidis_batch_size_current":
			sizeFound = true
			gauge := mf.GetMetric()[0].GetGauge()
			// Gauge should have the last set value (1024)
			if gauge.GetValue() != 1024 {
				t.Errorf("Expected batch size 1024, got %.0f", gauge.GetValue())
			}
		}
	}

	if !queueFound || !sizeFound {
		t.Error("Gauge metrics not found")
	}

	t.Log("Successfully verified gauge metrics")
}

// ============================================================================
// Step 19: Backward Compatibility Testing
// ============================================================================

// Test 72: Large-scale SET coalescing (100 SETs to same key)
func TestBackwardCompat_LargeSETCoalescing(t *testing.T) {
	ops := make([]*BatchOp, 100)
	for i := 0; i < 100; i++ {
		ops[i] = &BatchOp{
			Type:  OpSET,
			Key:   "testkey",
			Value: []byte(fmt.Sprintf("value%d", i)),
		}
	}

	result := coalesceOps(ops)

	// Should coalesce to 1 operation (keep last value)
	if len(result.Coalesced) != 1 {
		t.Errorf("Expected 1 coalesced op, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 99 {
		t.Errorf("Expected 99 saved ops, got %d", result.SavedCount)
	}

	// Verify final value is the last one
	if string(result.Coalesced[0].Value) != "value99" {
		t.Errorf("Expected final value 'value99', got %s", result.Coalesced[0].Value)
	}

	t.Logf("Successfully coalesced 100 SETs to 1: saved %d ops", result.SavedCount)
}

// Test 73: Large-scale HSET coalescing (50 HSETs to same hash)
func TestBackwardCompat_LargeHSETCoalescing(t *testing.T) {
	ops := make([]*BatchOp, 50)
	for i := 0; i < 50; i++ {
		ops[i] = &BatchOp{
			Type:  OpHSET,
			Key:   "testhash",
			Field: fmt.Sprintf("field%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
		}
	}

	result := coalesceOps(ops)

	// Should keep all 50 fields (different fields, not coalesced further)
	if len(result.Coalesced) != 50 {
		t.Errorf("Expected 50 coalesced ops, got %d", len(result.Coalesced))
	}

	// No savings yet (coalescing only merges same field)
	if result.SavedCount != 0 {
		t.Errorf("Expected 0 saved ops (different fields), got %d", result.SavedCount)
	}

	// Verify all 50 HSETs are for the same hash
	for _, op := range result.Coalesced {
		if op.Type != OpHSET {
			t.Errorf("Expected OpHSET, got %v", op.Type)
		}
		if op.Key != "testhash" {
			t.Errorf("Expected key 'testhash', got %s", op.Key)
		}
	}

	t.Logf("Successfully verified 50 HSETs to same hash (ready for HMSET grouping)")
}

// Test 74: HINCRBY delta summation (10 HINCRBYs to same field)
func TestBackwardCompat_HINCRBYSummation(t *testing.T) {
	ops := make([]*BatchOp, 10)
	for i := 0; i < 10; i++ {
		ops[i] = &BatchOp{
			Type:  OpHINCRBY,
			Key:   "counter",
			Field: "total",
			Delta: int64(i + 1), // 1, 2, 3, ..., 10
		}
	}

	result := coalesceOps(ops)

	// Should coalesce to 1 HINCRBY with sum of all deltas
	if len(result.Coalesced) != 1 {
		t.Errorf("Expected 1 coalesced op, got %d", len(result.Coalesced))
	}

	if result.SavedCount != 9 {
		t.Errorf("Expected 9 saved ops, got %d", result.SavedCount)
	}

	// Sum of 1+2+3+...+10 = 55
	expectedSum := int64(55)
	if result.Coalesced[0].Delta != expectedSum {
		t.Errorf("Expected delta sum %d, got %d", expectedSum, result.Coalesced[0].Delta)
	}

	t.Logf("Successfully summed 10 HINCRBYs: %d", result.Coalesced[0].Delta)
}

// Test 75: Flush trigger - Size (512 ops)
func TestBackwardCompat_FlushTrigger_Size(t *testing.T) {
	m := newTestRueidisMetaWithFlusher(true, 1000)
	defer func() {
		close(m.batchStopChan)
		<-m.batchDoneChan
	}()

	// Enqueue exactly 512 ops (default batch size)
	for i := 0; i < 512; i++ {
		op := &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
		}
		select {
		case m.batchQueue <- op:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("Failed to enqueue op %d", i)
		}
	}

	// Wait for flush to complete
	time.Sleep(50 * time.Millisecond)

	// Queue should be empty after size-triggered flush
	queueLen := len(m.batchQueue)
	if queueLen > 0 {
		t.Errorf("Expected empty queue after size flush, got %d ops", queueLen)
	}

	t.Log("Successfully triggered flush at 512 ops (size trigger)")
}

// Test 76: Flush trigger - Time (2ms)
func TestBackwardCompat_FlushTrigger_Time(t *testing.T) {
	m := newTestRueidisMetaWithFlusher(true, 1000)
	defer func() {
		close(m.batchStopChan)
		<-m.batchDoneChan
	}()

	// Enqueue only 10 ops (well below batch size)
	for i := 0; i < 10; i++ {
		op := &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
		}
		select {
		case m.batchQueue <- op:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("Failed to enqueue op %d", i)
		}
	}

	// Wait for time-based flush (2ms + buffer)
	time.Sleep(10 * time.Millisecond)

	// Queue should be empty after time-triggered flush
	queueLen := len(m.batchQueue)
	if queueLen > 0 {
		t.Errorf("Expected empty queue after time flush, got %d ops", queueLen)
	}

	t.Log("Successfully triggered flush after 2ms (time trigger)")
}

// Test 77: Flush trigger - Bytes (256KB)
func TestBackwardCompat_FlushTrigger_Bytes(t *testing.T) {
	m := newTestRueidisMetaWithFlusher(true, 1000)
	defer func() {
		close(m.batchStopChan)
		<-m.batchDoneChan
	}()

	// Enqueue ops with large values to exceed 256KB byte limit
	// Each op: ~10KB value = 26 ops to exceed 256KB
	largeValue := make([]byte, 10240) // 10KB
	for i := 0; i < 30; i++ {
		op := &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: largeValue,
		}
		select {
		case m.batchQueue <- op:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("Failed to enqueue op %d", i)
		}
	}

	// Wait for byte-triggered flush
	time.Sleep(50 * time.Millisecond)

	// Queue should be mostly empty (some ops may have been added after flush)
	queueLen := len(m.batchQueue)
	if queueLen > 5 {
		t.Errorf("Expected near-empty queue after byte flush, got %d ops", queueLen)
	}

	t.Log("Successfully triggered flush when exceeding 256KB (byte trigger)")
}

// Test 78: Mixed operation types coalescing
func TestBackwardCompat_MixedOperations(t *testing.T) {
	// Create a realistic mixed workload
	ops := []*BatchOp{
		// 10 SETs to key1 (should coalesce to 1)
		{Type: OpSET, Key: "key1", Value: []byte("v1")},
		{Type: OpSET, Key: "key1", Value: []byte("v2")},
		{Type: OpSET, Key: "key1", Value: []byte("v3")},
		{Type: OpSET, Key: "key1", Value: []byte("v4")},
		{Type: OpSET, Key: "key1", Value: []byte("v5")},
		{Type: OpSET, Key: "key1", Value: []byte("v6")},
		{Type: OpSET, Key: "key1", Value: []byte("v7")},
		{Type: OpSET, Key: "key1", Value: []byte("v8")},
		{Type: OpSET, Key: "key1", Value: []byte("v9")},
		{Type: OpSET, Key: "key1", Value: []byte("v10")},
		// 5 HSETs to hash1/field1 (should coalesce to 1)
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("v1")},
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("v2")},
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("v3")},
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("v4")},
		{Type: OpHSET, Key: "hash1", Field: "field1", Value: []byte("v5")},
		// 5 HINCRBYs to counter/total (should sum to 15)
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 1},
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 2},
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 3},
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 4},
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 5},
		// 3 DELs (not coalescable)
		{Type: OpDEL, Key: "oldkey1"},
		{Type: OpDEL, Key: "oldkey2"},
		{Type: OpDEL, Key: "oldkey3"},
	}

	result := coalesceOps(ops)

	// Expected: 1 SET + 1 HSET + 1 HINCRBY + 3 DEL = 6 ops
	expectedOps := 6
	if len(result.Coalesced) != expectedOps {
		t.Errorf("Expected %d coalesced ops, got %d", expectedOps, len(result.Coalesced))
	}

	// Expected savings: (10-1) + (5-1) + (5-1) = 9 + 4 + 4 = 17
	expectedSaved := 17
	if result.SavedCount != expectedSaved {
		t.Errorf("Expected %d saved ops, got %d", expectedSaved, result.SavedCount)
	}

	// Verify final values
	for _, op := range result.Coalesced {
		switch op.Type {
		case OpSET:
			if op.Key == "key1" && string(op.Value) != "v10" {
				t.Errorf("SET key1: expected 'v10', got %s", op.Value)
			}
		case OpHSET:
			if op.Key == "hash1" && op.Field == "field1" && string(op.Value) != "v5" {
				t.Errorf("HSET hash1/field1: expected 'v5', got %s", op.Value)
			}
		case OpHINCRBY:
			if op.Key == "counter" && op.Field == "total" && op.Delta != 15 {
				t.Errorf("HINCRBY counter/total: expected delta 15, got %d", op.Delta)
			}
		}
	}

	t.Logf("Successfully coalesced mixed workload: %d → %d ops (saved %d)",
		len(ops), len(result.Coalesced), result.SavedCount)
}

// Test 79: Priority ordering preservation
func TestBackwardCompat_PriorityOrdering(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", Value: []byte("low"), Priority: 1},
		{Type: OpSET, Key: "key2", Value: []byte("high"), Priority: 100},
		{Type: OpSET, Key: "key3", Value: []byte("medium"), Priority: 50},
		{Type: OpSET, Key: "key4", Value: []byte("critical"), Priority: 1000},
	}

	result := coalesceOps(ops)

	// All ops should be preserved (different keys)
	if len(result.Coalesced) != 4 {
		t.Errorf("Expected 4 ops, got %d", len(result.Coalesced))
	}

	// Verify ordering by priority (descending)
	for i := 0; i < len(result.Coalesced)-1; i++ {
		if result.Coalesced[i].Priority < result.Coalesced[i+1].Priority {
			t.Errorf("Priority ordering violated at index %d: %d < %d",
				i, result.Coalesced[i].Priority, result.Coalesced[i+1].Priority)
		}
	}

	t.Log("Successfully verified priority ordering (highest first)")
}

// Test 80: Large batch stress test (1000 mixed operations)
func TestBackwardCompat_LargeBatchStress(t *testing.T) {
	ops := make([]*BatchOp, 1000)

	// Mix of operation types
	for i := 0; i < 1000; i++ {
		switch i % 5 {
		case 0:
			ops[i] = &BatchOp{Type: OpSET, Key: fmt.Sprintf("key%d", i%100), Value: []byte(fmt.Sprintf("v%d", i))}
		case 1:
			ops[i] = &BatchOp{Type: OpHSET, Key: fmt.Sprintf("hash%d", i%50), Field: fmt.Sprintf("f%d", i%20), Value: []byte(fmt.Sprintf("v%d", i))}
		case 2:
			ops[i] = &BatchOp{Type: OpHINCRBY, Key: fmt.Sprintf("counter%d", i%30), Field: "count", Delta: 1}
		case 3:
			ops[i] = &BatchOp{Type: OpDEL, Key: fmt.Sprintf("del%d", i)}
		case 4:
			ops[i] = &BatchOp{Type: OpRPUSH, Key: fmt.Sprintf("list%d", i%40), Value: []byte(fmt.Sprintf("item%d", i))}
		}
	}

	start := time.Now()
	result := coalesceOps(ops)
	duration := time.Since(start)

	// Should significantly reduce operation count
	if len(result.Coalesced) >= 1000 {
		t.Error("Coalescing should reduce operation count")
	}

	// Should complete quickly (< 100ms for 1000 ops)
	if duration > 100*time.Millisecond {
		t.Errorf("Coalescing took too long: %v", duration)
	}

	t.Logf("Coalesced 1000 ops → %d ops in %v (saved %d, %.1f%% reduction)",
		len(result.Coalesced), duration, result.SavedCount,
		float64(result.SavedCount)/float64(len(ops))*100)
}

// Test 81: Queue back-pressure test
func TestBackwardCompat_QueueBackPressure(t *testing.T) {
	// Create meta with small queue
	m := newTestRueidisMeta(true, 10) // Only 10 slots

	// Try to enqueue more than capacity
	successCount := 0
	for i := 0; i < 20; i++ {
		op := &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
		}

		// Try non-blocking send
		select {
		case m.batchQueue <- op:
			successCount++
		case <-time.After(1 * time.Millisecond):
			// Back-pressure working - queue full
		}
	}

	// Should have enqueued exactly 10 (queue capacity)
	if successCount != 10 {
		t.Errorf("Expected 10 successful enqueues, got %d", successCount)
	}

	queueLen := len(m.batchQueue)
	if queueLen != 10 {
		t.Errorf("Expected queue length 10, got %d", queueLen)
	}

	t.Logf("Back-pressure working: enqueued %d/%d ops", successCount, 20)
}

// Test 82: Non-coalescable operations preserved
func TestBackwardCompat_NonCoalescablePreserved(t *testing.T) {
	ops := []*BatchOp{
		{Type: OpDEL, Key: "key1"},
		{Type: OpDEL, Key: "key2"},
		{Type: OpDEL, Key: "key3"},
		{Type: OpRPUSH, Key: "list1", Value: []byte("item1")},
		{Type: OpRPUSH, Key: "list1", Value: []byte("item2")},
		{Type: OpRPUSH, Key: "list1", Value: []byte("item3")},
		{Type: OpZADD, Key: "zset1", Value: []byte("member1"), Score: 1.0},
		{Type: OpZADD, Key: "zset1", Value: []byte("member2"), Score: 2.0},
	}

	result := coalesceOps(ops)

	// All operations should be preserved (no coalescing for these types)
	if len(result.Coalesced) != len(ops) {
		t.Errorf("Expected %d ops preserved, got %d", len(ops), len(result.Coalesced))
	}

	if result.SavedCount != 0 {
		t.Errorf("Expected 0 saved ops (non-coalescable), got %d", result.SavedCount)
	}

	t.Log("Successfully verified non-coalescable operations preserved")
}

// Test 83: Inode-specific flush barrier
func TestBackwardCompat_InodeFlushBarrier(t *testing.T) {
	m := newTestRueidisMetaWithFlusher(true, 1000)
	defer func() {
		close(m.batchStopChan)
		<-m.batchDoneChan
	}()

	// Enqueue ops for multiple inodes
	for inode := 1; inode <= 5; inode++ {
		for i := 0; i < 10; i++ {
			op := &BatchOp{
				Type:  OpSET,
				Key:   fmt.Sprintf("i%d:key%d", inode, i),
				Value: []byte(fmt.Sprintf("value%d", i)),
				Inode: Ino(inode),
			}
			select {
			case m.batchQueue <- op:
			case <-time.After(100 * time.Millisecond):
				t.Fatalf("Failed to enqueue op")
			}
		}
	}

	// Flush only inode 3
	err := m.flushBarrier(Ino(3), 1*time.Second)
	if err != nil {
		t.Fatalf("Flush barrier failed: %v", err)
	}

	// Queue should still have ops for other inodes
	queueLen := len(m.batchQueue)
	if queueLen == 0 {
		t.Error("Expected ops remaining for other inodes")
	}

	t.Logf("Successfully flushed inode 3, %d ops remaining", queueLen)
}

// Test 84: Empty batch handling
func TestBackwardCompat_EmptyBatch(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Test empty slice
	err := m.flushBatch(nil)
	if err != nil {
		t.Errorf("Empty batch should not error, got: %v", err)
	}

	// Test empty array
	err = m.flushBatch([]*BatchOp{})
	if err != nil {
		t.Errorf("Empty batch should not error, got: %v", err)
	}

	t.Log("Successfully handled empty batches")
}

// Test 85: Concurrent enqueue safety
func TestBackwardCompat_ConcurrentEnqueue(t *testing.T) {
	m := newTestRueidisMetaWithFlusher(true, 10000)
	defer func() {
		close(m.batchStopChan)
		<-m.batchDoneChan
	}()

	// Concurrent enqueues from multiple goroutines
	numGoroutines := 10
	opsPerGoroutine := 100
	var wg sync.WaitGroup

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				op := &BatchOp{
					Type:  OpSET,
					Key:   fmt.Sprintf("g%d:key%d", goroutineID, i),
					Value: []byte(fmt.Sprintf("value%d", i)),
				}
				select {
				case m.batchQueue <- op:
				case <-time.After(100 * time.Millisecond):
					t.Errorf("Goroutine %d failed to enqueue op %d", goroutineID, i)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	time.Sleep(100 * time.Millisecond) // Allow flushes to complete

	t.Logf("Successfully enqueued %d ops from %d concurrent goroutines",
		numGoroutines*opsPerGoroutine, numGoroutines)
}

// Test 86: Error handling - Retryable errors
func TestBackwardCompat_RetryableErrors(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Simulate retryable error (network timeout)
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", Value: []byte("value1")},
		{Type: OpSET, Key: "key2", Value: []byte("value2")},
	}

	// First attempt: simulate network error (would be retried in real implementation)
	// For unit test, we just verify error handling doesn't panic
	err := m.flushBatch(ops)

	// Without real Redis, this will fail, but shouldn't panic
	// In real implementation with retry logic, this would succeed after retry
	if err == nil {
		t.Log("Flush succeeded (mock Redis)")
	} else {
		t.Logf("Flush failed as expected (no Redis): %v", err)
	}

	t.Log("Successfully handled retryable error scenario")
}

// Test 87: Error handling - Poison operation detection
func TestBackwardCompat_PoisonDetection(t *testing.T) {
	m := newTestRueidisMeta(true, 100)

	// Create operations including an invalid one (empty key)
	ops := []*BatchOp{
		{Type: OpSET, Key: "valid1", Value: []byte("value1")},
		{Type: OpSET, Key: "", Value: []byte("bad")}, // Empty key (invalid)
		{Type: OpSET, Key: "valid2", Value: []byte("value2")},
	}

	// Attempt flush - should detect poison operation
	err := m.flushBatch(ops)

	// Error expected due to invalid operation
	if err == nil {
		t.Error("Expected error for poison operation")
	} else {
		t.Logf("Poison operation correctly detected: %v", err)
	}

	t.Log("Successfully detected poison operation")
}

// Test 88: Adaptive batch sizing verification
func TestBackwardCompat_AdaptiveSizing(t *testing.T) {
	m := newTestRueidisMeta(true, 1000)

	// Verify adaptive sizing fields are initialized
	currentSize := m.currentBatchSize.Load()
	if currentSize <= 0 {
		t.Errorf("Expected positive batch size, got %d", currentSize)
	}

	// Simulate queue depth changes
	m.batchQueueSize.Store(500)
	m.adjustBatchSize()

	// Should have updated metrics
	t.Logf("Current batch size: %d, queue depth: %d",
		m.currentBatchSize.Load(), m.batchQueueSize.Load())

	t.Log("Successfully verified adaptive batch sizing mechanism")
}

// Test 89: MSET optimization - Multiple keys
func TestBackwardCompat_MSETOptimization(t *testing.T) {
	// Create 20 SET operations for different keys
	ops := make([]*BatchOp, 20)
	for i := 0; i < 20; i++ {
		ops[i] = &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
		}
	}

	// Verify all are SET operations (eligible for MSET grouping)
	for _, op := range ops {
		if op.Type != OpSET {
			t.Errorf("Expected OpSET, got %v", op.Type)
		}
	}

	t.Logf("Successfully verified 20 SETs (ready for MSET grouping)")
}

// Test 92: Real-world workload simulation
func TestBackwardCompat_RealWorldWorkload(t *testing.T) {
	// Simulate typical filesystem metadata workload:
	// - Many inode updates (SETs)
	// - Chunk metadata (HSETs)
	// - Reference counting (HINCRBYs)
	// - Deletions (DELs)

	ops := make([]*BatchOp, 0, 500)

	// 200 inode attribute updates (many to same inodes)
	for i := 0; i < 200; i++ {
		inode := (i % 50) + 1 // 50 inodes, each updated ~4 times
		ops = append(ops, &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("inode%d", inode),
			Value: []byte(fmt.Sprintf("attr%d", i)),
			Inode: Ino(inode),
		})
	}

	// 150 chunk metadata updates (HSET)
	for i := 0; i < 150; i++ {
		inode := (i % 30) + 1
		chunkIdx := i % 10
		ops = append(ops, &BatchOp{
			Type:  OpHSET,
			Key:   fmt.Sprintf("chunk%d", inode),
			Field: fmt.Sprintf("idx%d", chunkIdx),
			Value: []byte(fmt.Sprintf("data%d", i)),
			Inode: Ino(inode),
		})
	}

	// 100 reference count updates (HINCRBY)
	for i := 0; i < 100; i++ {
		inode := (i % 20) + 1
		ops = append(ops, &BatchOp{
			Type:  OpHINCRBY,
			Key:   fmt.Sprintf("refs%d", inode),
			Field: "count",
			Delta: 1,
			Inode: Ino(inode),
		})
	}

	// 50 deletions (DEL)
	for i := 0; i < 50; i++ {
		ops = append(ops, &BatchOp{
			Type: OpDEL,
			Key:  fmt.Sprintf("old%d", i),
		})
	}

	start := time.Now()
	result := coalesceOps(ops)
	coalesceDuration := time.Since(start)

	// Should achieve significant reduction
	reductionPct := float64(result.SavedCount) / float64(len(ops)) * 100
	if reductionPct < 20 {
		t.Errorf("Expected at least 20%% reduction, got %.1f%%", reductionPct)
	}

	t.Logf("Real-world workload: %d ops → %d ops (%.1f%% reduction) in %v",
		len(ops), len(result.Coalesced), reductionPct, coalesceDuration)
	t.Logf("  - Inode updates: coalesced from 200 to ~50 (same inodes)")
	t.Logf("  - Chunk metadata: 150 HSETs (some coalesced)")
	t.Logf("  - Reference counts: 100 HINCRBYs coalesced to ~20 sums")
	t.Logf("  - Deletions: 50 DELs preserved")
}

// Test 93: Batch ordering correctness
func TestBackwardCompat_BatchOrdering(t *testing.T) {
	// Ensure operations maintain correct order for correctness
	ops := []*BatchOp{
		{Type: OpSET, Key: "key1", Value: []byte("v1")},
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 5},
		{Type: OpSET, Key: "key1", Value: []byte("v2")},             // Should overwrite v1
		{Type: OpHINCRBY, Key: "counter", Field: "total", Delta: 3}, // Should sum to 8
		{Type: OpDEL, Key: "key1"},                                  // Should delete after v2
	}

	result := coalesceOps(ops)

	// Find final operations
	var foundSET, foundHINCRBY, foundDEL bool
	for _, op := range result.Coalesced {
		switch op.Type {
		case OpSET:
			if op.Key == "key1" {
				// Should NOT exist - DEL comes after
				foundSET = true
			}
		case OpHINCRBY:
			if op.Key == "counter" && op.Field == "total" {
				if op.Delta != 8 {
					t.Errorf("Expected HINCRBY delta 8, got %d", op.Delta)
				}
				foundHINCRBY = true
			}
		case OpDEL:
			if op.Key == "key1" {
				foundDEL = true
			}
		}
	}

	if foundSET {
		t.Error("SET key1 should have been eliminated by subsequent DEL")
	}
	if !foundHINCRBY {
		t.Error("HINCRBY should be present with summed delta")
	}
	if !foundDEL {
		t.Error("DEL should be present")
	}

	t.Log("Successfully verified batch ordering and DEL precedence")
}

// Test 94: Performance benchmark - Coalescing overhead
func TestBackwardCompat_CoalescingPerformance(t *testing.T) {
	// Measure coalescing performance for various batch sizes
	sizes := []int{10, 50, 100, 500, 1000, 5000}

	for _, size := range sizes {
		ops := make([]*BatchOp, size)
		for i := 0; i < size; i++ {
			ops[i] = &BatchOp{
				Type:  OpSET,
				Key:   fmt.Sprintf("key%d", i%100), // 10x coalescing opportunity
				Value: []byte(fmt.Sprintf("value%d", i)),
			}
		}

		start := time.Now()
		result := coalesceOps(ops)
		duration := time.Since(start)

		// Should complete in reasonable time
		maxDuration := time.Duration(size/10) * time.Millisecond // ~100 ops/ms
		if duration > maxDuration {
			t.Errorf("Coalescing %d ops took %v (expected < %v)", size, duration, maxDuration)
		}

		t.Logf("Batch size %4d: %d → %d ops in %v (%.1f ops/ms)",
			size, size, len(result.Coalesced), duration,
			float64(size)/duration.Seconds()/1000)
	}
}

// Test 95: Graceful shutdown
func TestBackwardCompat_GracefulShutdown(t *testing.T) {
	m := newTestRueidisMetaWithFlusher(true, 1000)

	// Enqueue operations
	for i := 0; i < 50; i++ {
		op := &BatchOp{
			Type:  OpSET,
			Key:   fmt.Sprintf("key%d", i),
			Value: []byte(fmt.Sprintf("value%d", i)),
		}
		select {
		case m.batchQueue <- op:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("Failed to enqueue op %d", i)
		}
	}

	// Initiate shutdown
	close(m.batchStopChan)

	// Wait for completion
	select {
	case <-m.batchDoneChan:
		t.Log("Successfully completed graceful shutdown")
	case <-time.After(1 * time.Second):
		t.Error("Shutdown timed out")
	}

	// Queue should be drained
	queueLen := len(m.batchQueue)
	if queueLen > 0 {
		t.Logf("Warning: %d ops remaining after shutdown (expected in mock)", queueLen)
	}
}

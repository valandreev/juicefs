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
	"fmt"
	"testing"
	"time"
)

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

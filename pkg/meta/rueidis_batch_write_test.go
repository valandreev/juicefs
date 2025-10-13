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

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
	"time"
)

// OpType represents the type of Redis operation for batching
type OpType uint8

const (
	OpSET     OpType = iota // SET key value
	OpHSET                  // HSET hash field value
	OpHDEL                  // HDEL hash field
	OpHINCRBY               // HINCRBY hash field delta
	OpDEL                   // DEL key
	OpZADD                  // ZADD key score member
	OpSADD                  // SADD key member
	OpRPUSH                 // RPUSH key value
	OpINCRBY                // INCRBY key delta
)

// String returns the string representation of OpType
func (t OpType) String() string {
	switch t {
	case OpSET:
		return "SET"
	case OpHSET:
		return "HSET"
	case OpHDEL:
		return "HDEL"
	case OpHINCRBY:
		return "HINCRBY"
	case OpDEL:
		return "DEL"
	case OpZADD:
		return "ZADD"
	case OpSADD:
		return "SADD"
	case OpRPUSH:
		return "RPUSH"
	case OpINCRBY:
		return "INCRBY"
	default:
		return "UNKNOWN"
	}
}

// BatchOp represents a single batched Redis operation
type BatchOp struct {
	// Core operation fields
	Type  OpType  // Operation type
	Key   string  // Redis key
	Field string  // For Hash operations (HSET, HDEL, HINCRBY)
	Value []byte  // Value to set/add
	Delta int64   // For INCRBY/HINCRBY operations
	Score float64 // For ZADD operations

	// Metadata for tracking and ordering
	Inode       Ino       // Associated inode (for ordering constraints)
	Priority    int       // Higher = flush sooner (default: 0)
	RetryCount  int       // Number of retry attempts
	EnqueueTime time.Time // When op was queued (for metrics)

	// For result tracking (optional)
	ResultChan chan error // For synchronous flush barriers
}

// OpMeta provides additional context for debugging
type OpMeta struct {
	Operation string // e.g., "doWrite", "dirStats"
	File      string // Source file that created this op
	Line      int    // Source line number
}

// CoalesceResult holds the result of coalescing operations
type CoalesceResult struct {
	Coalesced     []*BatchOp // Final coalesced operations
	OriginalCount int        // Original operation count
	SavedCount    int        // Number of operations saved by coalescing
}

// batchAccumulator collects and coalesces operations during flush
type batchAccumulator struct {
	// Raw operations in order
	ops []*BatchOp

	// Coalescing maps
	sets     map[string]*BatchOp                   // key → last SET
	hsets    map[string]map[string]*BatchOp        // hash → field → last HSET
	hincrbys map[string]map[string]*batchHIncrByOp // hash → field → accumulated delta
	incrbys  map[string]*batchIncrByOp             // key → accumulated delta

	// For ordering constraints
	inodeOps map[Ino][]*BatchOp // ops grouped by inode
}

// batchHIncrByOp accumulates HINCRBY operations
type batchHIncrByOp struct {
	hash  string
	field string
	delta int64
	inode Ino
}

// batchIncrByOp accumulates INCRBY operations
type batchIncrByOp struct {
	key   string
	delta int64
	inode Ino
}

// newBatchAccumulator creates a new batch accumulator
func newBatchAccumulator() *batchAccumulator {
	return &batchAccumulator{
		ops:      make([]*BatchOp, 0, 512),
		sets:     make(map[string]*BatchOp),
		hsets:    make(map[string]map[string]*BatchOp),
		hincrbys: make(map[string]map[string]*batchHIncrByOp),
		incrbys:  make(map[string]*batchIncrByOp),
		inodeOps: make(map[Ino][]*BatchOp),
	}
}

// add adds an operation to the accumulator
func (acc *batchAccumulator) add(op *BatchOp) {
	acc.ops = append(acc.ops, op)

	// Track by inode for ordering
	if op.Inode > 0 {
		acc.inodeOps[op.Inode] = append(acc.inodeOps[op.Inode], op)
	}
}

// coalesceSET coalesces SET operations (same key → keep last)
func (acc *batchAccumulator) coalesceSET(op *BatchOp) {
	if prev, exists := acc.sets[op.Key]; exists {
		// Replace previous SET with current one
		prev.Value = op.Value
		prev.EnqueueTime = op.EnqueueTime
		prev.Inode = op.Inode
		// Don't add new op to acc.ops, just update existing
	} else {
		acc.sets[op.Key] = op
		acc.add(op)
	}
}

// coalesceHSET coalesces HSET operations (same hash+field → keep last)
func (acc *batchAccumulator) coalesceHSET(op *BatchOp) {
	if _, exists := acc.hsets[op.Key]; !exists {
		acc.hsets[op.Key] = make(map[string]*BatchOp)
	}

	if prev, exists := acc.hsets[op.Key][op.Field]; exists {
		// Replace previous HSET with current one
		prev.Value = op.Value
		prev.EnqueueTime = op.EnqueueTime
		prev.Inode = op.Inode
	} else {
		acc.hsets[op.Key][op.Field] = op
		acc.add(op)
	}
}

// coalesceHINCRBY coalesces HINCRBY operations (same hash+field → sum deltas)
func (acc *batchAccumulator) coalesceHINCRBY(op *BatchOp) {
	if _, exists := acc.hincrbys[op.Key]; !exists {
		acc.hincrbys[op.Key] = make(map[string]*batchHIncrByOp)
	}

	if prev, exists := acc.hincrbys[op.Key][op.Field]; exists {
		// Sum the deltas
		prev.delta += op.Delta
	} else {
		acc.hincrbys[op.Key][op.Field] = &batchHIncrByOp{
			hash:  op.Key,
			field: op.Field,
			delta: op.Delta,
			inode: op.Inode,
		}
		// Add placeholder to maintain ordering
		acc.add(op)
	}
}

// coalesceINCRBY coalesces INCRBY operations (same key → sum deltas)
func (acc *batchAccumulator) coalesceINCRBY(op *BatchOp) {
	if prev, exists := acc.incrbys[op.Key]; exists {
		// Sum the deltas
		prev.delta += op.Delta
	} else {
		acc.incrbys[op.Key] = &batchIncrByOp{
			key:   op.Key,
			delta: op.Delta,
			inode: op.Inode,
		}
		acc.add(op)
	}
}

// coalesceOps processes all operations and applies coalescing rules
func coalesceOps(ops []*BatchOp) *CoalesceResult {
	acc := newBatchAccumulator()
	originalCount := len(ops)

	// Step 1: Accumulate ops into coalescing maps
	for _, op := range ops {
		switch op.Type {
		case OpSET:
			acc.coalesceSET(op)
		case OpHSET:
			acc.coalesceHSET(op)
		case OpHINCRBY:
			acc.coalesceHINCRBY(op)
		case OpINCRBY:
			acc.coalesceINCRBY(op)
		default:
			// No coalescing for other types, add directly
			acc.add(op)
		}
	}

	// Step 2: Build final coalesced operation list
	// Note: We keep the original order from acc.ops, but update values
	// from coalescing maps (already done in coalesce* methods above)

	// Update HINCRBY ops with summed deltas
	for i, op := range acc.ops {
		if op.Type == OpHINCRBY {
			if summed, exists := acc.hincrbys[op.Key][op.Field]; exists {
				acc.ops[i].Delta = summed.delta
			}
		} else if op.Type == OpINCRBY {
			if summed, exists := acc.incrbys[op.Key]; exists {
				acc.ops[i].Delta = summed.delta
			}
		}
	}

	return &CoalesceResult{
		Coalesced:     acc.ops,
		OriginalCount: originalCount,
		SavedCount:    originalCount - len(acc.ops),
	}
}

// canCoalesce returns true if two operations can be coalesced
func canCoalesce(op1, op2 *BatchOp) bool {
	if op1.Type != op2.Type {
		return false
	}

	switch op1.Type {
	case OpSET:
		return op1.Key == op2.Key
	case OpHSET, OpHINCRBY, OpHDEL:
		return op1.Key == op2.Key && op1.Field == op2.Field
	case OpINCRBY:
		return op1.Key == op2.Key
	default:
		return false
	}
}

// shouldPreserveOrder returns true if operations must preserve order
// (e.g., operations on the same inode)
func shouldPreserveOrder(op1, op2 *BatchOp) bool {
	// Operations on the same inode must preserve FIFO order
	if op1.Inode > 0 && op1.Inode == op2.Inode {
		return true
	}
	return false
}

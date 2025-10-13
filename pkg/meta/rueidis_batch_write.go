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
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/redis/rueidis"
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

// enqueueBatchOp enqueues a batch operation with back-pressure
func (m *rueidisMeta) enqueueBatchOp(op *BatchOp) error {
	if !m.batchEnabled {
		return syscall.ENOSYS // Not implemented when batching disabled
	}

	// Check queue capacity for back-pressure
	currentSize := m.batchQueueSize.Load()
	if currentSize >= int64(m.maxQueueSize) {
		// Queue is full, apply back-pressure
		m.batchErrors.WithLabelValues("queue_full").Inc()
		return syscall.EAGAIN
	}

	// Non-blocking send with timeout
	select {
	case m.batchQueue <- op:
		// Update metrics
		m.batchQueueSize.Add(1)
		m.batchQueueBytes.Add(int64(len(op.Value)))
		m.batchOpsQueued.WithLabelValues(op.Type.String()).Inc()
		return nil
	case <-time.After(100 * time.Millisecond):
		// Timeout - queue might be congested
		m.batchErrors.WithLabelValues("enqueue_timeout").Inc()
		return syscall.ETIMEDOUT
	}
}

// batchFlusher is the background goroutine that periodically flushes batched operations
func (m *rueidisMeta) batchFlusher() {
	defer close(m.batchDoneChan)

	pendingOps := make([]*BatchOp, 0, m.batchSize)
	adaptiveSampleCounter := 0 // Count flushes to trigger adaptive sizing check

	for {
		select {
		case <-m.batchStopChan:
			// Shutdown requested - flush any remaining ops
			if len(pendingOps) > 0 {
				_ = m.flushBatch(pendingOps)
			}
			return

		case <-m.batchFlushTicker.C:
			// Timer fired - flush if we have pending ops
			if len(pendingOps) > 0 {
				_ = m.flushBatch(pendingOps)
				pendingOps = pendingOps[:0] // Reset slice
			}
			// Check adaptive sizing periodically (every timer tick)
			m.adjustBatchSize()

		case op := <-m.batchQueue:
			// New operation arrived
			pendingOps = append(pendingOps, op)
			m.batchQueueSize.Add(-1)
			m.batchQueueBytes.Add(-int64(len(op.Value)))

			// Check if this is a barrier operation (has ResultChan)
			isBarrier := op.ResultChan != nil

			// Check flush triggers
			shouldFlush := false

			// Policy 0: Barrier operation - flush immediately for this inode
			if isBarrier {
				shouldFlush = true
			}

			// Policy 1: Size-based flush (use current adaptive size)
			currentSize := int(m.currentBatchSize.Load())
			if len(pendingOps) >= currentSize {
				shouldFlush = true
			}

			// Policy 2: Byte-based flush
			if !shouldFlush {
				totalBytes := 0
				for _, o := range pendingOps {
					totalBytes += len(o.Value)
				}
				if totalBytes >= m.batchBytes {
					shouldFlush = true
				}
			}

			// Drain more ops if available (batch opportunistically)
			if !shouldFlush {
			drainLoop:
				for len(pendingOps) < currentSize {
					select {
					case nextOp := <-m.batchQueue:
						pendingOps = append(pendingOps, nextOp)
						m.batchQueueSize.Add(-1)
						m.batchQueueBytes.Add(-int64(len(nextOp.Value)))
						// Stop draining if we hit another barrier
						if nextOp.ResultChan != nil {
							shouldFlush = true
							break drainLoop
						}
					default:
						// No more ops available
						goto checkFlush
					}
				}
			}

		checkFlush:
			if shouldFlush {
				// If this is a barrier flush, separate ops by inode
				if isBarrier {
					m.flushWithBarrier(pendingOps)
				} else {
					_ = m.flushBatch(pendingOps)
				}
				pendingOps = pendingOps[:0] // Reset slice

				// Periodically check adaptive sizing (every 10 flushes)
				adaptiveSampleCounter++
				if adaptiveSampleCounter >= 10 {
					adaptiveSampleCounter = 0
					m.adjustBatchSize()
				}
			}
		}

		// Update queue depth gauge
		m.batchQueueDepthGauge.Set(float64(m.batchQueueSize.Load()))
	}
}

// flushWithBarrier handles barrier operations by:
// 1. Finding all barrier ops in the batch
// 2. For each barrier, flush all ops for that inode up to the barrier
// 3. Signal the barrier's ResultChan when done
func (m *rueidisMeta) flushWithBarrier(ops []*BatchOp) {
	if len(ops) == 0 {
		return
	}

	// Find all barriers and their positions
	barriers := make([]*BatchOp, 0)
	for _, op := range ops {
		if op.ResultChan != nil {
			barriers = append(barriers, op)
		}
	}

	if len(barriers) == 0 {
		// No barriers - normal flush
		_ = m.flushBatch(ops)
		return
	}

	// Process each barrier
	for _, barrier := range barriers {
		// Filter ops for this barrier's inode (up to and including the barrier)
		opsToFlush := make([]*BatchOp, 0)
		for i, op := range ops {
			// If barrier inode is 0, flush all ops up to barrier
			// Otherwise, only flush ops for matching inode
			if barrier.Inode == 0 || op.Inode == barrier.Inode {
				// Skip the barrier itself (it's just a marker)
				if op == barrier {
					// Found the barrier - flush everything before it
					break
				}
				opsToFlush = append(opsToFlush, op)
			}
			// Stop when we reach the barrier
			if i == len(ops)-1 || ops[i+1] == barrier {
				break
			}
		}

		// Flush ops for this inode
		err := m.flushBatch(opsToFlush)

		// Signal barrier completion
		if barrier.ResultChan != nil {
			barrier.ResultChan <- err
			close(barrier.ResultChan)
		}
	}

	// Flush any remaining non-barrier ops
	remainingOps := make([]*BatchOp, 0)
	barrierSet := make(map[*BatchOp]bool)
	for _, b := range barriers {
		barrierSet[b] = true
	}
	for _, op := range ops {
		if !barrierSet[op] {
			// Check if this op was already flushed for a barrier
			alreadyFlushed := false
			for _, barrier := range barriers {
				if barrier.Inode == 0 || op.Inode == barrier.Inode {
					alreadyFlushed = true
					break
				}
			}
			if !alreadyFlushed {
				remainingOps = append(remainingOps, op)
			}
		}
	}
	if len(remainingOps) > 0 {
		_ = m.flushBatch(remainingOps)
	}
}

// flushBatch flushes accumulated operations to Redis using DoMulti (pipeline)
func (m *rueidisMeta) flushBatch(ops []*BatchOp) error {
	if len(ops) == 0 {
		return nil
	}

	start := time.Now()
	defer func() {
		duration := time.Since(start).Seconds()
		m.batchFlushDuration.Observe(duration)
		m.batchSizeHistogram.Observe(float64(len(ops)))
	}()

	// Step 1: Coalesce operations
	result := coalesceOps(ops)
	if result.SavedCount > 0 {
		// Track coalescing savings by type
		savedByType := make(map[OpType]int)
		for _, op := range ops {
			if !containsOp(result.Coalesced, op) {
				savedByType[op.Type]++
			}
		}
		for opType, count := range savedByType {
			m.batchCoalesceSaved.WithLabelValues(opType.String()).Add(float64(count))
		}
	}

	// Step 1.5: Optimize multiple SETs into MSET (Step 6)
	msetCmds, nonSetOps := m.buildMSET(result.Coalesced)

	// Step 1.6: Optimize multiple HSETs into HMSET (Step 7)
	hmsetCmdsBySlot, nonHsetOps := m.buildHMSET(nonSetOps)

	// Step 2: Build rueidis commands for remaining operations
	ctx := context.Background()

	// Calculate total command count
	totalHmsetCmds := 0
	for _, cmds := range hmsetCmdsBySlot {
		totalHmsetCmds += len(cmds)
	}
	cmds := make([]rueidis.Completed, 0, len(nonHsetOps)+len(msetCmds)+totalHmsetCmds)

	opMap := make(map[int]*BatchOp)         // index → original op for error tracking
	msetMap := make(map[int]int)            // index → slot for MSET commands
	hmsetMap := make(map[int]int)           // index → slot for HMSET commands
	msetOpsMap := make(map[int][]*BatchOp)  // index → original SET ops for MSET fallback
	hmsetOpsMap := make(map[int][]*BatchOp) // index → original HSET ops for HMSET fallback

	// Track original SET operations for MSET fallback
	setsBySlot := make(map[int][]*BatchOp)
	for _, op := range ops {
		if op.Type == OpSET {
			slot := getHashSlot(op.Key)
			setsBySlot[slot] = append(setsBySlot[slot], op)
		}
	}

	// Track original HSET operations for HMSET fallback
	hsetsByKey := make(map[string][]*BatchOp)
	for _, op := range ops {
		if op.Type == OpHSET {
			hsetsByKey[op.Key] = append(hsetsByKey[op.Key], op)
		}
	}

	// Add MSET commands
	for slot, cmd := range msetCmds {
		cmds = append(cmds, cmd)
		idx := len(cmds) - 1
		msetMap[idx] = slot                // Track which command is an MSET
		msetOpsMap[idx] = setsBySlot[slot] // Store original ops for fallback
	}

	// Add HMSET commands
	for slot, hmsetCmds := range hmsetCmdsBySlot {
		for _, cmd := range hmsetCmds {
			cmds = append(cmds, cmd)
			idx := len(cmds) - 1
			hmsetMap[idx] = slot // Track which command is an HMSET

			// Find original HSET ops for this HMSET command
			// Extract key from command (we need to track this better)
			// For now, store reference by slot
			for key, hsets := range hsetsByKey {
				if getHashSlot(key) == slot && len(hsets) > 1 {
					hmsetOpsMap[idx] = hsets
					break
				}
			}
		}
	}

	// Add individual commands
	for _, op := range nonHsetOps {
		cmd, err := m.buildCommand(op)
		if err != nil {
			m.batchErrors.WithLabelValues("build_command").Inc()
			continue
		}
		cmds = append(cmds, cmd)
		opMap[len(cmds)-1] = op // Map command index to op
	}

	if len(cmds) == 0 {
		return nil
	}

	// Step 3: Execute batch with DoMulti
	results := m.client.DoMulti(ctx, cmds...)

	// Step 4: Process results and handle errors
	failedOps := make([]*BatchOp, 0)
	for i, result := range results {
		if err := result.Error(); err != nil {
			// Check if this is an MSET command that failed
			if originalOps, isMSET := msetOpsMap[i]; isMSET {
				// Check if it's a CROSSSLOT error
				if m.isCrossSlotError(err) {
					// CROSSSLOT error - fall back to individual SET operations
					m.batchErrors.WithLabelValues("mset_crossslot").Inc()

					// Re-enqueue original SET operations individually
					// This allows them to be executed separately
					for _, op := range originalOps {
						failedOps = append(failedOps, op)
					}
				} else if m.isRetryableError(err) {
					// Other retryable error - retry entire MSET
					m.batchErrors.WithLabelValues("mset_retryable").Inc()
					for _, op := range originalOps {
						op.RetryCount++
						if op.RetryCount < 3 {
							failedOps = append(failedOps, op)
						} else {
							m.handlePoisonOp(op, err)
						}
					}
				} else {
					// Permanent error - log and drop
					m.batchErrors.WithLabelValues("mset_permanent").Inc()
					for _, op := range originalOps {
						m.handlePoisonOp(op, err)
					}
				}
				continue
			}

			// Check if this is an HMSET command that failed
			if originalOps, isHMSET := hmsetOpsMap[i]; isHMSET {
				// Check if it's a CROSSSLOT error
				if m.isCrossSlotError(err) {
					// CROSSSLOT error - fall back to individual HSET operations
					m.batchErrors.WithLabelValues("hmset_crossslot").Inc()

					// Re-enqueue original HSET operations individually
					for _, op := range originalOps {
						failedOps = append(failedOps, op)
					}
				} else if m.isRetryableError(err) {
					// Other retryable error - retry entire HMSET
					m.batchErrors.WithLabelValues("hmset_retryable").Inc()
					for _, op := range originalOps {
						op.RetryCount++
						if op.RetryCount < 3 {
							failedOps = append(failedOps, op)
						} else {
							m.handlePoisonOp(op, err)
						}
					}
				} else {
					// Permanent error - log and drop
					m.batchErrors.WithLabelValues("hmset_permanent").Inc()
					for _, op := range originalOps {
						m.handlePoisonOp(op, err)
					}
				}
				continue
			}

			// Regular operation error handling
			op := opMap[i]
			if m.isRetryableError(err) {
				op.RetryCount++
				if op.RetryCount < 3 { // Max 3 retries
					failedOps = append(failedOps, op)
					m.batchErrors.WithLabelValues("retryable").Inc()
				} else {
					// Poison operation - log and drop
					m.handlePoisonOp(op, err)
				}
			} else {
				// Permanent error - log and drop
				m.batchErrors.WithLabelValues("permanent").Inc()
			}
		} else {
			// Success
			if _, isMSET := msetMap[i]; !isMSET {
				if _, isHMSET := hmsetMap[i]; !isHMSET {
					// Regular operation (not MSET or HMSET)
					op := opMap[i]
					m.batchOpsFlushed.WithLabelValues(op.Type.String()).Inc()
				}
			}
			// MSET and HMSET success is already tracked via their respective metrics
		}
	}

	// Step 5: Re-enqueue failed operations for retry
	if len(failedOps) > 0 {
		go m.retryFailedOps(failedOps)
	}

	return nil
}

// buildCommand converts a BatchOp to a rueidis.Completed command
func (m *rueidisMeta) buildCommand(op *BatchOp) (rueidis.Completed, error) {
	switch op.Type {
	case OpSET:
		return m.client.B().Set().Key(op.Key).Value(rueidis.BinaryString(op.Value)).Build(), nil

	case OpHSET:
		return m.client.B().Hset().Key(op.Key).FieldValue().FieldValue(op.Field, rueidis.BinaryString(op.Value)).Build(), nil

	case OpHDEL:
		return m.client.B().Hdel().Key(op.Key).Field(op.Field).Build(), nil

	case OpHINCRBY:
		return m.client.B().Hincrby().Key(op.Key).Field(op.Field).Increment(op.Delta).Build(), nil

	case OpDEL:
		return m.client.B().Del().Key(op.Key).Build(), nil

	case OpINCRBY:
		return m.client.B().Incrby().Key(op.Key).Increment(op.Delta).Build(), nil

	case OpRPUSH:
		return m.client.B().Rpush().Key(op.Key).Element(rueidis.BinaryString(op.Value)).Build(), nil

	case OpZADD:
		return m.client.B().Zadd().Key(op.Key).ScoreMember().ScoreMember(op.Score, string(op.Value)).Build(), nil

	case OpSADD:
		return m.client.B().Sadd().Key(op.Key).Member(string(op.Value)).Build(), nil

	default:
		return rueidis.Completed{}, fmt.Errorf("unsupported operation type: %v", op.Type)
	}
}

// isRetryableError determines if an error should trigger a retry
func (m *rueidisMeta) isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Network errors
	if err == syscall.ETIMEDOUT ||
		err == syscall.ECONNRESET ||
		err == syscall.ECONNREFUSED {
		return true
	}

	// Redis temporary errors
	if containsAny(errStr, []string{
		"BUSY",
		"LOADING",
		"TIMEOUT",
		"connection",
		"broken pipe",
	}) {
		return true
	}

	return false
}

// isCrossSlotError checks if an error is a CROSSSLOT error from Redis Cluster
func (m *rueidisMeta) isCrossSlotError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	// Redis Cluster returns CROSSSLOT error when keys in a multi-key command
	// are not all in the same hash slot
	return containsAny(errStr, []string{"CROSSSLOT", "crossslot"})
}

// handlePoisonOp logs and tracks poison operations (failed after max retries)
func (m *rueidisMeta) handlePoisonOp(op *BatchOp, err error) {
	m.batchPoisonOps.Inc()

	// Log detailed error for debugging
	logger := utils.GetLogger("juicefs")
	logger.Errorf("Poison operation after %d retries: type=%v key=%s error=%v",
		op.RetryCount, op.Type, op.Key, err)

	// Notify via result channel if someone is waiting
	if op.ResultChan != nil {
		select {
		case op.ResultChan <- err:
		default:
			// Channel full or closed, ignore
		}
	}
}

// retryFailedOps re-enqueues failed operations for retry
func (m *rueidisMeta) retryFailedOps(ops []*BatchOp) {
	for _, op := range ops {
		// Try to re-enqueue with a small delay to avoid tight retry loop
		time.Sleep(10 * time.Millisecond)

		if err := m.enqueueBatchOp(op); err != nil {
			// If re-enqueue fails, treat as poison
			m.handlePoisonOp(op, fmt.Errorf("retry enqueue failed: %w", err))
		}
	}
}

// getHashSlot calculates the Redis Cluster hash slot for a key
// Uses CRC16 algorithm as per Redis Cluster specification
func getHashSlot(key string) int {
	// Extract hash tag if present (text between first { and first })
	start := indexOf(key, "{")
	if start >= 0 {
		end := indexOf(key[start+1:], "}")
		if end > 0 {
			// Use hash tag content for slot calculation
			key = key[start+1 : start+1+end]
		}
	}

	// CRC16 lookup table (XMODEM variant used by Redis)
	crc16tab := [256]uint16{
		0x0000, 0x1021, 0x2042, 0x3063, 0x4084, 0x50a5, 0x60c6, 0x70e7,
		0x8108, 0x9129, 0xa14a, 0xb16b, 0xc18c, 0xd1ad, 0xe1ce, 0xf1ef,
		0x1231, 0x0210, 0x3273, 0x2252, 0x52b5, 0x4294, 0x72f7, 0x62d6,
		0x9339, 0x8318, 0xb37b, 0xa35a, 0xd3bd, 0xc39c, 0xf3ff, 0xe3de,
		0x2462, 0x3443, 0x0420, 0x1401, 0x64e6, 0x74c7, 0x44a4, 0x5485,
		0xa56a, 0xb54b, 0x8528, 0x9509, 0xe5ee, 0xf5cf, 0xc5ac, 0xd58d,
		0x3653, 0x2672, 0x1611, 0x0630, 0x76d7, 0x66f6, 0x5695, 0x46b4,
		0xb75b, 0xa77a, 0x9719, 0x8738, 0xf7df, 0xe7fe, 0xd79d, 0xc7bc,
		0x48c4, 0x58e5, 0x6886, 0x78a7, 0x0840, 0x1861, 0x2802, 0x3823,
		0xc9cc, 0xd9ed, 0xe98e, 0xf9af, 0x8948, 0x9969, 0xa90a, 0xb92b,
		0x5af5, 0x4ad4, 0x7ab7, 0x6a96, 0x1a71, 0x0a50, 0x3a33, 0x2a12,
		0xdbfd, 0xcbdc, 0xfbbf, 0xeb9e, 0x9b79, 0x8b58, 0xbb3b, 0xab1a,
		0x6ca6, 0x7c87, 0x4ce4, 0x5cc5, 0x2c22, 0x3c03, 0x0c60, 0x1c41,
		0xedae, 0xfd8f, 0xcdec, 0xddcd, 0xad2a, 0xbd0b, 0x8d68, 0x9d49,
		0x7e97, 0x6eb6, 0x5ed5, 0x4ef4, 0x3e13, 0x2e32, 0x1e51, 0x0e70,
		0xff9f, 0xefbe, 0xdfdd, 0xcffc, 0xbf1b, 0xaf3a, 0x9f59, 0x8f78,
		0x9188, 0x81a9, 0xb1ca, 0xa1eb, 0xd10c, 0xc12d, 0xf14e, 0xe16f,
		0x1080, 0x00a1, 0x30c2, 0x20e3, 0x5004, 0x4025, 0x7046, 0x6067,
		0x83b9, 0x9398, 0xa3fb, 0xb3da, 0xc33d, 0xd31c, 0xe37f, 0xf35e,
		0x02b1, 0x1290, 0x22f3, 0x32d2, 0x4235, 0x5214, 0x6277, 0x7256,
		0xb5ea, 0xa5cb, 0x95a8, 0x8589, 0xf56e, 0xe54f, 0xd52c, 0xc50d,
		0x34e2, 0x24c3, 0x14a0, 0x0481, 0x7466, 0x6447, 0x5424, 0x4405,
		0xa7db, 0xb7fa, 0x8799, 0x97b8, 0xe75f, 0xf77e, 0xc71d, 0xd73c,
		0x26d3, 0x36f2, 0x0691, 0x16b0, 0x6657, 0x7676, 0x4615, 0x5634,
		0xd94c, 0xc96d, 0xf90e, 0xe92f, 0x99c8, 0x89e9, 0xb98a, 0xa9ab,
		0x5844, 0x4865, 0x7806, 0x6827, 0x18c0, 0x08e1, 0x3882, 0x28a3,
		0xcb7d, 0xdb5c, 0xeb3f, 0xfb1e, 0x8bf9, 0x9bd8, 0xabbb, 0xbb9a,
		0x4a75, 0x5a54, 0x6a37, 0x7a16, 0x0af1, 0x1ad0, 0x2ab3, 0x3a92,
		0xfd2e, 0xed0f, 0xdd6c, 0xcd4d, 0xbdaa, 0xad8b, 0x9de8, 0x8dc9,
		0x7c26, 0x6c07, 0x5c64, 0x4c45, 0x3ca2, 0x2c83, 0x1ce0, 0x0cc1,
		0xef1f, 0xff3e, 0xcf5d, 0xdf7c, 0xaf9b, 0xbfba, 0x8fd9, 0x9ff8,
		0x6e17, 0x7e36, 0x4e55, 0x5e74, 0x2e93, 0x3eb2, 0x0ed1, 0x1ef0,
	}

	var crc uint16 = 0
	for i := 0; i < len(key); i++ {
		crc = (crc << 8) ^ crc16tab[(byte(crc>>8)^key[i])]
	}

	return int(crc & 0x3FFF) // Modulo 16384
}

// buildMSET detects multiple SET operations and builds MSET commands
// Returns: map of slot → MSET command, and remaining non-SET ops
func (m *rueidisMeta) buildMSET(ops []*BatchOp) (map[int]rueidis.Completed, []*BatchOp) {
	// Group SET operations by hash slot
	setsBySlot := make(map[int][]*BatchOp)
	nonSetOps := make([]*BatchOp, 0)

	for _, op := range ops {
		if op.Type == OpSET {
			slot := getHashSlot(op.Key)
			setsBySlot[slot] = append(setsBySlot[slot], op)
		} else {
			nonSetOps = append(nonSetOps, op)
		}
	}

	// Build MSET commands for each slot (only if > 1 SET in that slot)
	msetCmds := make(map[int]rueidis.Completed)
	for slot, sets := range setsBySlot {
		if len(sets) > 1 {
			// Build MSET command: MSET key1 val1 key2 val2 ...
			// Flatten into alternating key-value sequence
			args := make([]string, 0, len(sets)*2)
			for _, op := range sets {
				args = append(args, op.Key, string(op.Value))
			}

			// Build command using the args
			cmd := m.client.B().Arbitrary("MSET").Keys(args[0]).Args(args[1:]...).Build()
			msetCmds[slot] = cmd

			// Track metrics
			m.batchMsetConversions.Inc()
			m.batchMsetOpsSaved.Add(float64(len(sets) - 1)) // N SETs → 1 MSET saves N-1 ops
		} else if len(sets) == 1 {
			// Only one SET in this slot, keep as individual op
			nonSetOps = append(nonSetOps, sets[0])
		}
	}

	return msetCmds, nonSetOps
}

// buildHMSET groups multiple HSET operations to the same hash key into HMSET commands
// Returns: map of slot → []HMSET commands, and remaining non-HSET ops
// Note: HINCRBY operations are excluded and kept separate (different semantics)
func (m *rueidisMeta) buildHMSET(ops []*BatchOp) (map[int][]rueidis.Completed, []*BatchOp) {
	// Group HSET operations by hash key
	hsetsByKey := make(map[string][]*BatchOp)
	nonHsetOps := make([]*BatchOp, 0)

	for _, op := range ops {
		if op.Type == OpHSET {
			// Group by key (all HSETs to same hash)
			hsetsByKey[op.Key] = append(hsetsByKey[op.Key], op)
		} else {
			// Keep non-HSET operations (including HINCRBY, HDEL, etc.)
			nonHsetOps = append(nonHsetOps, op)
		}
	}

	// Build HMSET commands grouped by slot
	hmsetCmdsBySlot := make(map[int][]rueidis.Completed)

	for key, hsets := range hsetsByKey {
		if len(hsets) > 1 {
			// Multiple HSETs to same hash → build HMSET
			// HMSET key field1 value1 field2 value2 ...
			args := make([]string, 0, len(hsets)*2+1)
			args = append(args, key) // First arg is the hash key

			for _, op := range hsets {
				args = append(args, op.Field, string(op.Value))
			}

			// Build HMSET command
			cmd := m.client.B().Arbitrary("HMSET").Keys(args[0]).Args(args[1:]...).Build()

			// Group by slot for Redis Cluster compatibility
			slot := getHashSlot(key)
			hmsetCmdsBySlot[slot] = append(hmsetCmdsBySlot[slot], cmd)

			// Track metrics
			m.batchHmsetConversions.Inc()
			m.batchHsetCoalesced.Add(float64(len(hsets))) // N HSETs → 1 HMSET
		} else if len(hsets) == 1 {
			// Only one HSET to this hash, keep as individual op
			nonHsetOps = append(nonHsetOps, hsets[0])
		}
	}

	return hmsetCmdsBySlot, nonHsetOps
}

// containsAny checks if string contains any of the substrings
func containsAny(s string, substrs []string) bool {
	for _, substr := range substrs {
		if containsSubstring(s, substr) {
			return true
		}
	}
	return false
}

// containsSubstring checks if string contains substring (case-insensitive)
func containsSubstring(s, substr string) bool {
	s = toLower(s)
	substr = toLower(substr)
	return indexOf(s, substr) >= 0
}

// toLower converts string to lowercase
func toLower(s string) string {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		result[i] = c
	}
	return string(result)
}

// indexOf returns the index of substr in s, or -1 if not found
func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// executeSingleOp is deprecated - replaced by DoMulti batch execution
// Kept for backward compatibility only
func (m *rueidisMeta) executeSingleOp(ctx context.Context, op *BatchOp) error {
	switch op.Type {
	case OpSET:
		return m.compat.Set(ctx, op.Key, op.Value, 0).Err()
	case OpHSET:
		return m.compat.HSet(ctx, op.Key, op.Field, op.Value).Err()
	case OpHDEL:
		return m.compat.HDel(ctx, op.Key, op.Field).Err()
	case OpHINCRBY:
		return m.compat.HIncrBy(ctx, op.Key, op.Field, op.Delta).Err()
	case OpDEL:
		return m.compat.Del(ctx, op.Key).Err()
	case OpINCRBY:
		return m.compat.IncrBy(ctx, op.Key, op.Delta).Err()
	case OpRPUSH:
		return m.compat.RPush(ctx, op.Key, op.Value).Err()
	default:
		return fmt.Errorf("unsupported operation type: %v", op.Type)
	}
}

// containsOp checks if an operation is in the slice
func containsOp(ops []*BatchOp, target *BatchOp) bool {
	for _, op := range ops {
		if op == target {
			return true
		}
	}
	return false
}

// adjustBatchSize dynamically adjusts the batch size based on queue depth.
//
// Algorithm:
// 1. Sample current queue depth
// 2. Store in circular buffer (last 10 samples)
// 3. Calculate average queue depth
// 4. If average > highWaterMark (1000): increase to maxBatchSize (2048)
// 5. If average < lowWaterMark (100): decrease to baseBatchSize (512)
//
// This prevents oscillation by using a windowed average and hysteresis
// (high/low watermarks with gap to avoid thrashing).
func (m *rueidisMeta) adjustBatchSize() {
	m.adaptiveLock.Lock()
	defer m.adaptiveLock.Unlock()

	// Sample current queue depth
	currentDepth := m.batchQueueSize.Load()

	// Store sample in circular buffer
	m.lastQueueSamples[m.sampleIndex] = currentDepth
	m.sampleIndex = (m.sampleIndex + 1) % len(m.lastQueueSamples)

	// Calculate average queue depth over last N samples
	var sum int64
	var count int
	for i := 0; i < len(m.lastQueueSamples); i++ {
		if m.lastQueueSamples[i] > 0 || i < m.sampleIndex {
			sum += m.lastQueueSamples[i]
			count++
		}
	}

	if count == 0 {
		return // No samples yet
	}

	avgDepth := sum / int64(count)
	currentSize := m.currentBatchSize.Load()

	// Decide whether to adjust batch size
	var newSize int32

	if avgDepth > int64(m.highWaterMark) && currentSize < int32(m.maxBatchSize) {
		// Queue is consistently high - increase batch size
		newSize = int32(m.maxBatchSize)
	} else if avgDepth < int64(m.lowWaterMark) && currentSize > int32(m.baseBatchSize) {
		// Queue is consistently low - decrease batch size
		newSize = int32(m.baseBatchSize)
	} else {
		// No change needed
		return
	}

	// Update batch size
	m.currentBatchSize.Store(newSize)
	m.batchSizeCurrent.Set(float64(newSize))
}

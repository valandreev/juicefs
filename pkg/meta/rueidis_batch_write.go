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

		case op := <-m.batchQueue:
			// New operation arrived
			pendingOps = append(pendingOps, op)
			m.batchQueueSize.Add(-1)
			m.batchQueueBytes.Add(-int64(len(op.Value)))

			// Check flush triggers
			shouldFlush := false

			// Policy 1: Size-based flush
			if len(pendingOps) >= m.batchSize {
				shouldFlush = true
			}

			// Policy 2: Byte-based flush
			totalBytes := 0
			for _, o := range pendingOps {
				totalBytes += len(o.Value)
			}
			if totalBytes >= m.batchBytes {
				shouldFlush = true
			}

			// Drain more ops if available (batch opportunistically)
			if !shouldFlush {
				for len(pendingOps) < m.batchSize {
					select {
					case nextOp := <-m.batchQueue:
						pendingOps = append(pendingOps, nextOp)
						m.batchQueueSize.Add(-1)
						m.batchQueueBytes.Add(-int64(len(nextOp.Value)))
					default:
						// No more ops available
						goto checkFlush
					}
				}
			}

		checkFlush:
			if shouldFlush {
				_ = m.flushBatch(pendingOps)
				pendingOps = pendingOps[:0] // Reset slice
			}
		}

		// Update queue depth gauge
		m.batchQueueDepthGauge.Set(float64(m.batchQueueSize.Load()))
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

	// Step 2: Build rueidis commands
	ctx := context.Background()
	cmds := make([]rueidis.Completed, 0, len(result.Coalesced))
	opMap := make(map[int]*BatchOp) // index → original op for error tracking

	for _, op := range result.Coalesced {
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
		op := opMap[i]
		if err := result.Error(); err != nil {
			// Check if error is retryable
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
			m.batchOpsFlushed.WithLabelValues(op.Type.String()).Inc()
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

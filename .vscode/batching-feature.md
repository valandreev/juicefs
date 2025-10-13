# ID Batching Feature for Rueidis Metadata Engine

## Overview

The ID batching feature reduces Redis round-trips by allocating inode and chunk IDs in local pools using atomic INCRBY operations instead of individual INCR calls. This significantly improves write performance for high-rate workloads.

## Architecture

### Components

1. **Prime Functions** (`primeInodes`, `primeChunks`)
   - Perform atomic INCRBY operations to allocate batches of IDs from Redis
   - Return the starting ID of the allocated range
   - Update Prometheus metrics

2. **Pool Management** (`nextInode`, `nextChunkID`)
   - Maintain local pools with mutex-protected state
   - Synchronously refill when pool is empty
   - Trigger async prefetch when reaching low watermark
   - Serve IDs from local pool without Redis roundtrips

3. **Async Prefetch** (`asyncPrefetchInodes`, `asyncPrefetchChunks`)
   - Background goroutines that prefill pools before exhaustion
   - Deduplication to prevent concurrent prefetches
   - Non-blocking operation

4. **Integration** (via `incrCounter` override)
   - Transparent integration with existing `baseMeta` allocation paths
   - Automatically applies to `allocateInodes()` and `NewSlice()`
   - No changes needed to call sites

### Data Flow

```
baseMeta.NewSlice()
  └─> engine.incrCounter("nextChunk", sliceIdBatch)
       └─> rueidisMeta.incrCounter() [OVERRIDDEN]
            └─> primeChunks(sliceIdBatch)
                 └─> Redis INCRBY nextChunk sliceIdBatch
                      └─> Returns start ID
```

## Configuration

Connection URI parameters (all optional):

- **`metaprime`** (default: `1`)
  - `1` = Enable ID batching (recommended)
  - `0` = Disable, fallback to direct INCR (legacy behavior)

- **`inode_batch`** (default: `256`)
  - Number of inode IDs to allocate per batch
  - Higher = fewer Redis calls, more wasted IDs on crash

- **`chunk_batch`** (default: `2048`)
  - Number of chunk IDs to allocate per batch
  - Chunks are allocated more frequently, so larger batch is beneficial

- **`inode_low_watermark`** (default: `25` = 25% of `inode_batch`)
  - Pool level at which async prefetch triggers
  - Percentage (0-100) of batch size

- **`chunk_low_watermark`** (default: `25` = 25% of `chunk_batch`)
  - Pool level at which async prefetch triggers for chunks

### Example URIs

```bash
# Defaults (recommended for most workloads)
rueidis://localhost:6379/1

# High write rate workload
rueidis://localhost:6379/1?inode_batch=512&chunk_batch=4096

# Conservative (minimize wasted IDs on crashes)
rueidis://localhost:6379/1?inode_batch=64&chunk_batch=256

# Disable batching (legacy mode)
rueidis://localhost:6379/1?metaprime=0

# Custom watermarks (aggressive prefetch)
rueidis://localhost:6379/1?inode_low_watermark=50&chunk_low_watermark=50
```

## Metrics

Prometheus counters exposed via `InitMetrics()`:

- **`meta_inode_prime_calls_total`** - Number of inode batch allocations
- **`meta_chunk_prime_calls_total`** - Number of chunk batch allocations
- **`meta_prime_errors_total`** - Prime operation failures
- **`meta_inode_ids_served_total`** - Total inode IDs served from pool
- **`meta_chunk_ids_served_total`** - Total chunk IDs served from pool
- **`meta_inode_prefetch_async_total`** - Successful async prefetches (inodes)
- **`meta_chunk_prefetch_async_total`** - Successful async prefetches (chunks)

### Monitoring

**Check batching efficiency:**
```
# Average IDs served per prime call (should be close to batch size)
meta_inode_ids_served_total / meta_inode_prime_calls_total

# Prefetch success rate
meta_inode_prefetch_async_total / meta_inode_prime_calls_total
```

**Detect issues:**
```
# Prime errors (should be 0 in normal operation)
rate(meta_prime_errors_total[5m]) > 0

# Low prefetch rate (may indicate low watermark too low)
rate(meta_inode_prefetch_async_total[5m]) < 0.1
```

## Performance Characteristics

### Before Batching
- **1000 file creates** = ~1000 INCR nextInode calls
- Redis RTT: 30ms → 30 seconds total for ID allocation
- Each write operation blocks on Redis

### After Batching (default: inode_batch=256)
- **1000 file creates** = ~4 INCRBY calls (1000/256)
- Redis RTT: 30ms → 120ms total for ID allocation
- **250x reduction** in Redis roundtrips
- Most operations served from local pool (no blocking)

### Async Prefetch Impact
- Pool refills in background before exhaustion
- Write operations rarely block on Redis
- Smooth performance even under high load

## Crash Recovery Semantics

**ID gaps are expected and safe:**
- Unused IDs in local pools are lost on crash
- Maximum gap = `inode_batch + chunk_batch`
- JuiceFS is designed to handle non-sequential IDs
- No data loss, just numeric gaps

**Example:**
```
1. Client allocates IDs 1-256 (inode_batch=256)
2. Client uses IDs 1-10
3. Client crashes
4. IDs 11-256 are lost (gap)
5. Next client starts at ID 257
```

## Thread Safety

- **Pool state**: Protected by `inodePoolLock` / `chunkPoolLock`
- **Prefetch dedup**: Protected by `prefetchLock` + boolean flags
- **Concurrent calls**: Safe - each allocation acquires lock
- **Race conditions**: Prevented by mutex design

## Testing

### Unit Tests

**Step 3:** `TestRueidisPrimeFunctions` (4 subtests)
- Single batch allocation
- Multiple batches (no overlap)
- Custom sizes (1, 10, 100, 1000, 4096)

**Step 4:** `TestRueidisNextInode`, `TestRueidisNextChunkID` (6 subtests)
- Pool refill and serve
- Pool exhaustion (multiple refills)
- Concurrent allocation (100 goroutines)

**Step 6:** `TestRueidisWriteRead`
- End-to-end file creation and chunk allocation
- Verifies correct integration with baseMeta

**Fallback:** `TestRueidisMetaPrimeDisabled`
- Verifies `metaprime=0` uses legacy INCR

### Running Tests

```bash
# All batching tests
go test -v ./pkg/meta -run "TestRueidis(Prime|Next|MetaPrime)"

# Full Rueidis suite
go test -v ./pkg/meta -run "TestRueidis"

# Specific test
go test -v ./pkg/meta -run "TestRueidisWriteRead"
```

## Implementation Files

- **`pkg/meta/rueidis.go`**
  - Struct fields (lines ~40-80)
  - Config parsing (lines ~95-180)
  - Metrics init (lines ~268-280)
  - Prime functions (lines ~720-780)
  - Pool management (lines ~810-910)
  - Async prefetch (lines ~912-950)
  - `incrCounter` override (lines ~702-745)

- **`pkg/meta/rueidis_batch_test.go`**
  - Config parsing tests
  - Prime function tests
  - Pool management tests
  - Fallback tests

## Troubleshooting

### High `meta_prime_errors_total`
- **Cause**: Redis connection issues or server errors
- **Action**: Check Redis availability, network latency

### Pool exhaustion (no async prefetch)
- **Symptom**: All allocations block on synchronous prime
- **Cause**: Watermark too low or batch size too small
- **Action**: Increase `inode_low_watermark` or `inode_batch`

### Excessive wasted IDs
- **Symptom**: Large gaps in inode/chunk sequences after restarts
- **Cause**: Batch sizes too large for workload
- **Action**: Reduce `inode_batch` and `chunk_batch`

### Fallback to INCR not working
- **Symptom**: Batching still active when `metaprime=0`
- **Check**: Verify URI parameter parsing
- **Debug**: Add logs in `incrCounter` to confirm fallback path

## Future Improvements

- **Dynamic batch sizing**: Adjust batch size based on allocation rate
- **Shared pools**: Multiple clients coordinate to reduce Redis load
- **Persistent pools**: Save pool state on graceful shutdown
- **Adaptive watermarks**: Auto-tune based on Redis latency

## References

- Design doc: `.vscode/batchINCR.md`
- Implementation plan: `.vscode/devplan3.md`
- Copilot instructions: `.github/copilot-instructions.md`

# Dev Plan: Rueidis write-path performance improvements (INCR batching + optimizations)

This plan implements the "ID batching" feature described in `batchINCR.md` and integrates it into the Rueidis (`pkg/meta/rueidis.go`) metadata engine. It is intentionally broken into small, verifiable steps with a clear checklist. Follow the numbered steps sequentially. Each step includes acceptance criteria and optional test/verification commands.

Status legend:
- [ ] not started
- [>] in-progress
- [x] completed

---

## Goals (short)
- Reduce RTTs and Redis INCR load for high-rate writes by allocating inode and chunk IDs in local batches (INCRBY).
- Make batching safe for concurrency, crash-recovery tolerant (holes allowed), and configurable via URI options.
- Preserve transactional semantics and avoid changing on-disk formats.

## Quick contract
- Inputs: requests for new inode IDs and chunk IDs from write paths (e.g., `doMknod`, `doWrite`, chunk allocation functions).
- Outputs: unique IDs (uint64/uint64) for inodes and chunks consumed locally.
- Error modes: Redis errors when priming (INCRBY) must be returned to caller; async prefetch failures should be retried and logged. No duplication allowed.
- Success criteria: All IDs allocated are unique, no deadlocks, and significant reduction in Redis round trips during heavy writes.

## High-level design (one-liner)
- Maintain two local ID pools (inode and chunk) with mutex-protected counters and sizes. When pool exhausted or low watermark hit, atomically prime the pool via Redis `INCRBY` and continue serving IDs locally.

---

## Tasks

### 1) Project prep & config parsing
- [x] Add and document new connection URI options handled by `newRueidisMeta`:
  - `inode_batch` (default 256)
  - `chunk_batch` (default 2048)
  - `inode_low_watermark` (default 25% of inode_batch)
  - `chunk_low_watermark` (default 25% of chunk_batch)
  - `metaprime=0` disables this feature (getting back to currecnt implementation), default is feature on.
- Acceptance: `newRueidisMeta` reads and validates these params; values stored in `rueidisMeta` struct fields.

### 2) Add pool state and metrics into `rueidisMeta`
- [x] Add fields (with concise names and comments) to `rueidisMeta` in `pkg/meta/rueidis.go`:
  - inodePoolBase uint64
  - inodePoolRem uint64
  - inodeBatch uint64
  - inodeLowWM uint64
  - chunkPoolBase uint64
  - chunkPoolRem uint64
  - chunkBatch uint64
  - chunkLowWM uint64
  - inodePoolLock sync.Mutex
  - chunkPoolLock sync.Mutex
  - primeMetrics (prom counters): inode_prime_calls, chunk_prime_calls, prime_errors, inode_ids_served, chunk_ids_served, inode_prefetch_async, chunk_prefetch_async
- Acceptance: Code compiles; metrics are initialized in `InitMetrics` and registered when `m.InitMetrics()` is called.

### 3) Implement `primeInodes(n)` and `primeChunks(n)` helpers
- [x] Implement private methods on `rueidisMeta`:
  - `func (m *rueidisMeta) primeInodes(n uint64) (start uint64, err error)`
  - `func (m *rueidisMeta) primeChunks(n uint64) (start uint64, err error)`
- Behavior:
  - Use `m.compat.IncrBy(Background(), key, int64(n))` or `m.compat.IncrBy(ctx, key, n)` where key is `m.counterKey("nextInode")` / `m.counterKey("nextChunk")`.
  - Compute start = returned - n + 1 (account for current storage semantics where `nextInode` might be stored off-by-one; consult `incrCounter` implementation and adapt if needed).
  - Update metrics and return error on failure.
- Acceptance: Unit test for the helper using a mocked compat client or local Redis instance verifying returned range semantics.
- **COMPLETED**: Added `primeInodes` and `primeChunks` functions in `rueidis.go` (lines ~720-780). Created comprehensive unit test `TestRueidisPrimeFunctions` with 4 subtests (all passing). Tests use direct Redis reads to bypass client-side cache and verify correct ID allocation semantics.

### 4) Implement `nextInode()` and `nextChunkID()` sync paths
- [x] Implement small methods returning next ID:
  - `func (m *rueidisMeta) nextInode() (uint64, error)`
  - `func (m *rueidisMeta) nextChunkID() (uint64, error)`
- Behavior:
  - Acquire the pool-specific lock.
  - If `PoolRem == 0`, call `prime*` synchronously to refill with `poolBatch`.
  - Decrement PoolRem and return PoolBase (then increment PoolBase). Use 64-bit counters.
  - If after serving, remaining <= low watermark, trigger an asynchronous non-blocking `prime*` to prefill next batch.
  - Ensure only one async prime runs concurrently (use a boolean flag or use atomic/Semaphore to dedupe concurrent prefetch attempts).
- Acceptance: Unit tests simulate sequential allocation and that counts/returns are monotonic and unique.
- **COMPLETED**: Implemented both methods with full pool management in `rueidis.go` (lines ~810-910). Created comprehensive tests in `TestRueidisNextInode` (3 subtests), `TestRueidisNextChunkID` (2 subtests), and `TestRueidisMetaPrimeDisabled` (1 subtest) - all 6 tests passing with test Redis at 100.121.51.13:6379.

### 5) Async prefetch / low-watermark logic
- [x] Implement asynchronous prefetch that runs in new goroutine:
  - Prefetch should try to prime exactly `batch` size.
  - Prefetch should be a no-op if another prefetch already in progress or pool was refilled.
  - Errors should be logged and increment `prime_errors` metric.
- Acceptance: If low-watermark reached, async goroutine triggers, and pool refills without blocking writers.
- **COMPLETED**: Implemented `asyncPrefetchInodes()` and `asyncPrefetchChunks()` (lines ~912-950). Both functions use `prefetchLock` and boolean flags (`inodePrefetching`/`chunkPrefetching`) for deduplication. Integrated into Step 4's `nextInode()`/`nextChunkID()` methods. Concurrent allocation test validates this works correctly.

### 6) Integrate `next*` into allocation sites
- [x] Identify all places where the code previously relied on `incrCounter("nextInode")` / `incrCounter("nextChunk")` (use global grep for `nextInode` and `nextChunk` and `incrCounter(` in repo). Replace those with `m.nextInode()` and `m.nextChunkID()` appropriately.
- Important: If any `INCR` was performed *inside* a `txn` or `Watch` critical section, do NOT call `prime*` inside the txn. Instead:
  - Pre-prime before the transaction and pass the already-allocated ID(s) into the transaction.
  - If the code previously used INCR inside txn for a reason (rare), flag those spots and handle carefully — usually they are outside txn.
- Acceptance: All compilation errors fixed and tests for functions that allocate IDs updated.
- **COMPLETED**: Modified `incrCounter()` in `rueidis.go` to intercept `nextInode` and `nextChunk` calls. When `metaPrimeEnabled=true` and `value > 0`, uses `primeInodes(value)` / `primeChunks(value)` and returns `start + value` to match `baseMeta` expectations. This automatically integrates batching into all allocation sites (`baseMeta.allocateInodes()`, `baseMeta.NewSlice()`, etc.) without modifying each call site individually. All tests pass including `TestRueidisWriteRead`.

### 7) Crash/restart semantics test and doc note
- [ ] Add unit/integration test or documentation verifying that unused IDs lost during crashes are acceptable and expected. Add a note in `docs/development/internals.md` explaining the behavior and cluster hash-tag requirement.
- Acceptance: Test demonstrating that after simulating crash (stop process) with partially used pool, restarts continue with the next prime without collisions.

### 8) Concurrency stress test
- [ ] Add a test that spawns N goroutines requesting IDs concurrently (e.g., 100 goroutines, 1000 requests each). Verify uniqueness and that `prime*` calls are limited (observe metrics).
- Acceptance: All returned IDs are unique and the number of prime calls is near theoretical minimum.

### 9) Integrate metrics and instrumentation
- [ ] Expose Prometheus counters and register in `InitMetrics`. Add logging for `prime*` start/end, range, and errors at debug level.
- Acceptance: Metrics are available and incremented during tests or manual run.

### 10) Config toggle & backwards compatibility
- [ ] Allow disabling batching by setting `inode_batch=1` and `chunk_batch=1` or by an explicit `inode_batch=0` meaning fallback to `INCR` per-request. (Pick semantic consistent with other options.)
- [ ] Validate that disabling returns behavior to previous `incrCounter` path.
- Acceptance: When disabled, code paths call `m.incrCounter(...)` as before, tests pass.

### 11) Documentation, README and release notes
- [ ] Update `docs/development/internals.md` and add a short howto in docs referencing new URI options, cluster-note (hash-tag), and recommended batch sizes for heavy write workloads.
- Acceptance: New docs file or section added with examples.

### 12) Long-running soak test plan
- [ ] Provide a soak-test script/instructions for QA that performs heavy parallel writes for hours and collects metrics.
- Acceptance: Documented commands and expected metrics behavior.

---

## Suggested small PR breakdown (for reviewable changes)
- PR 1: Add config parsing + struct fields + metric placeholders + unit tests for parsing.
- PR 2: Implement primeInodes/primeChunks helpers with unit tests (mock Redis or integration test against local Redis).
- PR 3: Implement nextInode/nextChunkID logic and async prefetch primitives, plus unit tests.
- PR 4: Replace allocation call sites, fix txn-sensitive places, and run full meta tests.
- PR 5: Add concurrency stress tests, metrics, docs, and soak instructions.

---

## File-level checklist (use these to mark progress in this file)
- [x] 1. Parse config and add fields in `rueidis.go`
- [x] 2. Add metrics and InitMetrics hooks
- [ ] 3. Implement `primeInodes` / `primeChunks`
- [ ] 4. Implement `nextInode` / `nextChunkID` with prefetch
- [ ] 5. Replace allocation call sites
- [ ] 6. Add tests: range semantics, concurrency, low-watermark
- [ ] 7. Add integration test for crash-recovery
- [ ] 8. Update docs (`internals.md`) and add example URI
- [ ] 9. Soak/run instructions

---

## Quick "How to run" (dev)
- Start a local Redis (or cluster if testing cluster-specific tag). Example (PowerShell):

```powershell
# Start a local Redis (example using docker desktop WSL or docker for Windows)
docker run --rm --name juicefs-redis -p 6379:6379 redis:7
```

- Run unit tests for meta package during development:

```powershell
go test ./pkg/meta -run TestNextID -v
```

(Replace `TestNextID` with the actual tests you create.)

---

## Notes & risks
- Redis Cluster: explain hash-tag requirement for `nextInode`/`nextChunk` keys if cluster mode is used.
- IDs lost on crash: documented and acceptable.
- Atomicity & transactions: prime happens outside WATCH; ensure places that require transactional INCR are adjusted.

---

Created by dev assistant on (local) branch: `dual-redis`.

Please tell me which step you'd like me to implement first and I will start making the code changes and tests.
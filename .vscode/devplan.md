# Rueidis metadata engine development plan

## Context

- Current metadata backend `pkg/meta/redis.go` (≈5 KLOC) implements all JuiceFS metadata primitives using `go-redis/v9` (`redis.UniversalClient`) and shares helpers across auxiliary files (`redis_lock.go`, `redis_bak.go`, etc.).
- Redis URIs (`redis://`, `rediss://`, `unix://`) are wired via `meta.Register` in `pkg/meta/redis.go`, consumed by `meta.NewClient` and surfaced in CLI/config examples.
- JuiceFS tests rely on starting a real Redis server (see `pkg/meta/load_dump_test.go`, `pkg/meta/benchmarks_test.go`, `pkg/meta/random_test.go`). No new driver currently exercises those tests automatically.
- Rueidis provides:
  - Native auto-pipelining and request coalescing via `ClientOption` (enabled by default).
  - Server-assisted client-side caching via `client.DoCache` / `Cache()` helpers (requires Redis 6+ tracking).
  - `rueidiscompat` layer emulates the go-redis API, easing migration.

## Goals & acceptance criteria

1. Add a fully featured Rueidis-based metadata engine that matches all behaviour of the existing Redis backend (functional parity, config options, background jobs, backup/restore, locking, quota, ACL, dir stats, etc.).
2. Support new URI schemes `rueidis://` and `ruediss://` (TLS) handled transparently by CLI/config; UX otherwise unchanged.
3. Leverage Rueidis strengths:
   - Ensure auto-pipelining is active (no accidental disablement; keep enough connections/buffer sizes for workload).
   - Enable server-assisted client-side caching on read-heavy paths (directory listing, inode attr reads) with consistent invalidation.
4. Tests first (TDD): extend / duplicate existing integration tests so Rueidis backend is exercised and green before code merge.
5. Documentation and samples updated (`metadata.sample`, docs, README) to mention new schemes.

Assumptions / open questions
- Redis version used in CI supports tracking (Redis 6.0+). Confirm before relying on caching tests; otherwise guard tests with version detection.
- `rueidiscompat` sufficiently covers sentinel/cluster features we currently use. If gaps exist, decide whether to implement those paths using native `rueidis` APIs or document limitations.
- Decide whether to share code between `redisMeta` and new `rueidisMeta` via common helpers to reduce drift. The request is to duplicate file; we can factor reusable pieces into `redis_common.go` if it avoids copy/paste bugs.

## Environment & prerequisites

- New Go dependency: `github.com/redis/rueidis` (and `github.com/redis/rueidis/rueidiscompat`). Update `go.mod` / `go.sum` once tests fail due to missing imports.
- Ensure CI/integration redis containers expose tracking (enable with `CONFIG SET tracking-table-max-keys` if needed).
- For local dev: run Redis 7+ with `redis-server --enable-protected-config yes` to allow caching tests.
- Add feature toggle (env flag) allowing fallback to vanilla go-redis if rueidis cache causes regressions.
- Test Redis server (shared): `redis://100.121.51.13:6379` using database `/1` is available and already initialized with a JuiceFS instance. This is a dedicated test server — you can run integration tests and perform destructive operations against it. Documented here so we don't forget to use it for Rueidis integration tests.

## TDD implementation roadmap

### Phase 0 – Test scaffolding bootstrap

 ✅ `doReadlink`
 ✅ `doTruncate`
✅ `doFallocate`
✅ `doMknod`
✅ `doUnlink`
✅ `doRmdir`
✅ `doRename`
✅ `doLink`
✅ `doRead`
✅ `CopyFileRange`
✅ `Resolve`
✅ `doWrite`
✅ `doGetParents`
✅ `doSyncDirStat`
✅ `doUpdateDirStat`
✅ `doGetDirStat`
✅ `doFindDeletedFiles`
✅ `deleteChunk`
✅ `doDeleteFileData`
✅ `doCompactChunk`
✅ `scanAllChunks`
✅ `doRepair`
✅ `GetXattr`
✅ `ListXattr`
✅ `doSetXattr`
✅ `doRemoveXattr`
✅ `doGetQuota`
✅ `doSetQuota`
✅ `doDelQuota`
✅ `doLoadQuotas`
✅ `doFlushQuotas`
✅ `Reset`
✅ `setIfSmall`
✅ `doCleanStaleSession`
✅ `doFindStaleSessions`
✅ `doRefreshSession`
✅ `doFlushStats`
✅ `doSyncVolumeStat`
✅ `doCloneEntry`
✅ `doDeleteSustainedInode`

### Phase 1 – Driver skeleton & connection plumbing

1. **Implementation (minimal):**
   - ✅ `pkg/meta/rueidis.go` now defines a dedicated `rueidisMeta` wrapper that registers the Rueidis schemes, instantiates a Rueidis client via `rueidis.ParseURL` / `rueidis.NewClient`, and resets the embedded engine pointer so background jobs route through the new type.
   - ✅ All core metadata helpers have been migrated to use `rueidiscompat` instead of go-redis delegation. The `compat == nil` guards remain as a safety fallback but are not expected to trigger in normal operation.
   - ✅ A `rueidiscompat.NewAdapter` instance now hangs off `rueidisMeta`, wiring all production calls through Rueidis while maintaining full behavioral parity with the Redis backend.
   - ✅ All Phase 0 helpers (`doLoad`, `doDeleteSlice`, `doInit`, `cacheACLs`, `getSession`, `GetSession`, `ListSessions`, `doNewSession`, `cleanupLegacies`, `cleanupLeakedChunks`, `cleanupOldSliceRefs`, `cleanupLeakedInodes`, `doCleanupDetachedNode`, `doFindDetachedNodes`, `doAttachDirNode`, `doTouchAtime`, `doSetFacl`, `doGetFacl`, `loadDumpedACLs`, `doFindDeletedFiles`, `doCleanupDelayedSlices`, `doCleanupSlices`, `doCleanStaleSession`, `fillAttr`, `doReaddir`, `doLookup`, `doGetAttr`, `doSetAttr`, `getCounter`, `incrCounter`, `newDirHandler`, and all file/directory operations) now use Rueidis natively.
   - ✅ Complex operations like `doCloneEntry` (transactional inode/chunk/xattr cloning), `doSyncVolumeStat` (volume-wide stat aggregation), and all CRUD helpers are fully Rueidis-backed.
   - Preserve uniform build tags (e.g., `//go:build !norueidis`) and align configuration parsing with the Redis driver so CLI/config UX stays identical. Extend parsing to honor Rueidis-specific knobs (auto-pipelining, cache sizing) once we expose them.
   - For TLS, wire `ruediss` URIs to load a `tls.Config` via existing helpers.
2. **Tests:**
   - ✅ `TestRueidisDriverRegistered` validates that both `rueidis://` and `ruediss://` schemes are registered.
   - ✅ `TestRueidisSmoke` added as Phase 1 smoke test—exercises `meta.NewClient("rueidis://...")` against the test Redis server, verifies client creation and basic naming.
   - Ready for Phase 2: extend smoke tests with actual metadata operations (format, mkdir, write, read) once we confirm end-to-end integration works.

### Phase 2 – Command execution compatibility

1. Build helper wrappers for command execution:
   - Create `type rueidisClient struct { raw rueidis.Client; compat *rueidiscompat.Adapter }` implementing the subset of methods used in metadata code: `Get`, `Set`, `Del`, `HGet`, `HGetAll`, `HMGet`, `EvalSha`, transactions, pipelines, Pub/Sub (for locks), etc.
   - Where go-redis returns `redis.Cmd`, use `rueidiscompat` equivalents (`*StringCmd`, `*SliceCmd`, etc.).
   - Update code in `rueidis.go` to call adapter methods; keep semantics identical (error handling, nil conversions, script caching).
2. Tests: compile-time should pass; run meta tests – expect many still failing due to unimplemented helpers. Add focused unit tests for wrappers (mock redis server using `miniredis` or actual redis with ephemeral keys) verifying conversion of nil replies, scanning, etc.

### Phase 3 – Transactions, Lua scripts, and pipelines

1. Ensure multi/exec flows (`txPipeline`, watchers) use rueidis dedicated clients or `DoMulti`. Replace `redis.Tx` usage with `rueidiscompat.Tx`.
2. Load Lua scripts via `client.LoadScript` equivalents. Confirm SHA caching works; update script invocation to use `DoMulti` if necessary.
3. Tests: add new tests verifying clone/rename operations run through transactions using Rueidis (reuse existing tests by ensuring they pass for rueidis). Add targeted unit test for `scriptLookup`/`scriptResolve` path (simulate missing script -> reload).

### Phase 4 – Pub/Sub & locking semantics

1. Port `redis_lock.go` logic: duplicate file to `rueidis_lock.go` (or generalize) ensuring Flock/Plock semantics identical.
2. Use `client.Receive` / `Subscribe` from `rueidiscompat` for blocking operations. Verify timeouts and error mapping to `syscall.Errno` remain consistent.
3. Tests: extend existing lock tests (if absent, create new tests verifying posix locks behave) for both drivers. Use small integration test that acquires a lock, simulates conflict, ensures waiters release.

### Phase 5 – Backup/restore & maintenance commands

1. Duplicate/adjust `redis_bak.go`, `redis_bak_test` (if any) to `rueidis_bak.go`. Ensure scanning and background goroutines use Rueidis commands (`Scan`, `HScan`, `ZScan`).
2. Confirm `Reset`, `Flush`, counters, quota sync etc. operate correctly (look at helpers in `redis.go` using `Scan` or iterators).
3. Tests: reuse `LoadDump` tests already duplicated for Rueidis to confirm metadata dump/restore works. Add TTL/resurrection tests if required.

### Phase 6 – Client-side caching integration

1. Introduce caching strategy:
   - Identify read paths safe for caching (e.g., `doGetAttr`, `doReaddir`, `doRead` for metadata). Wrap corresponding calls with `compat.Cache(ttl)` or `DoCache`.
   - Emit invalidation after writes using `Tracking` tokens (Rueidis handles automatically if `Cache()` used and connection tracking active). Validate we call `InvalidateCache` on modifications (rueidis handles if we reuse same client?).
   - Provide configuration knob (URI query `cache-ttl=`) defaulting to reasonable TTL (e.g., 100ms) to balance staleness.
2. Tests: create new integration test enabling caching on a local Redis 7 instance; test scenario: read attr -> modify -> read again ensures stale data invalidated. Use `IsCacheHit()` assertions to verify first read caches, second read before mutation hits cache, read after mutation returns new value (i.e., cache invalidated).
3. Add metrics (optional) to expose cache hit ratio via Prometheus (tie into existing metrics in `baseMeta`).

### Phase 7 – Configuration, docs, final polish

1. Update sample config files (`pkg/meta/metadata.sample`, `metadata-sub.sample`) to mention `rueidis://` scheme and caching tunables.
2. Update docs:
   - `docs/en/reference/how_to_set_up_metadata_engine.md` with new sections for Rueidis.
   - CLI help or flags (if any) referencing redis to note Rueidis alternative.
3. Update `.github/copilot-instructions.md` + developer docs to highlight new backend and tests.
4. Ensure build tags respect `noredis`/`norueidis` (add instructions in `Makefile` if building without Rueidis).

### Phase 8 – CI & performance validation

1. Update CI scripts (GitHub Actions) to spin up Redis container for Rueidis tests (already there, but ensure caching prerequisites). Add job matrix to run meta tests for both drivers.
2. Optional: add benchmark in `cmd/bench.go` or new microbench to compare autopipelining performance (document results).
3. Verify `go test ./...` passes; run `make test.meta.core` and targeted Rueidis tests.

## Deliverables checklist

- [ ] New `pkg/meta/rueidis.go` (+ supporting files) implementing `Meta` interface via Rueidis.
- [ ] Tests covering Rueidis path (unit + integration) running in CI.
- [ ] Config & docs updated (schemes, samples, instructions).
- [ ] Build passes (`go test ./...`, `make test.meta.core`).
- [ ] Optional benchmarks/metrics demonstrating Rueidis advantages.

## Risks & mitigations

- **Complexity drift:** Duplication of 5K LOC invites maintenance issues. Mitigate by extracting shared helpers prior to copying (e.g., formatting functions, key builders).
- **Caching correctness:** Server-assisted caching must invalidate promptly. Mitigate with exhaustive tests around directory modifications and attr updates; provide escape hatch (disable via query param) if issues arise.
- **Compatibility gaps:** Rueidis sentinel/cluster parity may differ. Create conformance tests connecting to mock sentinel/cluster; fall back to go-redis for unsupported features or document limitations.
- **CI stability:** Additional integration tests may increase runtime. Optimize by reusing existing Redis containers and gating heavy tests behind short TTL/timeouts.

## Next actions

1. Commit the initial Rueidis driver registration that delegates to the Redis engine, ensuring tests compile and the sentinel stays green.
2. Begin carving out a dedicated `rueidisMeta` implementation that uses `rueidiscompat` for command coverage; swap the delegate once the skeleton compiles.
3. Expand the redis-like harness defaults to include Rueidis endpoints (with skip guards) after the native Rueidis client can reach a test server.

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

1. **Tests:**
   - Extend `pkg/meta/load_dump_test.go` and other existing Redis-backed tests to accept a `metaDriver` parameter. Duplicate current Redis cases for `rueidis://` and `ruediss://` URIs (behind `t.Run("rueidis", ...)`). Expect initial failures because driver not registered yet.
   - Add a lightweight unit test in a new `pkg/meta/rueidis_test.go` verifying `meta.NewClient("rueidis://...")` returns a `Meta` instance whose `Name()` is `"rueidis"`.
2. **Support code:** introduce shared helper (e.g., `testRedisLike(t, scheme string)`) to reduce duplication between redis and rueidis test cases. Keep tests skipped when Redis not available (use existing `testTarget` detection).

### Phase 1 – Driver skeleton & connection plumbing

1. **Implementation (minimal):**
   - Copy `pkg/meta/redis.go` to `pkg/meta/rueidis.go`, adjust build tags (e.g., `//go:build !norueidis`) and rename types (`redisMeta` → `rueidisMeta`).
   - Replace `redis.UniversalClient` usages with a thin interface satisfied by `rueidiscompat.Adapter` (`type redisCmdable interface {...}`) so the bulk of logic compiles.
   - Register new drivers in `init(): Register("rueidis", newRueidisMeta)` and `Register("ruediss", newRueidisMeta)`.
   - Minimal `newRueidisMeta` reuses query parsing logic but builds `rueidis.ClientOption` from URI. For TLS, use `MustParseURL` and set `TLSConfig.
   - Keep auto-pipelining defaults (do not set `DisableAutoPipelining`). Configure connection pool similar to go-redis (matching `PoolSize`, etc.).
2. **Tests:** rerun updated tests; they should now fail deeper because many methods still expect go-redis return types. Fix compilation/test failures iteratively while keeping tests red.

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

1. Confirm Redis version in CI supports tracking (otherwise, adjust caching plan).
2. Introduce shared test harness for Redis-like backends (Phase 0).
3. Execute TDD phases sequentially, ensuring green status before proceeding to next phase.

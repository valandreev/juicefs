Dev Plan: Enable Client-Side Caching (CSC) for Rueidis (implementation checklist)

Goal: Fully enable Rueidis client-side caching so reads use local cache with server-side invalidation, ensuring high read performance with correct consistency.

How to use this file: Each step includes a short description and a checkbox. After you complete a step, update the checkbox to [x] and commit the change. For code changes, link to PR/commit SHA next to the step.

1. Design cached-read helpers
- [ ] Decide TTL default (suggest: 1h) and ensure TTL is sourced only from the connection URI `?ttl=` (e.g., `?ttl=2h`, `?ttl=10s`, `?ttl=0` to disable). Remove reliance on any `meta.cache-ttl` config for Rueidis connections.
- [ ] Define helper APIs to implement in `pkg/meta/rueidis.go`:
  - `cachedGet(ctx Context, key string) ([]byte, error)`
  - `cachedHGet(ctx Context, key string, field string) ([]byte, error)`
  - `doMultiCache(ctx Context, keys []string) ([][]byte, error)`
- Notes: Map `rueidiscompat.Nil` -> ENOENT. Helpers must accept `Context` for tracing and timeouts.

2. Implement cached helpers
- [ ] Implement `cachedGet` using `m.compat.Cache(ttl).Get(ctx, key)` or Rueidis equivalent
- [ ] Implement `cachedHGet` using `.Cache(ttl).HGet(ctx, key, field)`
- [ ] Implement `doMultiCache` using `DoMultiCache` or `MGetCache` for batches
- [ ] Add unit tests for these helpers (Nil case, normal cache hit/miss behavior)

3. Replace read paths to use cached helpers
- [ ] `GetAttr` / `doGetAttr`: replace `m.compat.Get` with `cachedGet`
- [ ] `doLookup`: use `cachedHGet` for entry:{parent} and `cachedGet` for inode:{id}`
- [ ] `fillAttr` / `Readdir`: use `doMultiCache` to prime batch of inode keys or call `cachedGet` in a loop
- [ ] `GetXattr`, `Readlink`, `StatFS`: replace direct reads with cached helpers
- [ ] Ensure code paths that currently used rueidiscompat for direct reads are updated accordingly

4. Preserve non-cacheable paths
- [ ] Confirm `rueidis_lock.go` and `rueidis_bak.go` keep direct `m.compat` reads
- [ ] Add comments explaining why cache is disabled in these files (consistency/backup correctness)

5. Write-paths and optional priming
- [ ] Audit all write operations (Create, Unlink, Rename, SetAttr, Truncate, Fallocate, Mknod) to ensure they update the same keys used by read helpers
- [ ] Optionally implement a feature-flag controlled `postWritePrime` that runs a `cachedGet` for just-written keys (disabled by default)
- [ ] Add minimal unit tests verifying that after a write, another client sees change immediately (integration test)

6. Metrics, tracing and diagnostics
- [ ] Add counters for cache hits and misses in `pkg/metric` (rueidis_cache_hits, rueidis_cache_miss)
- [ ] Expose a debug endpoint or CLI flag that calls `CLIENT TRACKINGINFO` on Redis to show tracked keys and invalidations
- [ ] Log a one-line debug when invalidation helper runs (e.g., `invalidateInodeCache`) so you can trace invalidations

7. Tests and integration
- [ ] Add unit tests for the read-write-consistency scenarios:
  - same-client write->read should see update
  - different-client write->read should see update
  - large TTL (1h) test where write must be visible immediately
- [ ] Create small integration harness using ephemeral Redis (Docker) + two JuiceFS clients (or two processes) to validate invalidation behavior

8. Config, docs and rollout
- [ ] Document that TTL is controlled only via the connection URI `?ttl=` for Rueidis (no `meta.cache-ttl` config). Provide examples and migration notes.
- [ ] Document usage in `docs/en/reference` and `.vscode/metafix.md` (link to this PR and tests)
- [ ] Add example CLI/testing flags (e.g., a `--rueidis-ttl` option for developers), but avoid introducing a persistent `meta.cache-ttl` config; prefer URI-based TTL for production.

9. Code review & monitored rollout
- [ ] Open PR, request reviews from Rueidis and JuiceFS maintainers
- [ ] Merge behind feature flag or opt-in config
- [ ] Deploy to canary hosts, monitor metrics and errors for 24-72h, then roll out globally

Appendix: Quick test commands (Windows PowerShell)

# From repo root (build juicefs)
go build -ldflags="-s -w" -o juicefs.exe .

# Run a quick integration test (example using dockerized redis)
docker run --name test-redis -p 6379:6379 -d redis:7
# Start two juicefs clients (or processes) pointing to same Redis and exercise write-then-read


Notes:
- Don't cache in transactional code paths; transactions must use direct reads.
- Prefer server-side invalidation; avoid eager priming unless necessary.
- Consider switching to OPTIN tracking later for large-scale deployments.

Configuration defaults (policy)
- [ ] CSC (client-side caching) must be ENABLED by default for the Rueidis metadata client in BCAST mode. This is the default behavior when the metadata URI does not explicitly disable caching.
- [ ] To explicitly disable CSC for a Rueidis connection, use the TTL query parameter `?ttl=0` in the metadata URI, e.g.:
  - `rueidis://ip:6379/0?ttl=0` (disable caching)
- [ ] To set TTL via the metadata URI use `?ttl=` with a duration suffix, examples:
  - `rueidis://ip:6379/0?ttl=2h` sets caching TTL to 2 hours
  - `rueidis://ip:6379/0?ttl=10s` sets caching TTL to 10 seconds
- [ ] TTL for Rueidis is controlled only via the connection URI `?ttl=`. Legacy `meta.cache-ttl` (if present) is ignored for Rueidis connections; document migration path if necessary.
- [ ] These defaults and URI parsing apply ONLY to the Rueidis metadata client. The existing go-redis (`redis.go`) implementation remains unchanged and continues to operate without automatic client-side caching unless explicitly adapted later.


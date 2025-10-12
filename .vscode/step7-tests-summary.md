# Step 7: Tests and Integration - Summary

## Problem Statement
Production issue: when writing files via JuiceFS with Rueidis metadata client (default 1h TTL), other clients could not read the files immediately. File size appeared as zero until very short TTL was used, and `?prime=1` parameter did not help.

## Root Cause Analysis

### Investigation Process
1. Created comprehensive unit and integration tests to isolate the problem
2. Tests revealed:
   - ✅ Same-client consistency worked (client sees its own writes)
   - ❌ Cross-client consistency failed (client B couldn't see client A's writes)
   - ❌ Direct `compat.Get()` returned correct data, but `cachedGet()` returned ENOENT
   - ✅ HGet/cachedHGet worked correctly for directory entries

### The Bug
**Location**: `pkg/meta/rueidis.go` lines 130-132

**Before (broken)**:
```go
prefix := base.prefix
if prefix == "" {
    prefix = "jfs"  // ❌ Hardcoded fallback
}
opt.ClientTrackingOptions = []string{
    "PREFIX", prefix + "i",  // Becomes "jfsi" for standalone Redis
    "PREFIX", prefix + "d",  // Becomes "jfsd"
    ...
}
```

**The Problem**:
- For standalone Redis, `base.prefix` is empty string `""`
- Code added hardcoded `"jfs"` fallback for tracking prefixes
- This made tracking look for keys like `jfsi88888`, `jfsd123`, etc.
- But actual keys stored in Redis are `i88888`, `d123` (no prefix!)
- Result: CLIENT TRACKING never sent INVALIDATE messages because key patterns didn't match

**After (fixed)**:
```go
prefix := base.prefix
// Note: prefix is empty for standalone Redis, or "{DB}" for cluster mode
// We must track keys as they are actually stored, not with a default "jfs" prefix
opt.ClientTrackingOptions = []string{
    "PREFIX", prefix + "i",  // Becomes "i" for standalone, "{DB}i" for cluster
    "PREFIX", prefix + "d",  // Matches actual key format
    ...
}
```

## Tests Implemented

### 1. `TestRueidis_SameClientWriteReadConsistency`
**File**: `pkg/meta/rueidis_consistency_test.go`

**Purpose**: Verify that a single client sees its own writes immediately through cached helpers

**Scenario**:
- Client writes inode attribute via `compat.Set()`
- Same client reads back via `cachedGet()`
- Client writes directory entry via `compat.HSet()`
- Same client reads back via `cachedHGet()`

**Result**: ✅ PASS (always worked, even before fix)

### 2. `TestRueidis_CrossClientWriteReadConsistency`
**File**: `pkg/meta/rueidis_consistency_test.go`

**Purpose**: Verify that client A's writes are immediately visible to client B through cached helpers (the critical production scenario)

**Scenario**:
- Client A writes inode key via `compat.Set()`
- Client B reads via `cachedGet()` (polls up to 5s)
- Client A writes directory entry via `compat.HSet()`
- Client B reads via `cachedHGet()` (polls up to 5s)

**Before Fix**: ❌ FAIL
- `cachedGet()` returned ENOENT for 5+ seconds
- Direct `compat.Get()` showed data was in Redis
- CLIENT TRACKINGINFO showed prefixes: `[jfsc jfsd jfsi jfsp jfss jfsx]`
- Actual keys: `i88888`, `d2` (no prefix match!)

**After Fix**: ✅ PASS
- Both `cachedGet()` and `cachedHGet()` succeed on attempt 0
- CLIENT TRACKINGINFO now shows prefixes: `[c d i p s x]`
- Matches actual key format

### 3. `TestRueidis_LargeTTLConsistency`
**File**: `pkg/meta/rueidis_consistency_test.go`

**Purpose**: Verify that even with default 1h TTL, writes are immediately visible via BCAST invalidation

**Scenario**:
- Both clients connect with `?ttl=1h` (default)
- Client A writes inode key
- Client B reads via `cachedGet()` within 3s

**Result**: ✅ PASS (works correctly with BCAST invalidation)

### 4. `TestRueidisIntegration_Invalidate`
**File**: `pkg/meta/rueidis_integration_test.go`

**Purpose**: Minimal integration test for CI/regression testing

**Scenario**:
- Two clients connect to same Redis DB
- Client A writes keys matching tracked prefixes
- Client B polls `cachedHGet` and `cachedGet` for up to 3s
- Verifies cross-client invalidation works

**Result**: ✅ PASS (30 attempts × 100ms, minimal logging)

## Files Changed

### Core Fix
1. **pkg/meta/rueidis.go**
   - Line 130-137: Removed hardcoded "jfs" fallback for CLIENT TRACKING prefixes
   - Now uses actual `base.prefix` value (empty for standalone, `{DB}` for cluster)

### Tests Added
2. **pkg/meta/rueidis_consistency_test.go** (NEW)
   - 3 comprehensive unit/integration tests
   - ~250 lines with detailed diagnostics
   - Uses remote Redis server (100.121.51.13:6379)

3. **pkg/meta/rueidis_integration_test.go** (updated)
   - Minimal regression test for CI
   - Cleaned up extra diagnostics
   - Poll window: 30×100ms

### Documentation
4. **.vscode/devplan2.md**
   - Step 7 marked complete
   - Documented root cause and fix

5. **.vscode/step7-tests-summary.md** (NEW, this file)
   - Comprehensive summary of investigation and fix

## Test Results

### Before Fix
```
=== RUN   TestRueidis_CrossClientWriteReadConsistency
    rueidis_consistency_test.go:127: attempt 0-49: cachedGet error: The system cannot find the file specified.
    rueidis_consistency_test.go:137: FAILED: client B cachedGet did not see client A's write after 5s
    rueidis_consistency_test.go:140:   Direct compat.Get value: "cross-client-test-data" (err=<nil>)
    rueidis_consistency_test.go:144:   Client B tracking info: map[flags:[on bcast] prefixes:[jfsc jfsd jfsi jfsp jfss jfsx]]
--- FAIL: TestRueidis_CrossClientWriteReadConsistency (7.08s)
```

### After Fix
```
=== RUN   TestRueidis_SameClientWriteReadConsistency
--- PASS: TestRueidis_SameClientWriteReadConsistency (0.69s)
=== RUN   TestRueidis_CrossClientWriteReadConsistency
    rueidis_consistency_test.go:123: client B read succeeded on attempt 0
    rueidis_consistency_test.go:173: client B HGet succeeded on attempt 0
--- PASS: TestRueidis_CrossClientWriteReadConsistency (1.92s)
=== RUN   TestRueidis_LargeTTLConsistency
--- PASS: TestRueidis_LargeTTLConsistency (1.45s)
PASS
ok      github.com/juicedata/juicefs/pkg/meta   4.792s
```

## Production Impact

### Before Fix
- Files written by one JuiceFS client were invisible to other clients
- File reads returned zero size / ENOENT
- Only workaround: set very short TTL like `?ttl=1s` (defeats purpose of caching)
- `?prime=1` parameter had no effect (wasn't the root cause)

### After Fix
- Cross-client invalidation works correctly
- Default 1h TTL is safe and performant
- BCAST mode delivers invalidation within milliseconds
- No need for short TTL or manual cache priming

## Verification Steps

To verify the fix in your environment:

```powershell
# 1. Rebuild JuiceFS
go build -ldflags="-s -w" -o juicefs.exe .

# 2. Run the consistency tests
go test -v -run "TestRueidis_(SameClient|CrossClient|LargeTTL)" ./pkg/meta -timeout 2m

# 3. Run the integration test
go test -v -run TestRueidisIntegration_Invalidate ./pkg/meta -tags=integration

# 4. Test in production (two terminals)
# Terminal 1:
.\juicefs.exe mount rueidis://YOUR_REDIS:6379/DB Y:

# Terminal 2:
.\juicefs.exe mount rueidis://YOUR_REDIS:6379/DB Z:

# Write from Y:, read from Z: immediately - should see the file
```

## Lessons Learned

1. **Always match tracked prefixes to actual key format**
   - CLIENT TRACKING is case-sensitive and exact-match
   - Empty prefix means track keys with no prefix (not "jfs" default)

2. **Test cross-client scenarios early**
   - Same-client tests can pass even when invalidation is broken
   - Production issues often involve multiple processes/clients

3. **Log actual vs expected key patterns**
   - CLIENT TRACKINGINFO is invaluable for debugging
   - Compare tracked prefixes against actual Redis KEYS output

4. **Rueidis BCAST mode requires correct setup**
   - Server must support CLIENT TRACKING (Redis 6.0+)
   - Prefixes must exactly match stored key format
   - When working, invalidation is near-instant (< 100ms in tests)

## Next Steps

- ✅ Step 7 complete (all tests passing)
- Next: Step 8 (Config, docs) - already complete
- Next: Step 9 (Code review & monitored rollout)
  - Open PR with this fix and comprehensive tests
  - Request review from Rueidis and JuiceFS maintainers
  - Deploy to canary environment
  - Monitor Prometheus metrics for 24-72h before wide rollout

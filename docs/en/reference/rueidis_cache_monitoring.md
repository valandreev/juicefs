---
title: Rueidis Client-Side Cache Monitoring and Troubleshooting
sidebar_position: 3
description: Guide for monitoring, debugging, and optimizing Rueidis client-side caching in JuiceFS metadata operations.
---

# Rueidis Client-Side Cache Monitoring and Troubleshooting

This guide covers monitoring, debugging, and tuning the Rueidis client-side cache feature in JuiceFS.

## Overview

JuiceFS uses [Rueidis](https://github.com/redis/rueidis), a high-performance Redis client with built-in client-side caching support. The caching is implemented using Redis **BCAST (broadcast) mode tracking**, which provides:

- **Automatic invalidation**: Redis server sends INVALIDATE messages when cached keys change
- **Strong consistency**: Clients never see stale data, even with long cache TTLs
- **Zero configuration**: Works out-of-the-box with Redis 6.0+
- **Low overhead**: Server tracks key prefixes, not individual keys

## How It Works

### Cache Architecture

```
┌─────────────────────────────────────────────────────┐
│ JuiceFS Client (Rueidis)                            │
│                                                     │
│  ┌──────────────┐      ┌──────────────────────┐   │
│  │ Application  │──────▶│ Metadata Operations  │   │
│  │   (FUSE)     │      │  (GetAttr, Lookup)   │   │
│  └──────────────┘      └──────────────────────┘   │
│                                │                    │
│                                ▼                    │
│                    ┌──────────────────┐            │
│                    │ Cached Helpers   │            │
│                    │  cachedGet()     │            │
│                    │  cachedHGet()    │            │
│                    │  cachedMGet()    │            │
│                    └──────────────────┘            │
│                            │                        │
│              ┌─────────────┴─────────────┐         │
│              ▼                           ▼         │
│      ┌───────────────┐           ┌──────────────┐ │
│      │ Client Cache  │           │ Redis Server │ │
│      │  (in-memory)  │◀──────────│  (source)    │ │
│      └───────────────┘ INVALIDATE└──────────────┘ │
│                                                     │
└─────────────────────────────────────────────────────┘
```

### Read Flow

1. **Cache Hit**: `cachedGet()` checks local cache → returns data immediately (no network)
2. **Cache Miss**: Fetch from Redis → store in cache with TTL → track key for invalidation
3. **Invalidation**: Redis sends INVALIDATE message → client evicts cached entry → next read is cache miss

### Write Flow

1. JuiceFS writes to Redis (SET/HSET/DEL/HDEL)
2. Redis identifies affected clients tracking those keys
3. Redis sends INVALIDATE messages to all tracking clients
4. Clients evict cached entries
5. Next read fetches fresh data from Redis

**Result**: Strong consistency with cache performance benefits.

## Configuration

### URI Parameters

Control caching behavior via metadata URI query parameters:

```
rueidis://host:6379/db?ttl=<duration>&prime=<0|1>
```

**Parameters:**

| Parameter | Default | Description | Examples |
|-----------|---------|-------------|----------|
| `ttl` | `1h` | Cache entry lifetime. Use `0` to disable caching. | `10s`, `30m`, `1h`, `2h` |
| `prime` | `0` | Enable post-write cache priming (optional). | `0` (disabled), `1` (enabled) |

**Examples:**

```bash
# Default: 1 hour cache TTL
rueidis://192.168.1.6:6379/1

# Custom: 2 hour cache TTL
rueidis://192.168.1.6:6379/1?ttl=2h

# Short TTL for high-churn workloads
rueidis://192.168.1.6:6379/1?ttl=10s

# Disable caching (for testing/debugging)
rueidis://192.168.1.6:6379/1?ttl=0

# Enable post-write priming (not recommended)
rueidis://192.168.1.6:6379/1?ttl=1h&prime=1
```

### Choosing TTL

| Workload Type | Recommended TTL | Rationale |
|---------------|----------------|-----------|
| Read-heavy (file serving, ML training data) | `1h` - `2h` | Maximize cache hits, invalidation handles consistency |
| Balanced (general purpose) | `30m` - `1h` | Good balance, default setting |
| Write-heavy (active development, logs) | `10s` - `30m` | Lower memory usage, fewer invalidations |
| Multi-client writes (collaboration) | `10s` - `30m` | Frequent invalidations, shorter TTL reduces overhead |
| Testing / Debugging | `0` | Disable caching to isolate issues |

:::tip
Start with the default `1h` TTL and adjust based on monitoring data. BCAST invalidation handles consistency, so longer TTLs are safe.
:::

## Monitoring

### Prometheus Metrics

Rueidis exposes cache metrics via Prometheus:

#### `rueidis_cache_hits_total`

**Type**: Counter  
**Description**: Count of cache-enabled operations (TTL > 0)  
**When it increments**: Every metadata read operation when caching is enabled

#### `rueidis_cache_misses_total`

**Type**: Counter  
**Description**: Count of direct operations (TTL = 0, caching disabled)  
**When it increments**: Every metadata read operation when caching is disabled (`?ttl=0`)

:::note
These metrics track **operational mode** (cache-enabled vs direct), not actual cache efficiency. Rueidis client doesn't expose true cache hit/miss rates from its internal cache.
:::

### Useful Queries

#### Cache Operation Rate

```promql
# Cache-enabled operations per second
rate(rueidis_cache_hits_total[5m])

# Direct operations per second (caching disabled)
rate(rueidis_cache_misses_total[5m])
```

#### Cache vs Direct Ratio

```promql
# Percentage of operations using cache
rueidis_cache_hits_total / (rueidis_cache_hits_total + rueidis_cache_misses_total) * 100
```

#### Expected Patterns

- **100% cache hits, 0% misses**: Caching enabled (`?ttl=1h`)
- **0% cache hits, 100% misses**: Caching disabled (`?ttl=0`)
- **Mixed values**: Configuration error (some clients have different TTL settings)

### Redis Server Metrics

Check Redis tracking state using `CLIENT TRACKINGINFO`:

```bash
# From redis-cli
redis-cli CLIENT TRACKINGINFO

# Expected output (when JuiceFS is connected):
flags: on bcast
redirect: -1
prefixes:
  - jfs1c_i
  - jfs1c_d
  - jfs1c_c
  - jfs1c_x
  - jfs1c_p
  - jfs1c_symlink
  - jfs1c_usedSpace
  - jfs1c_usedInodes
  - jfs1c_dataLength
  - jfs1c_counter
  - jfs1c_setting
```

**Key fields:**

- `flags`: Should include `on` and `bcast`
- `prefixes`: List of key prefixes being tracked (should match JuiceFS key patterns)
- `num-keys`: Number of keys currently tracked (grows with cache usage)

:::warning
If `flags` is empty or missing `bcast`, client-side caching is not active. Check Redis version (must be 6.0+).
:::

## Debugging

### Programmatic Cache Inspection

JuiceFS provides `GetCacheTrackingInfo()` method for debugging (Go API):

```go
package main

import (
    "fmt"
    "github.com/juicedata/juicefs/pkg/meta"
)

func inspectCache() {
    m := meta.NewClient("rueidis://192.168.1.6:6379/1?ttl=1h", &meta.Config{})
    defer m.Shutdown()

    rueidisClient := m.(*meta.RueidisMeta)
    info, err := rueidisClient.GetCacheTrackingInfo(meta.Background())
    if err != nil {
        panic(err)
    }

    fmt.Printf("Cache TTL: %v\n", info["juicefs_cache_ttl"])
    fmt.Printf("Tracking flags: %v\n", info["flags"])
    fmt.Printf("Tracked prefixes: %v\n", info["prefixes"])
    fmt.Printf("Tracked keys: %v\n", info["num-keys"])
    fmt.Printf("Prime enabled: %v\n", info["juicefs_enable_prime"])
}
```

**Output (caching enabled):**

```
Cache TTL: 1h0m0s
Tracking flags: [on bcast]
Tracked prefixes: [jfs1c_i jfs1c_d jfs1c_c ...]
Tracked keys: 1247
Prime enabled: false
```

**Output (caching disabled):**

```json
{
  "status": "caching disabled (ttl=0)"
}
```

### Common Issues

#### Issue: All Operations Show as "Misses"

**Symptoms:**
- `rueidis_cache_misses_total` increasing
- `rueidis_cache_hits_total` at zero

**Causes:**
1. Caching disabled in URI (`?ttl=0`)
2. Configuration error (wrong parameter name)

**Solutions:**
```bash
# Check current mount URI
ps aux | grep juicefs

# Verify TTL parameter is set
# WRONG: ?cache-ttl=1h  (old parameter name)
# RIGHT: ?ttl=1h        (correct parameter name)

# Remount with correct URI
juicefs umount /mnt/jfs
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=1h" /mnt/jfs
```

#### Issue: Redis Not Sending Invalidations

**Symptoms:**
- Stale data observed
- CLIENT TRACKINGINFO shows `flags: []` (empty)

**Causes:**
1. Redis version < 6.0 (no CLIENT TRACKING support)
2. Redis configuration blocking tracking

**Solutions:**
```bash
# Check Redis version
redis-cli INFO server | grep redis_version
# Must be 6.0.0 or higher

# Check tracking support
redis-cli CLIENT TRACKINGINFO
# Should show "flags: on bcast"

# If not working, upgrade Redis:
# Ubuntu/Debian: apt install redis-server=6:6.2.6-1rl1~focal1
# Docker: docker run -d -p 6379:6379 redis:7
```

#### Issue: High Memory Usage on Redis

**Symptoms:**
- Redis memory growing continuously
- CLIENT TRACKINGINFO shows very high `num-keys`

**Causes:**
1. Long cache TTL with high file churn
2. Many connected clients

**Solutions:**
```bash
# Reduce cache TTL
rueidis://192.168.1.6:6379/1?ttl=10s  # From 1h to 10s

# Monitor tracking table size
redis-cli CLIENT TRACKINGINFO | grep num-keys

# Consider switching to OPTIN mode (future enhancement)
# Currently not implemented in JuiceFS
```

#### Issue: Inconsistent Performance

**Symptoms:**
- Variable latency
- Some operations fast, others slow

**Diagnosis:**
```bash
# Check if caching is active
redis-cli CLIENT TRACKINGINFO

# Monitor cache metrics
curl http://localhost:9567/metrics | grep rueidis_cache

# Check network latency to Redis
ping -c 10 192.168.1.6
```

**Solutions:**
- Ensure stable network connection to Redis
- Verify all JuiceFS clients use same TTL setting
- Check Redis server load (CPU, memory)

## Performance Tuning

### TTL Optimization

**Experiment with different TTLs:**

```bash
# Baseline: No caching
ttl=0  → measure latency

# Conservative: Short TTL
ttl=10s  → measure latency, cache hit rate, invalidations

# Moderate: Default TTL
ttl=1h  → measure latency, cache hit rate, invalidations

# Aggressive: Long TTL
ttl=2h  → measure latency, cache hit rate, invalidations
```

**Metrics to track:**

1. **Average metadata operation latency** (from JuiceFS logs or metrics)
2. **Redis server load** (CPU, network traffic)
3. **Cache operation rate** (`rate(rueidis_cache_hits_total[5m])`)
4. **Memory usage** (Redis and JuiceFS client)

### Post-Write Priming

**Default behavior (recommended):**
```bash
# prime=0 (disabled)
rueidis://192.168.1.6:6379/1?ttl=1h
```

**Pros**: Lower write latency, relies on server-side invalidation  
**Cons**: First read after write might be cache miss

**Enabled priming:**
```bash
# prime=1 (enabled)
rueidis://192.168.1.6:6379/1?ttl=1h&prime=1
```

**Pros**: First read after write is cache hit  
**Cons**: Higher write latency (extra read operation)

:::tip
Server-side invalidation is usually sufficient. Only enable `prime=1` if profiling shows significant benefit for your specific workload.
:::

### Workload-Specific Tuning

#### Machine Learning Training (Read-Heavy)

```bash
# Long TTL, dataset rarely changes
rueidis://192.168.1.6:6379/1?ttl=2h

# Expected: 90%+ cache hits, low Redis load
```

#### Active Development (Write-Heavy)

```bash
# Short TTL, frequent file changes
rueidis://192.168.1.6:6379/1?ttl=10s

# Expected: More cache misses, frequent invalidations
```

#### Multi-Tenant Collaboration

```bash
# Balanced TTL, multiple writers
rueidis://192.168.1.6:6379/1?ttl=30m

# Monitor invalidation rate, adjust TTL if high
```

## Best Practices

### Production Deployment

1. **Start conservative**: Use default `ttl=1h`, monitor for 24-48 hours
2. **Monitor metrics**: Track `rueidis_cache_hits_total` and Redis `CLIENT TRACKINGINFO`
3. **Gradual tuning**: Adjust TTL in small increments (±30m) based on data
4. **Document settings**: Record TTL in deployment configs, explain rationale

### Monitoring Checklist

- [ ] Prometheus metrics collection configured
- [ ] Alerts set up for cache misconfiguration (0% hits when expected)
- [ ] Redis server monitoring (CPU, memory, network)
- [ ] CLIENT TRACKINGINFO queried periodically (verify BCAST active)
- [ ] JuiceFS client logs reviewed for cache-related warnings

### Troubleshooting Workflow

1. **Verify caching enabled**: Check `rueidis_cache_hits_total` is increasing
2. **Check Redis version**: Must be 6.0+
3. **Inspect tracking state**: Run `CLIENT TRACKINGINFO`, verify `flags: on bcast`
4. **Review URI syntax**: Ensure `?ttl=1h` not `?cache-ttl=1h`
5. **Test invalidation**: Write file → verify change visible on other clients
6. **Compare with Redis client**: Switch to `redis://` temporarily, measure performance difference

## Migration Guide

### From Redis to Rueidis

**Before:**
```bash
juicefs mount -d "redis://192.168.1.6:6379/1" /mnt/jfs
```

**After:**
```bash
# Start conservative with short TTL
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=10s" /mnt/jfs

# Monitor for 24 hours, then increase TTL
juicefs umount /mnt/jfs
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=1h" /mnt/jfs
```

**No metadata changes needed** - Rueidis and Redis use identical on-disk format.

### Rollback Plan

If issues occur, rollback is simple:

```bash
juicefs umount /mnt/jfs
juicefs mount -d "redis://192.168.1.6:6379/1" /mnt/jfs
```

Or disable caching while keeping Rueidis:

```bash
juicefs umount /mnt/jfs
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=0" /mnt/jfs
```

## Advanced Topics

### Understanding BCAST vs OPTIN

**BCAST (Broadcast) Mode** - Current Implementation:
- Server tracks key prefixes
- All matching keys trigger invalidation
- Clients receive invalidations for all tracked prefixes
- Pros: Simple, automatic, works for all metadata operations
- Cons: More invalidation messages with many clients

**OPTIN Mode** - Future Enhancement:
- Client explicitly requests tracking per key
- Fewer invalidation messages
- Requires manual opt-in logic
- Better for large-scale deployments (100+ clients)

:::note
JuiceFS currently uses BCAST mode exclusively. OPTIN mode support may be added in future versions.
:::

### Cache Internals

Cached data includes:

1. **Inode attributes** (`GET i:{ino}`) - file/directory metadata
2. **Directory entries** (`HGET d:{parent} {name}`) - file listings
3. **Extended attributes** (`HGET x:{ino} {name}`) - xattrs, ACLs
4. **Directory stats** (`HGET {dirUsedSpace|dirDataLength|dirUsedInodes} {ino}`)
5. **Quota usage** (`HGET {usedSpace|usedInodes} {quota_id}`)
6. **Counters** (`GET counter`)
7. **Settings** (`GET setting`)

**Non-cached operations** (always direct):
- Transaction reads (`tx.Get`, `tx.HGet`)
- Lock operations (strong consistency required)
- Backup/restore (authoritative data required)

See `pkg/meta/rueidis.go` for implementation details.

## Conclusion

Rueidis client-side caching provides significant performance benefits for read-heavy JuiceFS workloads while maintaining strong consistency through server-assisted invalidation. Start with default settings, monitor metrics, and tune TTL based on your specific workload characteristics.

**Key Takeaways:**

- Default `ttl=1h` works well for most use cases
- BCAST invalidation ensures consistency automatically
- Monitor `rueidis_cache_hits_total` and `CLIENT TRACKINGINFO`
- Adjust TTL based on read/write ratio and latency requirements
- Rollback to `redis://` is simple if needed

For questions or issues, refer to:
- [JuiceFS GitHub Issues](https://github.com/juicedata/juicefs/issues)
- [Rueidis Documentation](https://github.com/redis/rueidis)
- [Redis CLIENT TRACKING Documentation](https://redis.io/commands/client-tracking/)

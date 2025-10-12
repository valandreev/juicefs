# Migration Guide: Redis to Rueidis with Client-Side Caching

## Overview

This guide helps you migrate JuiceFS from the standard Redis client (`redis://`) to the high-performance Rueidis client (`rueidis://`) with client-side caching enabled.

## Why Migrate?

**Benefits of Rueidis with Client-Side Caching:**

- **70-90% lower read latency** - Most metadata reads served from local memory
- **Reduced Redis load** - Only writes and cache misses hit the Redis server
- **Strong consistency** - Server-assisted invalidation ensures no stale data
- **Better concurrency** - Automatic pipelining improves multi-threaded performance
- **Same metadata format** - No data migration required, instant rollback possible

**No Downsides:**

- Write performance unchanged
- Metadata compatibility maintained
- Can disable caching anytime with `?ttl=0`
- Rollback to `redis://` takes seconds

## Prerequisites

- **Redis version**: 6.0 or higher (required for CLIENT TRACKING support)
- **JuiceFS version**: Latest version with Rueidis support
- **Redis configuration**: `maxmemory-policy noeviction` (already required for Redis)

**Check Redis version:**

```bash
redis-cli INFO server | grep redis_version
# Output should be: redis_version:6.0.0 or higher
```

**Check Redis eviction policy:**

```bash
redis-cli CONFIG GET maxmemory-policy
# Output should be: 1) "maxmemory-policy" 2) "noeviction"
```

## Migration Strategies

### Strategy 1: Simple Switchover (Recommended for Most)

**Best for**: Single client or coordinated maintenance window

**Steps:**

1. **Unmount existing JuiceFS mount:**

   ```bash
   juicefs umount /mnt/jfs
   ```

2. **Change scheme from `redis://` to `rueidis://`:**

   ```bash
   # Before
   juicefs mount -d "redis://192.168.1.6:6379/1" /mnt/jfs
   
   # After  
   juicefs mount -d "rueidis://192.168.1.6:6379/1" /mnt/jfs
   ```

3. **Verify mount is working:**

   ```bash
   ls /mnt/jfs
   juicefs status rueidis://192.168.1.6:6379/1
   ```

**That's it!** The metadata format is identical, so no data migration needed.

### Strategy 2: Gradual Migration (Recommended for Multi-Client)

**Best for**: Multiple JuiceFS clients, zero-downtime migration

**Steps:**

1. **Start with one test client:**

   ```bash
   # On test machine
   juicefs umount /mnt/jfs
   juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=10s" /mnt/jfs
   ```

   Note: Start with short TTL (`10s`) to minimize impact if issues occur.

2. **Monitor for 24-48 hours:**

   ```bash
   # Check Prometheus metrics
   curl http://localhost:9567/metrics | grep rueidis_cache
   
   # Verify tracking is active
   redis-cli CLIENT TRACKINGINFO
   ```

   Expected output:
   ```
   flags: on bcast
   prefixes: [jfs1c_i jfs1c_d jfs1c_c ...]
   ```

3. **Increase TTL if stable:**

   ```bash
   juicefs umount /mnt/jfs
   juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=1h" /mnt/jfs
   ```

4. **Migrate remaining clients one by one:**

   - Migrate 1-2 clients per day
   - Monitor metrics after each migration
   - Keep at least one `redis://` client as fallback

5. **Once all clients stable, increase TTL if desired:**

   ```bash
   # Optional: longer TTL for read-heavy workloads
   rueidis://192.168.1.6:6379/1?ttl=2h
   ```

### Strategy 3: Conservative Migration (Production Critical Systems)

**Best for**: High-availability production systems

**Steps:**

1. **Deploy monitoring first:**

   - Set up Prometheus scraping of JuiceFS metrics
   - Create dashboards for `rueidis_cache_hits_total` and `rueidis_cache_misses_total`
   - Set up alerts for cache misconfiguration

2. **Start with caching disabled:**

   ```bash
   # Rueidis client but no caching (same behavior as redis://)
   juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=0" /mnt/jfs
   ```

3. **Verify functionality:**

   - Run full test suite
   - Check all applications work correctly
   - Verify Rueidis client is stable

4. **Enable caching with conservative TTL:**

   ```bash
   juicefs umount /mnt/jfs
   juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=10s" /mnt/jfs
   ```

5. **Gradually increase TTL:**

   - Day 1-2: `ttl=10s`
   - Day 3-4: `ttl=30s`
   - Day 5-7: `ttl=5m`
   - Day 8-14: `ttl=30m`
   - Day 15+: `ttl=1h` (or higher based on workload)

6. **Monitor at each step:**

   - Redis server CPU, memory, network
   - JuiceFS client metrics (cache hit rate)
   - Application latency and throughput
   - CLIENT TRACKINGINFO output

## Configuration Examples

### Basic Migration

```bash
# Before (Redis)
export META_PASSWORD=mypassword
juicefs mount -d "redis://192.168.1.6:6379/1" /mnt/jfs

# After (Rueidis with default 1h TTL)
export META_PASSWORD=mypassword
juicefs mount -d "rueidis://192.168.1.6:6379/1" /mnt/jfs
```

### Custom TTL for Different Workloads

```bash
# Read-heavy workload (ML training, file serving)
rueidis://192.168.1.6:6379/1?ttl=2h

# Balanced workload (general purpose)
rueidis://192.168.1.6:6379/1?ttl=1h

# Write-heavy workload (active development, logs)
rueidis://192.168.1.6:6379/1?ttl=10s

# Testing/debugging (caching disabled)
rueidis://192.168.1.6:6379/1?ttl=0
```

### With TLS

```bash
# Before (Redis with TLS)
rediss://192.168.1.6:6379/1?tls-cert-file=/etc/certs/client.crt&tls-key-file=/etc/certs/client.key

# After (Rueidis with TLS and caching)
rueidiss://192.168.1.6:6379/1?ttl=1h&tls-cert-file=/etc/certs/client.crt&tls-key-file=/etc/certs/client.key
```

### Systemd Service Example

Update your systemd service file:

```ini
[Unit]
Description=JuiceFS
After=network.target

[Service]
Type=simple
User=juicefs
Environment="META_PASSWORD=mypassword"
ExecStart=/usr/local/bin/juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=1h" /mnt/jfs
ExecStop=/usr/local/bin/juicefs umount /mnt/jfs
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Then reload and restart:

```bash
sudo systemctl daemon-reload
sudo systemctl restart juicefs
sudo systemctl status juicefs
```

## Validation and Testing

### Verify Caching is Active

**1. Check Prometheus metrics:**

```bash
curl http://localhost:9567/metrics | grep rueidis_cache

# Expected output:
# rueidis_cache_hits_total 12458
# rueidis_cache_misses_total 0
```

If `cache_hits_total` is increasing → caching is working  
If `cache_misses_total` is high → check TTL configuration

**2. Check Redis tracking state:**

```bash
redis-cli CLIENT TRACKINGINFO

# Expected output:
# flags: on bcast
# redirect: -1
# prefixes:
#   - jfs1c_i
#   - jfs1c_d
#   - jfs1c_c
#   ...
```

**3. Test consistency across clients:**

```bash
# On client A - create a file
echo "test" > /mnt/jfs/testfile.txt

# On client B - verify it's visible immediately
cat /mnt/jfs/testfile.txt
# Should output: test

# On client A - delete the file
rm /mnt/jfs/testfile.txt

# On client B - verify it's gone immediately
ls /mnt/jfs/testfile.txt
# Should output: No such file or directory
```

This works even with long TTL (1h+) because Redis sends INVALIDATE messages.

### Performance Testing

**Benchmark metadata operations:**

```bash
# Before migration (redis://)
time ls -lR /mnt/jfs/large_directory > /dev/null

# After migration (rueidis://)
time ls -lR /mnt/jfs/large_directory > /dev/null
```

**Expected results**: 30-70% faster on second run (cache warmed up)

**JuiceFS bench command:**

```bash
# Before
juicefs bench /mnt/jfs --metadata

# After
juicefs bench /mnt/jfs --metadata

# Compare "Metadata Operations" section
```

## Monitoring

### Key Metrics to Track

**1. Prometheus Metrics:**

```promql
# Cache hit rate (should be high for stable workloads)
rate(rueidis_cache_hits_total[5m])

# Cache miss rate (should be low after warmup)
rate(rueidis_cache_misses_total[5m])

# Ratio of cached vs direct operations (should be ~100% if TTL>0)
rueidis_cache_hits_total / (rueidis_cache_hits_total + rueidis_cache_misses_total) * 100
```

**2. Redis Server Metrics:**

```bash
# Monitor Redis memory usage
redis-cli INFO memory | grep used_memory_human

# Monitor Redis network traffic (should decrease)
redis-cli INFO stats | grep instantaneous_ops_per_sec

# Monitor tracking table size
redis-cli CLIENT TRACKINGINFO | grep num-keys
```

**3. Application Metrics:**

- File operation latency (getattr, lookup, readdir)
- Overall application throughput
- Error rates (should be unchanged)

### Alert Thresholds

Set up alerts for:

```yaml
# Example Prometheus alerts
groups:
  - name: juicefs_rueidis
    rules:
      # Cache not working (misconfiguration)
      - alert: RueidisNotCaching
        expr: rate(rueidis_cache_hits_total[5m]) == 0 AND rate(rueidis_cache_misses_total[5m]) > 0
        for: 10m
        annotations:
          summary: "Rueidis caching not active (check TTL configuration)"
      
      # Redis tracking disabled
      - alert: RedisTrackingInactive
        expr: redis_tracking_enabled == 0
        for: 5m
        annotations:
          summary: "Redis CLIENT TRACKING not active (check Redis version >= 6.0)"
```

## Troubleshooting

### Issue: Cache not working (all misses)

**Symptoms**: `rueidis_cache_misses_total` increasing, `cache_hits_total` at zero

**Solution**:

```bash
# Check current mount
ps aux | grep juicefs | grep mount

# Verify TTL is set correctly
# WRONG: ?cache-ttl=1h (old parameter)
# RIGHT: ?ttl=1h (correct parameter)

# Remount with correct URI
juicefs umount /mnt/jfs
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=1h" /mnt/jfs
```

### Issue: Redis version too old

**Symptoms**: CLIENT TRACKINGINFO returns error

**Solution**:

```bash
# Check version
redis-cli INFO server | grep redis_version

# If < 6.0, upgrade Redis:
# Ubuntu/Debian
sudo apt install redis-server=6:7.0.0-1rl1~focal1

# Docker
docker stop redis
docker run -d --name redis -p 6379:6379 redis:7

# Then remount JuiceFS
```

### Issue: High Redis memory usage

**Symptoms**: Redis memory growing continuously

**Solution**:

```bash
# Reduce TTL to lower memory footprint
rueidis://192.168.1.6:6379/1?ttl=10s

# Monitor tracking table
redis-cli CLIENT TRACKINGINFO | grep num-keys

# If still high, consider switching to OPTIN mode (future feature)
```

### Issue: Stale data observed

**Symptoms**: Changes not visible on other clients

**Diagnosis**:

```bash
# Verify tracking is active
redis-cli CLIENT TRACKINGINFO

# Should show:
# flags: on bcast

# If empty, check Redis version
redis-cli INFO server | grep redis_version

# If version >= 6.0 but tracking not active, check JuiceFS logs
journalctl -u juicefs -n 100 | grep -i track
```

**Solution**: Usually indicates configuration issue, try:

```bash
# Restart with explicit tracking test
juicefs umount /mnt/jfs
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=1h" /mnt/jfs

# Test consistency immediately
# Client A: touch /mnt/jfs/test_$(date +%s)
# Client B: ls /mnt/jfs/ (should see new file)
```

## Rollback Plan

If issues occur, rollback is simple and safe:

### Immediate Rollback (Emergency)

```bash
# On all affected clients simultaneously
juicefs umount /mnt/jfs
juicefs mount -d "redis://192.168.1.6:6379/1" /mnt/jfs
```

**Impact**: Instant rollback, no data loss, metadata unchanged

### Disable Caching While Keeping Rueidis

```bash
# Alternative: keep Rueidis but disable caching
juicefs umount /mnt/jfs
juicefs mount -d "rueidis://192.168.1.6:6379/1?ttl=0" /mnt/jfs
```

**Benefit**: Isolate whether issue is with Rueidis client or caching specifically

### Rollback with Monitoring

```bash
# Gradually reduce TTL first
rueidis://192.168.1.6:6379/1?ttl=1s  # Very short TTL

# If still issues, switch to redis://
redis://192.168.1.6:6379/1

# Monitor for 24h, then retry rueidis with longer TTL
```

## Best Practices

### Pre-Migration

- [ ] Verify Redis version >= 6.0
- [ ] Check Redis `maxmemory-policy noeviction`
- [ ] Set up Prometheus monitoring
- [ ] Document current performance baseline
- [ ] Test rollback procedure in staging

### During Migration

- [ ] Start with one client
- [ ] Use short TTL initially (`?ttl=10s`)
- [ ] Monitor metrics continuously
- [ ] Test file create/delete consistency
- [ ] Verify application functionality

### Post-Migration

- [ ] Increase TTL gradually
- [ ] Monitor for at least 7 days
- [ ] Document final configuration
- [ ] Update runbooks with new URI format
- [ ] Train team on rollback procedure

## FAQ

### Q: Do I need to migrate metadata?

**A**: No. Rueidis and Redis use the same metadata format. Just change the URI scheme.

### Q: Can I mix Redis and Rueidis clients?

**A**: Yes, temporarily during migration. Both work with the same Redis server and metadata.

### Q: What happens if Redis crashes during migration?

**A**: Same as before—JuiceFS will retry connection. Cache is client-side, so Redis failure just means cache misses until Redis recovers.

### Q: How do I know what TTL to use?

**A**: Start with default `1h`. For read-heavy workloads, try `2h`. For write-heavy, try `10s` or `30m`. Monitor and adjust.

### Q: Can I disable caching after migration?

**A**: Yes, use `?ttl=0` or rollback to `redis://`.

### Q: What's the memory impact?

**A**: ~128MB per JuiceFS client by default (configurable). Redis memory may decrease due to fewer connections.

### Q: Does this work with Redis Sentinel/Cluster?

**A**: Yes, same URI format works. Rueidis supports both Sentinel and Cluster modes.

### Q: Can I use this with Redis 5.0?

**A**: No, CLIENT TRACKING requires Redis 6.0+. Upgrade Redis first.

## Conclusion

Migrating from `redis://` to `rueidis://` is straightforward and provides significant performance benefits. Start conservatively, monitor carefully, and gradually increase TTL based on your workload. Rollback is always available if needed.

**Key Takeaways:**

- Migration requires only changing URI scheme (`redis://` → `rueidis://`)
- No metadata changes needed
- Start with short TTL, increase gradually
- Monitor `rueidis_cache_hits_total` and CLIENT TRACKINGINFO
- Rollback takes seconds if issues occur

For support, see:
- [Rueidis Cache Monitoring Guide](./rueidis_cache_monitoring.md)
- [How to Set Up Metadata Engine](./how_to_set_up_metadata_engine.md)
- [JuiceFS GitHub Issues](https://github.com/juicedata/juicefs/issues)

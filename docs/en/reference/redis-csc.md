# Redis Client-Side Caching Support in JuiceFS

Starting with version 7.4, Redis provides [Client-Side Caching](https://redis.io/docs/latest/develop/reference/client-side-caching/) which allows clients to maintain local caches of data in a faster and more efficient way.

## How it works

Redis Client-Side Caching (CSC) works by:
1. The client enables tracking mode with `CLIENT TRACKING ON`
2. The client caches data locally after reading it from Redis
3. Redis notifies the client when cached keys are modified by any client
4. The client invalidates those keys in its local cache

This results in reduced network traffic, lower latency, and higher throughput.

## Configuration

JuiceFS supports Redis CSC through the following options in the metadata URL:

```
--meta-url="redis://localhost/1?client-cache=true"  # Enable client-side caching
--meta-url="redis://localhost/1?client-cache=true&client-cache-bcast=true"  # Use BCAST mode
--meta-url="redis://localhost/1?client-cache=true&client-cache-size=50000"  # Set cache size (default 100000)
--meta-url="redis://localhost/1?client-cache=true&client-cache-expire=3m"   # Set cache expiration (default 5m)
```

### Options

- `client-cache`: Enables client-side caching (set to any non-empty value except "false")
- `client-cache-bcast`: If present, uses broadcast mode to track all keys (better for workloads with many keys)
- `client-cache-size`: Maximum number of cached entries (default: 100000)
- `client-cache-expire`: Cache expiration time (default: 5 minutes)

## Modes

JuiceFS supports two CSC tracking modes:

1. **OPTIN mode (default)**: Only keys that are explicitly requested to be tracked are tracked.
2. **BCAST mode**: All keys accessed by the client are tracked.

BCAST mode is simpler but may generate more invalidation traffic. OPTIN mode is more selective but requires careful management of which keys to track.

## Requirements

- Redis server version 7.4 or higher
- JuiceFS with CSC support enabled

## Performance Considerations

1. Avoid setting cache too large to prevent excessive memory usage
2. For small to medium deployments, BCAST mode is easier to use
3. For very large deployments with many clients, consider OPTIN mode
4. CSC works best when data reads are much more frequent than writes

## Limitations

Redis CSC has some limitations to be aware of:

1. Large workloads with high write rates may result in many invalidation messages
2. Cluster mode has some additional overhead for tracking
3. When many clients connect to Redis with CSC enabled, memory usage on the Redis server increases

## Troubleshooting

To verify CSC is working properly:

1. Check JuiceFS logs for "Redis client-side caching enabled" messages
2. Monitor Redis CPU and memory usage
3. If performance degrades, consider adjusting cache size or disabling CSC

## References

- [Redis Client-Side Caching Documentation](https://redis.io/docs/latest/develop/reference/client-side-caching/)

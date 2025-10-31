# JuiceFS AI Coding Agent Instructions

## Project Overview

JuiceFS is a high-performance POSIX distributed file system that separates data (stored in object storage like S3) from metadata (stored in Redis/MySQL/TiKV/etc.). Files are split into Chunks → Slices → Blocks before storage.

**Key Architecture Components:**
- **Client** (`cmd/`, `pkg/vfs/`, `pkg/fuse/`): CLI commands and VFS implementation
- **Metadata Layer** (`pkg/meta/`): Pluggable backends (Redis, SQL, TiKV, etcd, Badger) implementing the `Meta` interface
- **Data Layer** (`pkg/chunk/`): Caching layer between VFS and object storage
- **Object Storage** (`pkg/object/`): 20+ object storage implementations (S3, OSS, Azure, local, etc.)
- **Gateway** (`pkg/gateway/`): S3-compatible gateway and WebDAV server

## Critical Development Patterns

### 1. Metadata Engine Abstraction

All metadata backends implement `pkg/meta/interface.go::Meta` interface. Each backend has two parts:
- Interface implementation (e.g., `pkg/meta/redis.go`, `pkg/meta/sql.go`)
- Internal engine interface (e.g., `pkg/meta/base.go::engine` interface methods like `doLoad()`, `doNewSession()`)

When adding metadata operations, update both the `Meta` interface and implement `do*` methods in each backend.

### 2. Context Pattern

Use `pkg/meta/Context` (NOT `context.Context` directly) for metadata operations. It wraps standard context with UID/GID/PID for permission checks:
```go
func (m *baseMeta) Operation(ctx Context, ...) syscall.Errno {
    if !ctx.CheckPermission() { ... }
}
```

Create contexts with `meta.Background()` or `meta.WrapContext(stdCtx)`.

### 3. Build Tags for Optional Features

JuiceFS uses build tags extensively to create minimal binaries:

**Common build targets:**
- `juicefs` - full binary (default)
- `juicefs.lite` - minimal binary excluding most object storage/metadata engines
- `juicefs.ceph` - with Ceph support (requires `ceph` tag)
- `juicefs.fdb` - with FoundationDB support (requires `fdb` tag)
- `juicefs.gluster` - with Gluster support (requires `gluster` tag)

When adding object storage backends, use build tags like `//go:build !noXXX` (see `pkg/object/s3.go` for examples).

### 4. Testing Structure

**Run tests using Makefile targets:**
```bash
make test.meta.core      # Core metadata tests (Redis/SQLite)
make test.meta.non-core  # External metadata engines (etcd/TiKV/Postgres)
make test.pkg            # All package tests except meta
make test.cmd            # Command-line tests (requires sudo for some)
```

Tests require external services (Redis, MySQL, MinIO, etc.). See `.github/workflows/unittests.yml` for the full setup with environment variables like `REDIS_ADDR=redis://127.0.0.1:6379/13`.

### 5. CLI Command Structure

All commands in `cmd/` follow this pattern:
1. Define with `func cmd<Name>() *cli.Command`
2. Register in `cmd/main.go::Main()`
3. Parse flags, create metadata/chunk configs
4. Call core logic in `pkg/` packages

Example flow for `mount`:
```
cmd/mount.go::cmdMount() 
  → getMetaConf() + getChunkConf() 
  → meta.NewClient(addr) 
  → vfs.NewVFS() 
  → fuse.Serve()
```

### 6. Data Flow: File Read/Write

**Write:** VFS → Chunk Store → (local cache) → Object Storage  
**Read:** VFS → Chunk Store → (check cache) → Object Storage → (populate cache)

The chunk store (`pkg/chunk/cached_store.go`) manages:
- Local disk cache with configurable eviction policies
- Async upload to object storage (controlled by `--writeback` flag)
- Block-level deduplication and compression

### 7. Object Storage Registration

Register new object storage in `pkg/object/<name>.go`:
1. Implement the `ObjectStorage` interface (Head, Get, Put, Delete, List, etc.)
2. Add creator function in `pkg/object/object_storage.go`
3. Update docs at `docs/en/reference/how_to_set_up_object_storage.md`

All object operations should support context cancellation and respect timeout settings.

### 8. Error Handling Convention

Metadata operations return `syscall.Errno` (e.g., `syscall.ENOENT`, `syscall.EACCES`).  
Object operations return standard Go `error`.  
VFS layer translates errors between these conventions.

### 9. Metrics and Observability

- Prometheus metrics exposed via `--metrics` flag (default: random port)
- Use `pkg/utils.GetLogger("component")` for structured logging
- Profiling available via `--debug` flag (enables pprof endpoints)

## Development Workflow

**Build:**
```bash
make juicefs              # Standard build
make juicefs.lite         # Minimal build
CGO_ENABLED=1 make ...    # Required for Ceph/Gluster/FoundationDB
```

**Before committing:**
1. Run `go fmt ./...` 
2. Install and run `pre-commit install` for static checks
3. Run relevant test suites (`make test.meta.core test.pkg`)
4. Update tests if adding new commands or modifying interfaces

**Cross-compilation:**
- Linux from macOS: `make juicefs.linux` (requires musl-cross)
- Windows from macOS: `make juicefs.exe` (requires mingw-w64)

## Common Gotchas

- **Don't modify metadata directly**: Always use the Meta interface methods which handle locking and transactions
- **Chunk size is fixed at 64MB**: `meta.ChunkSize = 1 << 26`, this is a core assumption throughout the codebase
- **Session management is critical**: The metadata layer maintains active sessions to detect stale mounts (see `pkg/meta/base.go::doRefreshSession`)
- **Cache coherence**: JuiceFS provides close-to-open consistency but not perfect coherence; understand the trade-offs in `docs/en/guide/cache.md`
- **Inode allocation is batched**: Inodes are allocated in batches of 1024 (`inodeBatch = 1 << 10`) to reduce metadata roundtrips

## Key Files to Reference

- `pkg/meta/interface.go` - Core metadata interface definition
- `pkg/meta/base.go` - Shared metadata implementation (3000+ lines, read selectively)
- `pkg/vfs/vfs.go` - VFS layer that ties everything together
- `cmd/mount.go` - Mount command showing full initialization flow
- `README.md` - Architecture diagrams and high-level concepts

## Additional Resources

- Official docs: https://juicefs.com/docs/community/
- Architecture guide: https://juicefs.com/docs/community/architecture
- Hadoop SDK: Separate Java implementation in `sdk/java/`

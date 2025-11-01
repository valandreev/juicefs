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
## JuiceFS — Agent instructions (concise)

Use this file when you're an automated coding agent working on JuiceFS. Stay focused, preserve repository conventions, and prefer small, verifiable edits.

- Big picture: JuiceFS separates metadata (Redis/MySQL/TiKV/etc.) from data (object storage). Main areas:
  - CLI & mount: `cmd/`, `pkg/fuse/`, `pkg/vfs/`
  - Metadata engines: `pkg/meta/` (implement `Meta` and engine `do*` methods)
  - Chunk/data layer: `pkg/chunk/` (cache, eviction, async upload)
  - Object backends: `pkg/object/` (implement `ObjectStorage`, register in `object_storage.go`)

- Critical conventions (use these exactly):
  - Metadata APIs use `pkg/meta/Context` and return `syscall.Errno`. Object code uses `error`.
  - Chunk size constant: `meta.ChunkSize = 1 << 26` (64MB). Inode batch: `inodeBatch = 1 << 10`.
  - Build tags control optional engines (`juicefs.lite`, `ceph`, `fdb`, `gluster`). Add `//go:build` tags as other files do.
  - Do NOT change metadata storage directly — use `Meta` methods for locking/transactions.

- Developer workflows (quick commands):
  - Build: `make juicefs` (or `make juicefs.lite`). For platform-specific builds see `Makefile`.
  - Tests: `make test.meta.core`, `make test.pkg`, `make test.cmd`. Many tests need external services (see `.github/workflows/unittests.yml`).
  - Format: `go fmt ./...`; pre-commit hooks expected (`pre-commit install`).

Coding style

- Follow the project's `CONTRIBUTING.md` coding style:
  - Follow "Effective Go" and "Go Code Review Comments" conventions.
  - Run `go fmt ./...` before committing code.
  - Every new source file must begin with the project's license header.
  - Install and use `pre-commit` and run `pre-commit install` to enable static checks.
  - Prefer small, well-scoped PRs with clear commit messages and unit tests for changed behavior.

- Example patterns to follow:
  - Adding an object backend: implement `ObjectStorage` in `pkg/object/<name>.go`, then add a creator in `pkg/object/object_storage.go`.
  - Metadata operation: update `pkg/meta/interface.go`, then implement `do*` in each backend (`pkg/meta/redis.go`, `pkg/meta/sql.go`, ...).

- Small contract for PRs you create:
  - Inputs: modified files and small unit tests. Outputs: passing `go test` for touched packages and `go fmt`ed code.
  - Error modes: return `syscall.Errno` in meta packages; propagate context cancellation for object ops.

- Helpful entry points when investigating behavior:
  - `cmd/mount.go` — mount lifecycle and args handling
  - `pkg/meta/base.go` — session management and shared meta helpers
  - `pkg/chunk/cached_store.go` — cache + writeback logic
  - `pkg/object/object_storage.go` — registration of backends

  Short call graphs (quick reference)

  - mount (high-level):
    1. `cmd/cmdMount()` (CLI flags parsing)
    2. `mount()` in `cmd/mount.go` — builds `meta.Config`, loads `meta.Format`, creates `chunk.Config` and `vfs.Config`
    3. `NewReloadableStorage()` → `createStorage()` for object backend
    4. `meta.NewClient()` and `metaCli.Load()` — metadata client initialization
    5. `chunk.NewCachedStore()` — create chunk store (local cache + writer/reader)
    6. `vfs.NewVFS()` — construct VFS with Meta + ChunkStore
    7. `mountMain()` / fuse serve loop — hand off to kernel-facing layer

  - vfs (common hot-paths):
    1. `NewVFS(conf, m, store, ...)` sets up reader, writer, cache filler
    2. Kernel FUSE ops → VFS methods (e.g., `Lookup`, `Open`, `Read`, `Write`, `Flush`)
    3. VFS delegates metadata calls to `v.Meta.*` (returns `syscall.Errno`) and data ops to `writer`/`reader` (chunk layer)
    4. Writer/Reader interact with `chunk.ChunkStore` to stage/upload blocks to object storage
    5. VFS maintains handles, invalidation and background flush/backup tasks (see `FlushAll`, `Backup` integration)

If anything here is ambiguous or you want the agent to adopt a different editing style (e.g., longer PR bodies, more tests), tell me and I'll iterate.

Requested review: is the level-of-detail appropriate and are there specific files or patterns you want called out further?

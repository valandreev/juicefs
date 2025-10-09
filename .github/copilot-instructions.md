<!--
Guidance for AI coding agents working on JuiceFS.
Keep this file concise and actionable. Reference key files and idioms used across the repo.
-->
# JuiceFS — Copilot instructions (short)

Target: help an AI coding agent be productive immediately when editing JuiceFS (Go).

- Big picture
  - JuiceFS is a POSIX-compatible filesystem client implemented in Go. The CLI binary is built from the repository root (see `cmd/` and `cmd/main.go`).
  - Data path: client splits files -> chunks -> slices -> blocks stored in Object Storage (see docs and `pkg/meta/slice.go`, `pkg/meta/chunk.go`).
  - Metadata path: metadata engines live under `pkg/meta/*` (examples: `redis.go`, `sql_*.go`, `tkv_*.go`). `pkg/meta/interface.go` defines the `Meta` interface — use it as the contract when changing engines.

- Build & test (concrete)
  - Build locally (default): run `make` or `go build -ldflags="$(LDFLAGS)" -o juicefs .` from repo root. `Makefile` contains cross-build targets (`juicefs.exe`, `juicefs.lite`, etc.).
  - Quick unit tests: `make test.pkg` (excludes `pkg/meta`) or run `go test ./...` for full run. See `Makefile` test targets for narrower runs: `test.meta.core`, `test.cmd`.
  - When adding features that change the public binary, update `pkg/version` info via `Makefile` LDFLAGS.
  - Windows build note: When building on Windows with WinFsp installed, set CGO_CFLAGS to the WinFsp headers before building. On Windows PowerShell / cmd the maintainers use this exact sequence which is known to work:

    set CGO_CFLAGS=-IC:/WinFsp/inc/fuse && go env -w CGO_CFLAGS=-IC:/WinFsp/inc/fuse && go build -ldflags="-s -w" -o juicefs.exe .

    This ensures the WinFsp fuse headers are found for cgo when compiling the Windows FUSE client.

- Project-specific conventions
  - Metadata engine implementations must satisfy the `Meta` interface from `pkg/meta/interface.go`. Look for `Init`, `Load`, `Reset`, session management, and background jobs.
  - Background jobs and long-running goroutines are common (e.g., sessions, cleanup). Respect `Config.NoBGJob` and heartbeat settings in `pkg/meta/config.go` when writing tests or background logic.
  - Use existing helper utilities: logging via `pkg/utils` (GetLogger/SetLogLevel), buffer helpers in `pkg/utils`, and global CLI setup in `cmd/main.go` (setup, removePassword).
  - Flags and command wiring live in `cmd/*.go` (each `cmdX()` returns a cli.Command). Add new subcommands in `cmd/main.go`'s Commands list.

- Patterns & examples (copy-paste anchors)
  - CLI wiring: `cmd/main.go` + files like `cmd/mount.go`, `cmd/config.go`. Follow `setup()` and `reorderOptions()` patterns for option handling and mount integration.
  - Meta contract: open `pkg/meta/interface.go` and mirror marshaling/Unmarshal patterns (e.g. `Attr.Marshal()` / `Unmarshal()` for binary layout compatibility).
  - Metadata config samples: `pkg/meta/metadata.sample` and `pkg/meta/metadata-sub.sample` show canonical keys and formats used by `Format` in `pkg/meta/config.go`.

- Integration points & externals
  - Object storage clients are used throughout `pkg/object` and storage-specific code (S3, GCS, Azure). See `go.mod` for supported SDKs (aws, gcs, azblob, minio, etc.).
  - Metadata stores include Redis, Rueidis (high-performance Redis client with client-side caching), SQL (MySQL/Postgres/SQLite), TiKV, Badger, FoundationDB; implementations live under `pkg/meta` (e.g., `redis.go`, `rueidis.go`, `sql_*.go`, `tkv_*.go`).
  - Tests sometimes require external services (Redis, MinIO). Refer to `Makefile` test targets and `integration/` scripts for integration test env setup.

- When editing code
  - Keep public on-disk/over-the-wire/binary formats stable: changes to `Attr`/inode/chunk layout require compatibility consideration and tests (see `Format.CheckVersion()` in `pkg/meta/config.go`).
  - Use existing unit tests as examples: many packages include focused tests (look for `_test.go` files under `pkg/` and `cmd/`). Prefer to add small, fast tests (`go test -run TestXxx -v`) and use `Makefile` targets for larger runs.
  - For concurrency and locking: inspect `redis_lock.go`, `tkv_lock.go`, and file-lock abstractions in `pkg/meta/*_lock.go` for expected semantics.

- Quick references (files/dirs to open first)
  - `cmd/main.go` — CLI entry, setup and global flags
  - `Makefile` — build/test targets and cross-build hints
  - `pkg/meta/interface.go` — metadata contract
  - `pkg/meta/config.go` — Format and config semantics
  - `pkg/meta/redis.go`, `pkg/meta/rueidis.go`, `pkg/meta/sql_*.go`, `pkg/meta/tkv_*.go` — engine implementations
  - `pkg/object` — object storage upload/download patterns
  - `docs/en/reference/how_to_set_up_metadata_engine.md` — operational notes for metadata engines

- Rueidis metadata engine specifics (for AI working on this implementation)
  - `pkg/meta/rueidis.go` — Rueidis metadata driver with automatic client-side caching (broadcast mode)
  - Default cache TTL: 2 weeks (effectively infinite with server-side invalidation)
  - Automatic broadcast tracking for all metadata key prefixes (inodes, dirs, chunks, xattrs, parents, symlinks)
  - Redis server sends invalidation notifications immediately on writes — no polling, no manual cache management
  - Build tag: `norueidis` to exclude from build (see `rueidis_noimpl.go`)
  - Tests: `rueidis_*_test.go` files verify caching behavior, transaction support, Lua scripts, locking
  - Architecture: Delegates to `baseClient` (shared Redis command interface) after connection setup

- Why this matters (developer intent)
  - JuiceFS separates fast metadata operations (in-memory or DB-backed) from large object storage. Changes that blur this separation (e.g., synchronous large-object ops in metadata paths) risk performance and must be benchmarked.

If anything here is unclear, tell me which area you'd like expanded (examples, tests, CI workflow, or specific engine). After your feedback I'll iterate.

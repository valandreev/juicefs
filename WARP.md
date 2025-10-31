# WARP.md

This file provides guidance to WARP (warp.dev) when working with code in this repository.

## Project Overview

JuiceFS is a high-performance POSIX file system designed for cloud-native environments. It separates data and metadata storage:
- **Data** is stored in object storage (S3, MinIO, local disk, etc.)
- **Metadata** is stored in various database engines (Redis, MySQL, PostgreSQL, TiKV, SQLite, etc.)
- **Client** coordinates between object storage and metadata engines

Written in Go (1.23.0+), JuiceFS has ~276 Go source files implementing a complete distributed file system with FUSE mount support.

## Common Commands

### Build
```bash
# Standard build
make juicefs

# Build with coverage support
make juicefs.cover

# Lite version (fewer storage backends)
make juicefs.lite

# Specialized builds
make juicefs.ceph    # With Ceph support
make juicefs.fdb     # With FoundationDB support
make juicefs.gluster # With GlusterFS support
```

### Testing

#### Unit Tests
```bash
# Core metadata tests (fast)
make test.meta.core

# Non-core metadata tests (Redis cluster, PostgreSQL, etcd)
make test.meta.non-core

# Package tests (excluding meta)
make test.pkg

# Command tests
make test.cmd

# FoundationDB tests
make test.fdb
```

**Note**: Tests require a `cover/` directory for coverage output. Unit tests are split into core and non-core to separate fast tests from those requiring external services.

#### Integration Tests
Integration tests are bash scripts in `integration/`:
```bash
cd integration
make s3test   # S3 Gateway tests
make webdav   # WebDAV tests
make ioctl    # ioctl functionality tests
```

### Linting
```bash
# Go linting (uses golangci-lint with minimal config)
golangci-lint run

# Markdown linting
npm run markdown-lint
npm run markdown-lint-fix

# Autocorrect for docs
npm run autocorrect-lint
npm run autocorrect-lint-fix

# Check for broken links
npm run check-broken-link
```

### Pre-commit Hooks
The project uses pre-commit hooks. Install with:
```bash
pre-commit install
```

## Architecture

### Core Package Structure

#### `pkg/meta/` - Metadata Management
The metadata layer is the heart of JuiceFS. Key concepts:
- **`engine` interface** (in `base.go`): Defines operations that all metadata engines must implement (Redis, SQL databases, TiKV, etc.)
- Each metadata engine (redis.go, sql.go, tkv.go) implements this interface
- Handles file/directory metadata, inodes, directory entries, attributes, and extended attributes
- Manages transactions for atomic operations
- Supports multiple backends through a unified interface

#### `pkg/object/` - Object Storage Abstraction
Object storage layer provides unified access to various storage backends:
- **`ObjectStorage` interface** (in `interface.go`): Common interface for all storage backends
- Individual implementations for 40+ storage services (S3, Azure, GCS, MinIO, local filesystem, etc.)
- Each file (s3.go, azure.go, file.go, etc.) implements the ObjectStorage interface
- Handles multipart uploads, encryption, checksums, and prefixing
- Files are split into Blocks (4 MiB default) and stored as objects

#### `pkg/chunk/` - Data Chunking & Caching
Manages data chunking and local caching:
- **`ChunkStore` interface**: Provides Reader/Writer for chunk access
- Chunks are 64 MiB max, composed of variable-length Slices
- Slices are composed of fixed-size Blocks (4 MiB default)
- Handles local caching, prefetching, and writeback strategies
- Bridges the gap between VFS operations and object storage

#### `pkg/vfs/` - Virtual File System
POSIX-compliant VFS implementation:
- Implements file system operations (open, read, write, stat, etc.)
- Coordinates between metadata and chunk layers
- Handles file handles, access logging, and backup operations
- Entry point for FUSE and other mount interfaces

#### `pkg/fuse/` - FUSE Integration
FUSE (Filesystem in Userspace) mounting:
- Translates FUSE kernel requests to VFS operations
- Platform-specific implementations (Unix vs Windows/WinFSP)

#### `cmd/` - CLI Commands
Each file in `cmd/` implements a JuiceFS subcommand:
- `mount.go` - Mount filesystem
- `format.go` - Format a new volume
- `bench.go` - Performance benchmarks
- `sync.go` - Data synchronization
- `dump.go`, `load.go` - Backup/restore metadata
- 50+ commands total

### Data Flow

**Write Path**: Application → VFS → Chunk (Writer) → Object Storage + Metadata Engine

**Read Path**: Application → VFS → Chunk (Reader with cache) → Object Storage / Local Cache + Metadata Engine

**Metadata Path**: VFS operations → Meta Engine (Redis/SQL/TiKV/etc.) for inode/directory/xattr operations

### Key Design Patterns

1. **Separation of Concerns**: Data and metadata are completely separate, enabling independent scaling
2. **Interface-Based Architecture**: `engine`, `ObjectStorage`, and `ChunkStore` interfaces allow pluggable backends
3. **Chunking Strategy**: Files are split hierarchically (Chunks → Slices → Blocks) for efficient storage and caching
4. **Transaction Support**: Metadata operations use database transactions for POSIX consistency guarantees
5. **Multi-Backend Support**: Same interface works with 40+ object storage services and 9+ metadata engines

## Development Guidelines

### Code Style
- Follow [Effective Go](https://go.dev/doc/effective_go) and [Go Code Review Comments](https://github.com/golang/go/wiki/CodeReviewComments)
- Run `go fmt` before committing
- Every new source file must begin with the Apache 2.0 license header
- Use pre-commit hooks for automatic checks

### Testing Requirements
- Add unit tests for all new functionality
- Tests should use the `cover/` directory for coverage output
- Integration tests go in `integration/` as shell scripts
- Test with multiple metadata engines when modifying metadata layer

### Metadata Engine Development
When adding or modifying metadata engines:
- Implement all methods of the `engine` interface in `pkg/meta/base.go`
- Consider transaction semantics and ACID properties
- Test with both core and non-core test suites
- Handle connection pooling and error recovery

### Object Storage Development  
When adding new object storage backends:
- Implement the `ObjectStorage` interface from `pkg/object/interface.go`
- Handle multipart upload limits and capabilities correctly
- Support encryption and checksum options if applicable
- Add backend-specific tests

### Build Tags
JuiceFS uses build tags to optionally exclude backends:
- Storage: `nogateway`, `nowebdav`, `nocos`, `nobos`, `nohdfs`, `nooss`, `noazure`, etc.
- Metadata: `nosqlite`, `nomysql`, `nopg`, `notikv`, `nobadger`, `noetcd`
- Special: `ceph`, `fdb`, `gluster` (require additional dependencies)

## Project Specifics

### Entry Point
`main.go` is minimal - it calls `cmd.Main(os.Args)` which dispatches to individual command implementations in `cmd/`

### Version Information
Build injects version info via ldflags:
- `github.com/juicedata/juicefs/pkg/version.revision`
- `github.com/juicedata/juicefs/pkg/version.revisionDate`

### Environment Variables for Testing
Unit tests expect these environment variables:
- `MINIO_ACCESS_KEY`, `MINIO_SECRET_KEY` - For MinIO tests
- `REDIS_ADDR` - Redis connection string
- `MYSQL_ADDR`, `MYSQL_USER` - MySQL connection
- `TIKV_ADDR`, `ETCD_ADDR` - KV store addresses
- `HDFS_ADDR`, `SFTP_HOST`, `CIFS_ADDR`, `NFS_ADDR` - Storage backend addresses

### Cross-Platform Considerations
- Unix-specific code: `*_unix.go`, `*_linux.go`, `*_darwin.go`
- Windows-specific code: `*_windows.go`
- Windows builds use WinFSP instead of FUSE

### Coverage Reporting
Coverage files go to `cover/cover.txt` and are uploaded to Codecov in CI.

## Contributing

1. Search GitHub issues before starting work
2. Discuss major changes in issues before implementation
3. Sign the CLA on first pull request
4. Include unit tests with PRs
5. Ensure commit messages are descriptive
6. Link PRs to related issues

Good first issues are labeled: `kind/good-first-issue` or `kind/help-wanted`

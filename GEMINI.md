## Project Overview

JuiceFS is a high-performance POSIX file system for the cloud. It's written in Go and designed to work with various object storage services and metadata engines.

**Key Technologies:**

*   **Language:** Go
*   **Metadata Engines:** Redis, MySQL, PostgreSQL, TiKV, etc.
*   **Data Storage:** Amazon S3, Google Cloud Storage, Azure Blob Storage, etc.
*   **Core Features:**
    *   POSIX compatible
    *   Hadoop compatible
    *   S3 compatible
    *   Kubernetes CSI Driver

## Building and Running

The project uses a `Makefile` for building and testing.

**Building the `juicefs` binary:**

```bash
make juicefs
```

**Running Tests:**

*   **Run all tests:**
    ```bash
    make test
    ```
*   **Run metadata tests:**
    ```bash
    make test.meta.core
    make test.meta.non-core
    ```
*   **Run package tests:**
    ```bash
    make test.pkg
    ```
*   **Run command tests:**
    ```bash
    make test.cmd
    ```

## Development Conventions

*   The project follows standard Go coding conventions.
*   Unit tests are located in the same directory as the source code with a `_test.go` suffix.
*   The project uses `golangci-lint` for linting, configured in `.golangci.yml`.
*   Pre-commit hooks are configured in `.pre-commit-config.yaml` to enforce code quality.
*   Contribution guidelines are available in `CONTRIBUTING.md`.

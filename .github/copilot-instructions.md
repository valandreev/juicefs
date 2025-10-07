This repository is JuiceFS (Go). Quick, actionable notes for an AI coding agent
working on this codebase.

High level
- JuiceFS is a single binary CLI (package `main`) providing a POSIX FUSE client
  and auxiliary commands under `cmd/` (mount, format, gc, fsck, etc.). Core
  libraries live under `pkg/` (meta, object, vfs, fs, chunk, usage, utils).

Build & test (examples)
- Local build: the Makefile builds the main binary: `make juicefs` (uses go build
  with version LDFLAGS). See `Makefile` for variants: `juicefs.lite`, `juicefs.exe`.
- Run unit tests: `make test.pkg` or specific targets in `Makefile` (CI runs
  `test.meta.core`, `test.meta.non-core`, `test.cmd`, `test.fdb`). Look at
  `.github/workflows/unittests.yml` for the CI matrix and environment.
- Integration tests mount and run long-running suites; CI builds and then:
  - `./juicefs format <meta-url> --bucket=...` then `sudo ./juicefs mount -d ...`
  See `.github/workflows/integrationtests.yml` and `.github/scripts/start_meta_engine.sh`.

Common patterns & conventions
- CLI wiring: `cmd/Main` constructs the urfave/cli app. Use `cmd/*` for
  subcommands; global flags are in `cmd/flags.go` (examples: `--no-usage-report`,
  `--no-agent`, logging flags). `setup()` enforces arg counts and log levels.
- Logging: use `utils.GetLogger("juicefs")`. Logger has a custom formatter
  in `pkg/utils/logger.go`. Use `utils.SetLogLevel(...)` and `--no-color` opt.
- Usage reporting: anonymous periodic reports live in `pkg/usage/usage.go` and
  can be disabled with `--no-usage-report`.
- Object/meta engines: storage backends live in `pkg/object/*`; meta engines are
  under `pkg/meta`. Many implementations use external SDKs (S3, Azure, TiKV,
  Redis, MySQL). Inspect `go.mod` for important dependencies and supported
  storage drivers.

Integration & CI notes
- CI builds and tests on Ubuntu runners. Tests require services (redis, etcd,
  mysql, minio). See `.github/workflows/*.yml` for exact env vars and setup
  scripts. For reproducing CI locally, follow those scripts (e.g. start services
  and run `make test.meta.core`).

Files to reference when making edits
- Command wiring & flags: `cmd/main.go`, `cmd/flags.go` and `cmd/*` commands.
- Core logic: `pkg/meta/`, `pkg/object/`, `pkg/vfs/`, `pkg/fs/`, `pkg/chunk/`.
- Utilities: `pkg/utils/` (logger, progress, helpers), `pkg/usage/usage.go`.
- Build/test automation: `Makefile`, `.github/workflows/unittests.yml`,
  `.github/workflows/integrationtests.yml`.

When changing public behaviour
- Update `Makefile` build flags when adding versioned ldflags. Add/adjust CI
  workflow steps if new services are required. Run relevant `make` test targets
  and ensure `GOCOVERDIR` instrumentation is preserved for CI coverage uploads.

If anything above is unclear or you want more detail (examples of CLI
subcommands, test scripts, or where to add new storage drivers), tell me which
area to expand.

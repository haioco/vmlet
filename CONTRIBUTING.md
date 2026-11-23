# Contributing to vmlet

Thanks for your interest in improving vmlet! This document explains how to set
up a development environment, the conventions we follow, and how to get your
changes merged.

vmlet is maintained by [HaioCloud](https://haio.ir), the first Iranian cloud
provider (hourly, pay-as-you-go services since 2016).

## Code of conduct

Be respectful and constructive. We want a welcoming community for everyone.

## Getting started

### Prerequisites

- Go **1.22+**
- A Linux machine with **KVM** (`/dev/kvm`) for integration testing
- A **Firecracker** binary for end-to-end VM tests
- Docker (only if you work on image builders)

### Build & test

```bash
git clone https://github.com/haioco/vmlet.git
cd vmlet

go build ./...        # must pass
go vet ./...          # must be clean for packages you touch
go test ./...         # see known issues below
```

### Known test issues (pre-existing)

A handful of legacy test files reference older internal APIs and currently fail
to build or fail assertions. These are **not** caused by recent changes and are
tracked for cleanup:

- `internal/firecracker/enhanced_lifecycle_test.go` — stale `NewManager` /
  `ValidationResult` signatures.
- `internal/lifecycle/manager_test.go` — `NewManager` arity mismatch.
- `internal/monitoring` — alert tests and a racy `TestLoggerConcurrency`
  (unsynchronized shared `bytes.Buffer` across goroutines).
- `internal/resource` — `ExampleResourceService`.

If you fix any of these, that's a great first PR. Please don't let them block
unrelated work; verify the packages you changed build and test cleanly.

## Development workflow

1. **Fork** the repository and create a topic branch:
   `git checkout -b feat/my-change`.
2. Make focused changes. Keep commits small and descriptive.
3. Run `gofmt -w` on changed files, then `go build ./...` and `go vet` on the
   packages you touched.
4. Add or update tests for behavior changes.
5. Update relevant docs in `docs/` and the README if you change public behavior.
6. Open a **pull request** against `main` with a clear description of the what
   and why. Link any related issue.

## Coding standards

- **Formatting:** `gofmt` (no exceptions). Run `go vet` before submitting.
- **Naming:** follow existing package conventions; exported identifiers get doc
  comments.
- **Errors:** wrap with context (`fmt.Errorf("...: %w", err)`); don't swallow
  errors silently.
- **Concurrency:** guard shared state with mutexes or channels; avoid data races
  (run `go test -race` on the packages you touch where practical).
- **No new heavy dependencies** without discussion — the control plane aims to
  stay lightweight and self-contained.
- **Secrets:** never commit credentials, tokens, or host-specific paths. Use
  placeholders like `CHANGE_ME_*` and `*.example.com`.

## Project layout

See the [README repository layout](README.md#-repository-layout) and
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for how subsystems fit together.
Most feature work lands in `internal/<subsystem>` plus a handler in
`cmd/http-server/main.go`.

## Reporting bugs & requesting features

Open a GitHub issue with:

- What you expected vs. what happened.
- Steps to reproduce (commands, config, environment).
- Relevant logs (`vmlet` logs to stdout; `/metrics` and `/health` help too).

## License

By contributing, you agree that your contributions will be licensed under the
[MIT License](LICENSE).

# Contributing to go-atlas-net

Thank you for your interest in contributing to go-atlas-net! This guide will help you get started.

## Prerequisites

- **Go 1.27+** — [download](https://go.dev/dl/)
- **Make** — for running project targets
- **Git** — for version control

## Getting Started

1. **Fork** the repository on GitHub.

2. **Clone** your fork:

   ```bash
   git clone https://github.com/<your-username>/go-atlas-net.git
   cd go-atlas-net
   ```

3. **Create a branch** from `develop`:

   ```bash
   git checkout -b feat/my-feature develop
   ```

4. **Make your changes**, then run checks:

   ```bash
   make fmt
   make lint
   make test-all
   ```

5. **Commit** using [Conventional Commits](#commit-messages) and push:

   ```bash
   git push origin feat/my-feature
   ```

6. **Open a Pull Request** against the `develop` branch.

## Branch Naming

Use the following prefixes:

| Prefix | Purpose |
|--------|---------|
| `feat/` | New features |
| `fix/` | Bug fixes |
| `docs/` | Documentation changes |
| `refactor/` | Code refactoring |

## Commit Messages

We follow [Conventional Commits](https://www.conventionalcommits.org/):

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

**Types:** `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `ci`, `perf`, `build`, `style`

**Scope** must match one of the allowed scopes in [`commit_scopes.txt`](commit_scopes.txt). The scope represents the package or module affected by the change.

Examples:

```
feat(providers/anthropic): support structured outputs
fix(retry): stop retrying once the context is canceled
docs(README): add quick start examples
test(schema): add benchmark for schema derivation
build(deps): bump golang.org/x/sys
```

### Automated Validation

- **GitHub Actions** automatically validates all commit messages and PR titles
- **Local validation** can be enabled with: `git config core.hooksPath .githooks`
- Run `.github/scripts/validate-commits.sh` to check your commits before pushing

## Code Style

### Formatting

- Run `make fmt` before committing — this runs `gci` with the project's import ordering.
- Maximum line length: **156 characters** (enforced by `lll` linter).

### Import Ordering

Imports must follow this order (enforced by `gci` via `.golangci.yml`):

1. Standard library
2. Third-party packages (default)
3. `google.golang.org`
4. `golang.org`
5. `github.com/altessa-s/go-atlas-core`
6. `github.com/altessa-s/go-atlas-net`
7. `github.com/altessa-s` (other altessa packages)
8. Blank imports
9. Aliases

### Copyright Headers

All `.go` files must include the copyright header. Run `make copyright` to add it automatically.

```go
// Copyright 2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.
```

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make fmt` | Format code and sort imports |
| `make lint` | Run golangci-lint with the repo config (`.golangci.yml`) |
| `make test` | Run all tests |
| `make test-verbose` | Run tests with verbose output |
| `make test-race` | Run tests with race detector |
| `make test-shuffle` | Run tests with randomized order |
| `make test-coverage` | Run tests with coverage report |
| `make test-all` | Run tests with race, shuffle, and coverage |
| `make test-coverage-func` | Per-package coverage breakdown |
| `make bench` | Run all benchmarks |
| `make bench-count` | Run benchmarks for benchstat |
| `make fuzz-list` | List every fuzz target |
| `make fuzz-seed` | Run fuzz targets over their seed corpus only (no mutation) |
| `make fuzz` | Fuzz every target for `FUZZTIME` each (`FUZZTIME=30s FUZZPKG=./...`) |
| `make dupl` | Find code duplication |
| `make dupl-check` | Fail on code duplication |
| `make copyright` | Add copyright headers |
| `make generate` | Run go generate |
| `make tidy` | Run go mod tidy |
| `make security-scan` | Run govulncheck and gosec |

## Fuzzing

`go test ./...` already executes every fuzz target over its `f.Add` seeds, so seeds act as ordinary regression cases and a PR needs nothing extra.
Mutation is separate: `make fuzz` walks each target for `FUZZTIME` (`go test -fuzz` accepts one target per package per invocation), and the `Fuzz`
workflow does the same nightly with a larger budget, restoring the generated corpus between runs so coverage compounds.

Write a fuzz target when the input is attacker-controlled or there is a property worth stating — a round trip, an allowlist that must hold, an
invariant the code documents. "Does not panic" is the floor, not the goal: a target with no assertion passes for the wrong reasons. When a run
fails, Go writes the minimized reproducer to the package's `testdata/fuzz/<Target>/`; commit it, so the case is pinned forever in the seed corpus.

## Pull Request Process

1. Ensure `make lint` and `make test-all` pass.
2. Write conventional commit messages: release notes are generated from them by goreleaser at release time, there is no
   manually maintained `CHANGELOG.md`.
3. Fill out the PR template completely.
4. One approving review is required before merging.

## Reporting Issues

- **Bugs**: Use the [Bug Report](https://github.com/altessa-s/go-atlas-net/issues/new?template=bug_report.yml) template.
- **Features**: Use the [Feature Request](https://github.com/altessa-s/go-atlas-net/issues/new?template=feature_request.yml) template.
- **Security**: See [SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).

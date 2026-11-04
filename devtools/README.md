# devtools

Module `github.com/altessa-s/go-atlas-net/devtools` pins the development tools of go-atlas-net separately, so their dependencies stay out of the main
`go.mod`. No code imports it. Every pinned tool is built from here and run from the built binary, never from `PATH`, so a different
version installed there never takes part: the make targets in the table build theirs into `bin/`, and `bench-compare` (through
`.github/scripts/bench-regression.sh`) builds benchstat into a temporary directory that it removes afterwards. Targets that run only `go`
itself (`test`, `vet`, `build`, ...) use no pinned tool.

## Pinned tools

| Tool            | Package                                                  | Make targets                          |
|-----------------|----------------------------------------------------------|---------------------------------------|
| golangci-lint   | `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` | `fmt`, `lint`, `golangci`, `generate` |
| optgen          | `github.com/altessa-s/go-optgen/cmd/optgen`              | `generate`, `generate-check`          |
| gosec           | `github.com/securego/gosec/v2/cmd/gosec`                 | `gosec`, `security-scan`              |
| govulncheck     | `golang.org/x/vuln/cmd/govulncheck`                      | `govulncheck`, `security-scan`        |
| dupl            | `github.com/mibk/dupl`                                   | `dupl`, `dupl-check`                  |
| benchstat       | `golang.org/x/perf/cmd/benchstat`                        | `bench-compare` (temporary directory) |
| copyright check | `github.com/altessa-s/go-copyright-checker`              | `copyright`                           |

`make fmt` and `make generate` format with golangci-lint's gci formatter, configured in `.golangci.yml`; it knows the standard library of
the Go toolchain it is built with (Go 1.27's `uuid`, for one), which the standalone gci does not.

## Layout

| File       | Purpose                                                                                                   |
|------------|-----------------------------------------------------------------------------------------------------------|
| `doc.go`   | Package documentation; the only file built without tags                                                   |
| `tools.go` | Blank imports of the tool commands under the `tools` build tag, so `go mod tidy` keeps their requirements |
| `go.mod`   | The pinned versions                                                                                       |

The module has no behavior a test could assert (blank imports only), so it has no `*_test.go` or `*_bench_test.go` files.

## Usage

```sh
# What make golangci does: build the pinned linter into bin/ and run it from there.
go -C devtools build -o "$PWD/bin/golangci-lint" github.com/golangci/golangci-lint/v2/cmd/golangci-lint
bin/golangci-lint run ./...
```

To upgrade a tool, run `go get <package>@<version>` and `go mod tidy` in this directory.

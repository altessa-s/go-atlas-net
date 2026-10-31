# devtools

Module `github.com/altessa-s/go-atlas-net/devtools` pins the development tools of go-atlas-net separately, so their dependencies stay out of the main
`go.mod`. No code imports it. The Makefile builds or installs each tool from here before running it.

## Pinned tools

| Tool            | Package                                                  | Make targets                  |
|-----------------|----------------------------------------------------------|-------------------------------|
| golangci-lint   | `github.com/golangci/golangci-lint/v2/cmd/golangci-lint` | `lint`                        |
| optgen          | `github.com/altessa-s/go-optgen/cmd/optgen`              | `generate` (`bin/optgen`)     |
| gci             | `github.com/daixiang0/gci`                               | `fmt`, `generate` (`bin/gci`) |
| gosec           | `github.com/securego/gosec/v2/cmd/gosec`                 | `gosec`, `security-scan`      |
| govulncheck     | `golang.org/x/vuln/cmd/govulncheck`                      | `security-scan`               |
| dupl            | `github.com/mibk/dupl`                                   | `dupl`, `dupl-check`          |
| benchstat       | `golang.org/x/perf/cmd/benchstat`                        | comparing `bench-count` runs  |
| copyright check | `github.com/altessa-s/go-copyright-checker`              | `copyright`                   |

## Layout

| File       | Purpose                                                                                                   |
|------------|-----------------------------------------------------------------------------------------------------------|
| `doc.go`   | Package documentation; the only file built without tags                                                   |
| `tools.go` | Blank imports of the tool commands under the `tools` build tag, so `go mod tidy` keeps their requirements |
| `go.mod`   | The pinned versions                                                                                       |

The module has no behavior a test could assert (blank imports only), so it has no `*_test.go` or `*_bench_test.go` files.

## Usage

```sh
# make generate and make fmt build their tools into bin/ and run them from there.
go -C devtools build -o "$PWD/bin/gci" github.com/daixiang0/gci

# The other targets install their tool from this module.
go -C devtools install github.com/golangci/golangci-lint/v2/cmd/golangci-lint
golangci-lint run ./...
```

To upgrade a tool, run `go get <package>@<version>` and `go mod tidy` in this directory.

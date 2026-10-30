# go-atlas-net

[![CI](https://github.com/altessa-s/go-atlas-net/actions/workflows/ci.yml/badge.svg?branch=develop)](https://github.com/altessa-s/go-atlas-net/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/altessa-s/go-atlas-net.svg)](https://pkg.go.dev/github.com/altessa-s/go-atlas-net)
[![Go Report Card](https://goreportcard.com/badge/github.com/altessa-s/go-atlas-net)](https://goreportcard.com/report/github.com/altessa-s/go-atlas-net)
[![Release](https://img.shields.io/github/v/release/altessa-s/go-atlas-net)](https://github.com/altessa-s/go-atlas-net/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/altessa-s/go-atlas-net)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

```go
import "github.com/altessa-s/go-atlas-net/<package>"
```

Network primitives and the outbound HTTP client of the [go-atlas](https://github.com/altessa-s/go-atlas) toolkit: egress rules, HTTP helpers, base
URLs, forward-proxy dialers, IP helpers, real client IP extraction, the incoming gRPC metadata getter and a resilient HTTP client with retries,
circuit breaker, SSRF guard and egress policy.

Each package can be imported independently without pulling in the rest of the module. The packages read no configuration schema and report no
metrics themselves: the HTTP client publishes events through its `Observer`, and go-atlas maps them to metrics and health
(`net/http/client/observe`) and configuration to options (`net/http/client/factory`, `net/proxydial/factory`).

## Requirements

Go 1.27 or later (`go 1.27.0` in `go.mod`). The module uses Go 1.27 APIs — `url.URL.Clone`, `strings.CutLast` — so earlier toolchains cannot
build it.

## Packages

| Package                          | Description                                                                                      |
|----------------------------------|--------------------------------------------------------------------------------------------------|
| [clientip](./clientip)           | Real client IP behind trusted proxies, for HTTP and gRPC                                         |
| [egressrule](./egressrule)       | Egress allow/deny rule grammar shared by go-atlas `httpconfig` and the HTTP client egress engine |
| [grpc/metadata](./grpc/metadata) | Adapts incoming gRPC metadata to the shared header getter                                        |
| [http/client](./http/client)     | Resilient outbound HTTP client: retries, circuit breaker, SSRF guard, egress policy, proxies     |
| [httpx](./httpx)                 | `RoundTripperFunc`, `Doer`, `IsSuccess` / `IsRedirect`, shared header names, `HTTPHeaderGetter`  |
| [netipx](./netipx)               | `IsPrivate`, `IsTrusted`, `InList`, `ParsePrefixes`                                              |
| [proxydial](./proxydial)         | Forward-proxy dialers (HTTP CONNECT, SOCKS5)                                                     |
| [urlx](./urlx)                   | `ParseBaseURL`, `JoinPath`, `Canonical` for service base URLs                                    |

## Design principles

- **Minimal dependencies** — the Go standard library, [go-atlas-core](https://github.com/altessa-s/go-atlas-core) and `golang.org/x/net`, plus
  package-scoped exceptions: gRPC (`grpc/metadata`), gobreaker and `golang.org/x/time` (`http/client`), `hashicorp/golang-lru` (`clientip`).
- **No dependency on go-atlas** — go-atlas depends on this module; observability and configuration adapters live there.
- **Transport-agnostic core** — `clientip`, `netipx` and `egressrule` take consumer-side interfaces and import no transport framework; `httpx`
  and `grpc/metadata` adapt HTTP and gRPC to them.
- **Safe by default** — the HTTP client blocks private destinations after DNS resolution, redacts URL secrets from errors and replays only
  idempotent requests; `WithMaxResponseSize` bounds response bodies.
- **Thread-safe by default** — all exported functions and types are safe for concurrent use unless documented otherwise.

## Development

| Command         | Description                                          |
|-----------------|------------------------------------------------------|
| `make test`     | Run all tests                                        |
| `make lint`     | golangci-lint with the repo config (`.golangci.yml`) |
| `make generate` | Install `optgen` and run go generate                 |
| `make bench`    | Run all benchmarks                                   |

`make generate` installs `optgen` from a sibling [go-optgen](https://github.com/altessa-s/go-optgen) checkout (`OPTGEN_DIR`, default `../go-optgen`).

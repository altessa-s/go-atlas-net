# go-atlas-net

```go
import "github.com/altessa-s/go-atlas-net/<package>"
```

Network primitives and the outbound HTTP client of the [go-atlas](https://github.com/altessa-s/go-atlas) toolkit, usable on their own. The packages
depend on the standard library, [go-atlas-core](https://github.com/altessa-s/go-atlas-core), `golang.org/x/net` and, per package, gRPC
(`grpc/metadata`), gobreaker and `golang.org/x/time` (`http/client`) and `hashicorp/golang-lru` (`clientip`). They read no configuration schema and
report no metrics themselves: the HTTP client publishes events through its `Observer`, and go-atlas maps them to metrics and health
(`net/http/client/observe`) and configuration to options (`net/http/client/factory`, `net/proxydial/factory`).

## Packages

| Package                          | Description                                                                                     |
|----------------------------------|-------------------------------------------------------------------------------------------------|
| [egressrule](./egressrule)       | Egress allow/deny rule grammar shared by go-atlas `httpconfig` and the HTTP client egress engine |
| [httpx](./httpx)                 | `RoundTripperFunc`, `Doer`, `IsSuccess` / `IsRedirect`, shared header names, `HTTPHeaderGetter` |
| [urlx](./urlx)                   | `ParseBaseURL`, `JoinPath`, `Canonical` for service base URLs                                   |
| [proxydial](./proxydial)         | Forward-proxy dialers (HTTP CONNECT, SOCKS5)                                                    |
| [netipx](./netipx)               | `IsPrivate`, `IsTrusted`, `InList`, `ParsePrefixes`                                             |
| [clientip](./clientip)           | Real client IP behind trusted proxies, for HTTP and gRPC                                        |
| [http/client](./http/client)     | Resilient outbound HTTP client: retries, circuit breaker, SSRF guard, egress policy, proxies    |
| [grpc/metadata](./grpc/metadata) | Adapts incoming gRPC metadata to the shared header getter                                       |

## Development

| Command         | Description                                                |
|-----------------|------------------------------------------------------------|
| `make test`     | Run all tests                                              |
| `make lint`     | golangci-lint with the repo config (`.golangci.yml`)       |
| `make generate` | Install `optgen` from `OPTGEN_DIR` and run go generate     |
| `make bench`    | Run all benchmarks                                         |

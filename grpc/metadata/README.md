# metadata

```go
import "github.com/altessa-s/go-atlas-net/grpc/metadata"
```

Provider-side adapter over incoming gRPC metadata. It satisfies consumer-side header interfaces implicitly (`GetHeader` for multi-value getters such
as [`clientip.HeaderGetter`](../../clientip), `GetSingleHeader` for single-value getters), so protocol-neutral packages read gRPC headers the same way
they read HTTP ones. HTTP counterpart: [`httpx.HTTPHeaderGetter`](../../httpx).

## Key types

| Symbol                 | Description                                                                 |
|------------------------|-----------------------------------------------------------------------------|
| `HeaderGetter`         | Reads header values from the incoming metadata of a context; nil-safe       |
| `NewHeaderGetter(ctx)` | Returns a `HeaderGetter` over `ctx`                                         |
| `GetHeader(name)`      | All values of `name`                                                        |
| `GetSingleHeader(name)`| The first value of `name`, or `""`                                          |

## Placement

This is the only package of the module that imports a transport framework (`google.golang.org/grpc`). It lives here, next to its HTTP counterpart,
so that `clientip` and other protocol-neutral consumers never import gRPC themselves: a module that does not import this package does not link gRPC.
Server-side gRPC wiring (interceptors, health, reflection) belongs to go-atlas, not here.

# metadata

```go
import "github.com/altessa-s/go-atlas-net/grpc/metadata"
```

Adapts incoming gRPC metadata to the header-access interface shared across transports, so protocol-neutral packages such as
[`net/clientip`](../../clientip) read gRPC headers the same way they read HTTP ones. HTTP counterpart: [`httpx.HTTPHeaderGetter`](../../httpx).

## Key types

| Symbol                 | Description                                                                 |
|------------------------|-----------------------------------------------------------------------------|
| `HeaderGetter`         | Reads header values from the incoming metadata of a context; nil-safe       |
| `NewHeaderGetter(ctx)` | Returns a `HeaderGetter` over `ctx`                                         |
| `GetHeader(name)`      | All values of `name`                                                        |
| `GetSingleHeader(name)`| The first value of `name`, or `""`                                          |

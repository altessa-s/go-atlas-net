# httpx

```go
import "github.com/altessa-s/go-atlas-net/httpx"
```

Package `httpx` holds small `net/http` extensions shared across the module: function adapters (`Doer`, `DoerFunc`, `RoundTripperFunc`), header
name constants, status classes and the `*http.Request` header adapter (`HTTPHeaderGetter`). It holds no client, IP or policy logic: the outbound client
lives in [`http/client`](../http/client), client IP resolution in [`clientip`](../clientip) and address policy in [`netipx`](../netipx) and
[`egressrule`](../egressrule).

## Key types

| Type               | Description                                                                                                       |
|--------------------|-------------------------------------------------------------------------------------------------------------------|
| `RoundTripperFunc` | Adapter that lets ordinary functions implement `http.RoundTripper` — middleware, transport decoration, test stubs |
| `Doer`             | `Do(*http.Request) (*http.Response, error)`; satisfied by `*http.Client`, accepted by code that only sends        |
| `DoerFunc`         | Adapter that lets ordinary functions implement `Doer` — typically a test double                                   |

## Functions

| Function     | Description                                       |
|--------------|---------------------------------------------------|
| `IsSuccess`  | Reports whether a status code is in the 2xx class |
| `IsRedirect` | Reports whether a status code is in the 3xx class |

## Usage

```go
func fetch(ctx context.Context, doer httpx.Doer, target string) error {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
    if err != nil {
        return err
    }
    resp, err := doer.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    if !httpx.IsSuccess(resp.StatusCode) {
        return fmt.Errorf("unexpected status %d", resp.StatusCode)
    }
    return nil
}

// In a test:
doer := httpx.DoerFunc(func(*http.Request) (*http.Response, error) {
    return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
})
```

## Header names

| Symbol                                                      | Description                                                                   |
|-------------------------------------------------------------|-------------------------------------------------------------------------------|
| `ContentType`, `ContentLength`, `Accept`                    | Interned header names shared by clients and servers                           |
| `RateLimitLimit`, `RateLimitRemaining`, `RateLimitReset`    | Interned rate-limit response headers                                          |
| `RetryAfter`                                                | Interned `Retry-After` header name                                            |
| `Header*` constants                                         | Raw (non-interned) values of the same names                                   |
| `HTTPHeaderGetter` / `NewHTTPHeaderGetter`                  | Adapts an `*http.Request` to header getters such as `clientip.HeaderGetter`   |

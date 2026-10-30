# hostpacer

```go
import "github.com/altessa-s/go-atlas-net/http/client/limiters/hostpacer"
```

Package `hostpacer` provides a waiting per-host `limiters.RequestsLimiter`: every host gets a token bucket refilled once per interval up to a burst,
and `Allow` blocks until the host's next slot instead of rejecting the request, so a crawl fan-out over one host is spaced rather than failed.

## Key types

| Type / Function  | Description                                                                                               |
|------------------|-----------------------------------------------------------------------------------------------------------|
| `Pacer`          | Waiting per-key limiter; `Allow(ctx, key)`, `Len()`                                                         |
| `New`            | `New(interval, burst, opts...)`; a non-positive interval or burst returns `ErrInvalidRate`                 |
| `ErrInvalidRate` | Sentinel for an invalid interval or burst                                                                  |

## Options

| Option         | Default                  | Description                                                                                            |
|----------------|--------------------------|--------------------------------------------------------------------------------------------------------|
| `WithMaxHosts` | `DefaultMaxHosts` (1024) | Soft bound on tracked hosts; reaching it makes a new host first drop the fully refilled buckets        |
| `WithClock`    | `clock.Real()`           | go-atlas-core `clock.Clock` bucket levels are read at and slot waits run on (`clock.NewFake` in tests) |

## Semantics

- `Allow` returns `ctx.Err()` when the wait is cut short, and an error wrapping `context.DeadlineExceeded` without waiting when the next slot lies
  beyond the context deadline.
- The bucket lookup and the slot reservation happen under one lock, then the wait runs outside it; a wait cut short releases its slot.
- Buckets live in memory and are created on first use. Once `WithMaxHosts` hosts are tracked, a new host first drops every bucket that has refilled
  completely — lossless, a new bucket starts full. Buckets still refilling are never dropped, so `WithMaxHosts` is a soft bound: the pacer can exceed
  it by the hosts used within one burst × interval window, which also bounds memory. LRU eviction is deliberately not used: dropping a bucket that is
  still refilling would give its host a fresh full bucket and let it exceed its pace. Pacing is per process: replicas do not share slots.

## Usage

Pair it with a request budget through [`limiters.Chain`](../README.md) (the budget first, so a refused request takes no slot) and pace every retry
with `client.WithLimiterPerAttempt`:

```go
pacer, err := hostpacer.New(time.Second, 1)
if err != nil {
    return err
}
c := client.New(
    client.WithLimiter(limiters.Chain(budget, pacer)),
    client.WithLimiterPerAttempt(),
)
```

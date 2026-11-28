# dnslookup

```go
import "github.com/altessa-s/go-atlas-net/dnslookup"
```

Look up DNS MX records for a validated host name. Queries use absolute names and reject [IANA special-use domains][iana] and their subdomains before
calling the resolver. The built-in registry snapshot was retrieved on 2026-11-28.

## Usage

```go
lookup := dnslookup.New(
	dnslookup.WithMaxTimeout(3*time.Second),
	dnslookup.WithMaxConcurrent(8),
)
records, err := lookup.LookupMX(ctx, "contoso.com")
if err != nil {
	return err
}
for _, mx := range records {
	fmt.Println(mx.Pref, mx.Host)
}
```

`LookupMX` uses [egressrule.NormalizeHost](../egressrule) for the query name and passes it to the resolver with a trailing dot. It returns all MX
records in resolver order with their original preferences. Ordinary exchange names are normalized to lowercase without a trailing dot. The root
exchange `.` keeps its original preference, including in mixed or partial answers. Callers interpret null MX records and apply any sorting or limits.

## Options

| Option              | Default               | Behavior                                                               |
|---------------------|-----------------------|------------------------------------------------------------------------|
| `WithResolver`      | `net.DefaultResolver` | Inject a resolver that honors context and supports concurrent calls    |
| `WithEgressPolicy`  | None                  | Apply [HTTP egress policy](../http/client) with `CheckHost(name, 0)`   |
| `WithMaxTimeout`    | Caller context only   | Apply an upper bound while honoring any earlier context deadline       |
| `WithMaxConcurrent` | Unlimited             | Cap concurrent lookups. A full limit returns `ErrBusy` immediately     |

Only host deny rules without a CIDR or port apply to the queried host. Configure those rules to block operator-defined zones such as `.internal`.
MX hosts in the response are left to the caller's connection policy.

## Errors

`LookupError.Error()` returns only the category text. Use `errors.Is` to match a category and `errors.Is` or `errors.As` to inspect the underlying
cause. Caller cancellation propagates as `context.Canceled`. A resolver can return partial records with an error. Treat those records as incomplete.

| Error         | Meaning                                                              |
|---------------|----------------------------------------------------------------------|
| `ErrNotFound` | The resolver reported that no MX was found                           |
| `ErrTimeout`  | The context deadline or resolver timeout elapsed                     |
| `ErrRefused`  | The query name is invalid, special-use, or denied by egress policy   |
| `ErrDenied`   | The egress policy denied the query name                              |
| `ErrBusy`     | All concurrency slots are occupied                                   |
| `ErrResolver` | The resolver failed or returned an invalid set                       |

A policy denial matches `ErrRefused`, `ErrDenied`, and `httpclient.ErrSSRFBlocked` via `errors.Is`.

[iana]: https://www.iana.org/assignments/special-use-domain-names

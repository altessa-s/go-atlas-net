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

`LookupMX` uses [egressrule.NormalizeHost](../egressrule) for the query name and passes it to the resolver with a trailing dot. Returned MX hosts
are lowercase and have no trailing dot. Results keep their DNS preference and sort by preference, then host, before truncation. A sole `0 .`
record returns an empty result. A root exchange with another preference or any other record makes the set invalid. The limit applies to returned
records after the resolver parses the DNS response.

## Options

| Option              | Default               | Behavior                                                               |
|---------------------|-----------------------|------------------------------------------------------------------------|
| `WithResolver`      | `net.DefaultResolver` | Inject a resolver that honors context and supports concurrent calls    |
| `WithEgressPolicy`  | None                  | Apply [HTTP egress policy](../http/client) with `CheckHost(name, 0)`   |
| `WithMaxRecords`    | 32                    | Cap sorted results. Nonpositive values restore the default             |
| `WithMaxTimeout`    | Caller context only   | Apply an upper bound while honoring any earlier context deadline       |
| `WithMaxConcurrent` | Unlimited             | Cap concurrent lookups. A full limit returns `ErrBusy` immediately     |

Only host deny rules without a CIDR or port apply to the queried host. Configure those rules to block operator-defined zones such as `.internal`.
MX hosts in the response are left to the caller's connection policy.

## Errors

Use `errors.Is` to match a category and `errors.As` to inspect a `*LookupError` or its underlying cause. Caller cancellation propagates as
`context.Canceled`. A resolver can return partial records with an error. Treat those records as incomplete.

| Error         | Meaning                                           |
|---------------|---------------------------------------------------|
| `ErrNotFound` | The resolver reported that no MX was found        |
| `ErrTimeout`  | The context deadline or resolver timeout elapsed |
| `ErrRefused`  | The query name is invalid or special-use          |
| `ErrDenied`   | The egress policy denied the query name           |
| `ErrBusy`     | All concurrency slots are occupied                |
| `ErrResolver` | The resolver failed or returned an invalid set    |

[iana]: https://www.iana.org/assignments/special-use-domain-names

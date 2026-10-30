# netipx

```go
import "github.com/altessa-s/go-atlas-net/netipx"
```

Classifies IP addresses and matches them against prefix lists, on top of `net/netip`. The outbound SSRF guard of
[`net/http/client`](../http/client) and the inbound IP ACLs and [`clientip`](../clientip) extraction share it, so an address is classified the same way
in both directions.

## Functions

| Function                       | Description                                                                         |
|--------------------------------|-------------------------------------------------------------------------------------|
| `IsPrivate(addr)`              | Private, loopback, link-local or reserved address                                   |
| `IsTrusted(addr, prefixes)`    | Address belongs to one of the trusted prefixes                                      |
| `InList(addr, prefixes)`       | Address belongs to one of the prefixes                                              |
| `ParsePrefixes(strs)`          | Parses CIDR (`10.0.0.0/8`) and single-address (`192.168.1.1`) strings into prefixes |

`IsPrivate` checks an IPv4-mapped IPv6 address (`::ffff:10.0.0.1`) and a well-known NAT64 address (`64:ff9b::a9fe:a9fe`) as the IPv4 address they
reach and ignores an IPv6 zone (`fe80::1%en0`), so none of these forms slips past the private, loopback, link-local and reserved ranges. The local-use
NAT64 prefix (`64:ff9b:1::/48`), IPv4-compatible (`::/96`), documentation (`3fff::/20`), SRv6 (`5f00::/16`) and site-local (`fec0::/10`) ranges are
reserved as well. `IsTrusted` and `InList` match the address exactly as given.

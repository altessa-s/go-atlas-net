# egressrule

```go
import "github.com/altessa-s/go-atlas-net/egressrule"
```

Package `egressrule` is the grammar of outbound (egress) allow and deny rules, shared by the configuration schema (`httpconfig.EgressRule` is an
alias of `egressrule.Rule`) and the egress engine (`httpclient.EgressPolicy`), so a rule is validated the same way wherever it is written. It
uses `golang.org/x/net/idna`, an explicit exception of the `core` boundary (see `docs/architecture.md`).

## Types

| Type      | Description                                                                                                                  |
|-----------|------------------------------------------------------------------------------------------------------------------------------|
| `Rule`    | Textual rule as configured or stored: `name`, `host`, `cidr`, `port` (YAML and JSON tags); every set matcher must match       |
| `Matcher` | Parsed rule: name, normalized host glob, `netip.Prefix`, `uint16` port                                                       |
| `List`    | `Allow` or `Deny`; decides the required matchers and names the rule in errors (`deny[1].host`)                              |

## Functions

| Function            | Description                                                                                                         |
|---------------------|---------------------------------------------------------------------------------------------------------------------|
| `Rule.Parse`        | Parses the CIDR and bounds the port (0 = any, 1 to 65535), then `Check`                                              |
| `Validate`          | Parses every allow and deny rule, returning the first error (deny rules first)                                      |
| `Check`             | Deny needs a matcher, allow needs a CIDR; name shape; host glob; prefix validity (an IPv4-mapped one is /96 or more) |
| `NormalizeHostGlob` | Validates a host glob and returns its matching form: lower case, ASCII dots, punycode labels                         |
| `NormalizeHost`     | A requested host in the form rules match it, without its trailing dot                                               |
| `NormalizePrefix`   | Masks a prefix and turns an IPv4-mapped one of /96 or more into the IPv4 prefix it covers                            |

## Rules

- A rule name is 1 to 64 characters of `A-Z`, `a-z`, `0-9`, `_`, `.`, `-`, `[` and `]`; it labels decisions and is not a matcher.
- A host glob is a `path.Match` pattern without scheme, port, path, user info, space, tab or trailing dot. A Unicode label is converted to
  punycode under the IDNA lookup profile; one holding a glob metacharacter, an invalid internationalized name, a label separator or mapping to
  nothing is rejected, and the converted form is checked again.
- A CIDR is an IPv4 or IPv6 prefix; an IPv4-mapped prefix shorter than /96 is rejected, since it would span IPv4-mapped and IPv6 space.

## Usage

```go
rule := egressrule.Rule{Name: "blockSSH", Host: "*.internal", Port: 22}
m, err := rule.Parse(egressrule.Deny, 0)
if err != nil {
    return err // "deny[0].host: ..."
}
```

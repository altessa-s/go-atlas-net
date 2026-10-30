# urlx

```go
import "github.com/altessa-s/go-atlas-net/urlx"
```

Package `urlx` validates, joins and canonicalizes the base URLs of services a client talks to. It rejects the forms that would move an appended
API path (and a credential in it) into the query or fragment — a bare `?` or `#` included — and userinfo, in one place.

## Functions

| Function       | Description                                                                                                                   |
|----------------|-------------------------------------------------------------------------------------------------------------------------------|
| `ParseBaseURL` | Parses an absolute URL with an allowed scheme and a host; rejects userinfo, query (bare `?` too) and fragment (bare `#` too)  |
| `JoinPath`     | Appends literal path elements to a parsed base, escaping each segment; rejects `.` / `..` segments with `ErrDotSegment`       |
| `Canonical`    | Lower-case scheme and host, escaped path without trailing slashes, no userinfo, query or fragment — for comparing endpoints   |

## Options

| Option               | Default                                     | Description                                                                     |
|----------------------|---------------------------------------------|---------------------------------------------------------------------------------|
| `WithAllowedSchemes` | `DefaultAllowedSchemes()` (`http`, `https`) | Accepted schemes, matched case-insensitively                                    |
| `WithEndpointMode`   | off (base mode)                             | Keep the path exactly as given; base mode trims trailing slashes for `JoinPath` |

## Errors

| Error                 | Description                                                                                               |
|-----------------------|-----------------------------------------------------------------------------------------------------------|
| `ErrInvalidURL`       | Every `ParseBaseURL` failure; wraps `coreerrs.ErrInvalidArgument`; the text names the rule, never the URL |
| `ErrSchemeNotAllowed` | A URL valid except for its scheme (the scheme is checked last); wraps `ErrInvalidURL`                     |
| `ErrDotSegment`       | A `JoinPath` element with a `.` or `..` segment; wraps `coreerrs.ErrInvalidArgument`; never names it      |

## Usage

```go
base, err := urlx.ParseBaseURL(cfg.BaseURL)
if err != nil {
    return fmt.Errorf("%w: base URL: %w", ErrConfig, err)
}
endpoint, err := urlx.JoinPath(base, "bot"+token, "sendMessage")
if err != nil {
    return err
}

// A full endpoint such as https://host/custom/recognize keeps its path.
ocr, err := urlx.ParseBaseURL(cfg.Endpoint, urlx.WithEndpointMode())

// Bind a stored credential to the endpoint it was issued for.
scope := urlx.Canonical(base)
```

## Semantics

- `ParseBaseURL` does not trim whitespace and does not lower-case the host; compare URLs with `Canonical`.
- The escaped path is never decoded: `/a%2Fb` and `/a/b` stay distinct in all three functions.
- `JoinPath` rejects any element with a `.` or `..` segment (`../x`, `a/./b`) with `ErrDotSegment`, so untrusted elements cannot climb out of the
  base path; names that merely contain dots (`...`, `.well-known`) and escaped dots (`%2e`, escaped to `%252e`) are literal. A trailing slash on
  the last element is kept.
- `Canonical` keeps an explicit port, default or not, because it changes the `Host` header a gateway may route on.

// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxyconfig

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/altessa-s/go-atlas-core/types/redacted"

	"github.com/altessa-s/go-atlas-net/urlx"
)

// Mode selects the proxy configuration. The empty value preserves transport defaults.
type Mode string

// Supported proxy modes.
const (
	ModeNone Mode = "none"
	ModeURL  Mode = "url"
	ModeHost Mode = "host"
)

const (
	maxHostnameLength  = 253
	maxHostLabelLength = 63
	maxPort            = 65535
)

// Validation errors identify the rejected field without retaining its value.
var (
	ErrMode     = errors.New("proxyconfig: invalid mode")
	ErrConflict = errors.New("proxyconfig: field incompatible with mode")
	ErrURL      = errors.New("proxyconfig: invalid proxy URL")
	ErrHost     = errors.New("proxyconfig: invalid proxy host")
	ErrPort     = errors.New("proxyconfig: port must be between 1 and 65535")
	ErrUsername = errors.New("proxyconfig: auth requires a username")
)

// Config is the shared YAML and JSON representation of an outbound proxy.
type Config struct {
	Mode Mode                    `yaml:"mode" json:"mode"`
	URL  redacted.RedactedString `yaml:"url,omitempty" json:"url,omitempty"`
	Host string                  `yaml:"host" json:"host"`
	Port int                     `yaml:"port" json:"port"`
	Auth *Auth                   `yaml:"auth" json:"auth"`
}

// Auth holds optional credentials for a host-mode proxy.
type Auth struct {
	Username string                  `yaml:"username" json:"username"`
	Password redacted.RedactedString `yaml:"password" json:"password"`
}

// String hides credentials during formatting.
func (Auth) String() string { return "[redacted]" }

// GoString hides credentials in Go-syntax output.
func (a Auth) GoString() string { return a.String() }

// String omits configured fields, including malformed URLs.
func (Config) String() string { return "proxy configuration" }

// GoString hides configured fields in Go-syntax output.
func (c Config) GoString() string { return c.String() }

// Validate checks all fields for consistency with the selected mode.
func (c *Config) Validate() error {
	_, err := c.URLValue()
	return err
}

// URLValue validates c and resolves it to a proxy URL. Nil, empty mode and
// ModeNone return nil. Consumers distinguish passthrough from explicit direct by Mode.
func (c *Config) URLValue() (*url.URL, error) {
	if c == nil {
		return nil, nil //nolint:nilnil // nil selects no explicit proxy URL
	}

	if c.Mode != ModeURL && c.URL != "" || c.Mode != ModeHost && (c.Host != "" || c.Port != 0 || c.Auth != nil) {
		return nil, ErrConflict
	}

	switch c.Mode {
	case "", ModeNone:
		return nil, nil //nolint:nilnil // nil selects no explicit proxy URL
	case ModeURL:
		u, err := urlx.ParseBaseURL(c.URL.Expose(), urlx.WithAllowedSchemes("http", "https", "socks5", "socks5h"), urlx.WithEndpointMode())
		if err != nil || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
			return nil, ErrURL
		}

		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > maxPort {
			return nil, ErrPort
		}

		return u, nil
	case ModeHost:
		if !validProxyHost(c.Host) {
			return nil, ErrHost
		}

		if c.Port < 1 || c.Port > maxPort {
			return nil, ErrPort
		}

		u := &url.URL{Scheme: "http", Host: net.JoinHostPort(c.Host, strconv.Itoa(c.Port))}
		if c.Auth != nil {
			if c.Auth.Username == "" {
				return nil, ErrUsername
			}

			u.User = url.User(c.Auth.Username)
			if c.Auth.Password != "" {
				u.User = url.UserPassword(c.Auth.Username, c.Auth.Password.Expose())
			}
		}

		return u, nil
	default:
		return nil, ErrMode
	}
}

// validProxyHost accepts an unbracketed, zone-free IP or an ASCII DNS name.
// International names must be supplied as punycode: URL.String would escape
// Unicode input. Ports and IPv6 brackets belong only in URLValue's assembly.
func validProxyHost(host string) bool {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == ""
	}

	name := strings.TrimSuffix(host, ".")
	if name == "" || len(name) > maxHostnameLength {
		return false
	}

	for label := range strings.SplitSeq(name, ".") {
		if len(label) == 0 || len(label) > maxHostLabelLength || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for _, c := range label {
			if c != '-' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
				return false
			}
		}
	}

	return true
}

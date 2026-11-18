// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxyconfig_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/altessa-s/go-atlas-core/types/redacted"

	"github.com/altessa-s/go-atlas-net/proxyconfig"
)

func TestModes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		c    *proxyconfig.Config
		want error
	}{
		{"nil", nil, nil}, {"empty", &proxyconfig.Config{}, nil}, {"none", &proxyconfig.Config{Mode: "none"}, nil},
		{"unknown", &proxyconfig.Config{Mode: "unknown"}, proxyconfig.ErrMode},
		{"inactiveURL", &proxyconfig.Config{Mode: "host", URL: "secret"}, proxyconfig.ErrConflict},
		{"inactiveHost", &proxyconfig.Config{Mode: "url", Host: "host"}, proxyconfig.ErrConflict},
		{"inactivePort", &proxyconfig.Config{Port: 1}, proxyconfig.ErrConflict},
		{"inactiveAuth", &proxyconfig.Config{Mode: "none", Auth: &proxyconfig.Auth{}}, proxyconfig.ErrConflict},
		{"missingHost", &proxyconfig.Config{Mode: "host", Port: 1}, proxyconfig.ErrHost},
		{"missingPort", &proxyconfig.Config{Mode: "host", Host: "localhost"}, proxyconfig.ErrPort},
		{"negativePort", &proxyconfig.Config{Mode: "host", Host: "localhost", Port: -1}, proxyconfig.ErrPort},
		{"largePort", &proxyconfig.Config{Mode: "host", Host: "localhost", Port: 65536}, proxyconfig.ErrPort},
		{"missingUser", &proxyconfig.Config{Mode: "host", Host: "localhost", Port: 1, Auth: &proxyconfig.Auth{}}, proxyconfig.ErrUsername},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, tc.c.Validate(), tc.want)
			u, err := tc.c.URLValue()
			require.ErrorIs(t, err, tc.want)
			require.Nil(t, u)
		})
	}
}

func TestURL(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]error{
		"": proxyconfig.ErrURL, "http://%": proxyconfig.ErrURL, "proxy": proxyconfig.ErrURL, "http://:123": proxyconfig.ErrURL,
		"ftp://p:1": proxyconfig.ErrURL, "http://p": proxyconfig.ErrPort, "http://p:0": proxyconfig.ErrPort, "http://p:65536": proxyconfig.ErrPort,
		"http://u:p@@p:1": proxyconfig.ErrURL, "http://p:1/a@b": proxyconfig.ErrURL, "http://p:1?q": proxyconfig.ErrURL,
		"http://p:1/%2F": proxyconfig.ErrURL,
		"http://p:1/??":  proxyconfig.ErrURL, "http://p:1/##": proxyconfig.ErrURL,
		"http://p:1/?x#": proxyconfig.ErrURL, "http://p:1/#x?": proxyconfig.ErrURL, "http://p:1#frag": proxyconfig.ErrURL, "http://p:1/path": proxyconfig.ErrURL,
		"http://p:1?": nil, "http://p:1/#": nil, "http://p:1/?#": nil,
		"http://u@p:1": nil, "https://u:p%40ss@p:1": nil, "socks5://u:p%2Fss@p:1": nil, "socks5h://u:p@p:1": nil,
		"http://p:1": nil, "https://p:65535/": nil, "socks5://p:1080": nil, "socks5h://[::1]:1080": nil,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()

			c := proxyconfig.Config{Mode: "url", URL: redacted.RedactedString(raw)}
			u, err := c.URLValue()
			require.ErrorIs(t, err, want)

			if want == nil {
				require.Equal(t, strings.TrimSuffix(raw, "#"), u.String())
			} else {
				require.Nil(t, u)
			}
		})
	}
}

func TestHost(t *testing.T) {
	t.Parallel()

	for _, host := range []string{
		"", "[::1]", "p:80", "127.0.0.1:80", "[fe80::1%lo]:80", "proxy/name", "u@proxy",
		"p q", "p\u00a0q", "p\x00q", "p\u0085q", "fe80::1%eth/0", "fe80::1%a@b", "fe80::1%a\tb",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			c := proxyconfig.Config{Mode: "host", Host: host, Port: 1}
			require.ErrorIs(t, c.Validate(), proxyconfig.ErrHost)
		})
	}

	for _, host := range []string{
		"proxy.test.", "127.0.0.1", "::1", "fe80::1%eth0", "p..t", "-p", "p-", "p_t", "bücher",
		strings.Repeat("a", 64), strings.Repeat("a.", 127) + "x",
	} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()

			c := proxyconfig.Config{Mode: "host", Host: host, Port: 65535, Auth: &proxyconfig.Auth{Username: "u"}}
			u, err := c.URLValue()
			require.NoError(t, err)
			require.Equal(t, host, u.Hostname())
			parsed, err := url.Parse(u.String())
			require.NoError(t, err)
			require.Equal(t, host, parsed.Hostname())
			require.Equal(t, "u", parsed.User.Username())
			_, present := u.User.Password()
			require.False(t, present)
		})
	}
}

func TestWireSchema(t *testing.T) {
	t.Parallel()

	const input = `{"mode":"host","host":"::1","port":80,"auth":{"username":"user","password":"secret!"}}`

	for name, decode := range map[string]func([]byte, any) error{"json": json.Unmarshal, "yaml": yaml.Unmarshal} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var c proxyconfig.Config
			require.NoError(t, decode([]byte(input), &c))
			u, err := c.URLValue()
			require.NoError(t, err)
			require.Equal(t, "http://user:secret%21@[::1]:80", u.String())

			c.URL = "url-secret"
			for _, v := range []any{c, &c, *c.Auth, c.Auth} {
				for _, format := range []string{"%v", "%+v", "%#v"} {
					output := fmt.Sprintf(format, v)
					require.NotContains(t, output, "secret")
				}
			}

			c.URL = ""
			data, err := yaml.Marshal(c)
			require.NoError(t, err)
			require.NotContains(t, string(data), "secret!")

			var again proxyconfig.Config
			require.NoError(t, yaml.Unmarshal(data, &again))
			require.Equal(t, c.Host, again.Host)
			require.Equal(t, c.Mode, again.Mode)
			require.Equal(t, c.Port, again.Port)
			require.Equal(t, c.Auth.Username, again.Auth.Username)
			require.Equal(t, "<redacted>", again.Auth.Password.Expose())
		})
	}
}

func TestURLRedaction(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  redacted.RedactedString
		want error
	}{
		{"valid", "https://secret-user:secret-password@proxy:443", nil},
		{"ambiguous", "http://user:prefix@secret-host:80/rest@proxy:80", proxyconfig.ErrURL},
		{"malformed", "http://secret-user:%xx@p:80", proxyconfig.ErrURL},
		{"missing port", "http://secret-user@p", proxyconfig.ErrPort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			c := proxyconfig.Config{Mode: proxyconfig.ModeURL, URL: tc.raw}
			err := c.Validate()
			require.ErrorIs(t, err, tc.want)

			if err != nil {
				require.NotContains(t, err.Error(), "secret")
			}

			j, err := json.Marshal(c)
			require.NoError(t, err)
			y, err := yaml.Marshal(c)
			require.NoError(t, err)

			var logs bytes.Buffer
			slog.New(slog.NewTextHandler(&logs, nil)).Info("proxy", "config", c)

			for _, out := range []string{string(j), string(y), logs.String(), fmt.Sprintf("%v %+v %#v", c, c, c)} {
				require.NotContains(t, out, "secret")
			}
		})
	}
}

func FuzzURLValue(f *testing.F) {
	for _, seed := range []string{"socks5://proxy:1080", "https://u:p%40ss@p:443/?#", "http://u:p@@p:80", "fe80::1%eth0", "proxy_local", "p:123"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		c := proxyconfig.Config{Mode: proxyconfig.ModeURL, URL: redacted.RedactedString(raw)}

		u, err := c.URLValue()
		if err != nil {
			require.Nil(t, u)
			require.Contains(t, []error{proxyconfig.ErrURL, proxyconfig.ErrPort}, err)
		} else {
			require.False(t, redacted.URL(raw).HasAmbiguousUserInfo())
			require.Contains(t, []string{"http", "https", "socks5", "socks5h"}, u.Scheme)
			require.Contains(t, []string{"", "/"}, u.EscapedPath())
			require.Empty(t, u.RawQuery)
			require.Empty(t, u.Fragment)
			require.NotEmpty(t, u.Hostname())
			port, portErr := strconv.Atoi(u.Port())
			require.NoError(t, portErr)
			require.Greater(t, port, 0)
			require.LessOrEqual(t, port, 65535)

			parsed, parseErr := url.Parse(raw)
			require.NoError(t, parseErr)
			require.Equal(t, parsed, u)
		}

		host := proxyconfig.Config{Mode: proxyconfig.ModeHost, Host: raw, Port: 80}

		u, err = host.URLValue()
		if err != nil {
			require.ErrorIs(t, err, proxyconfig.ErrHost)
			require.Nil(t, u)
		} else {
			require.Equal(t, net.JoinHostPort(raw, "80"), u.Host)
			require.Equal(t, "http", u.Scheme)
		}
	})
}

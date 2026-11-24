// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package proxyconfig_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/altessa-s/go-atlas-net/proxyconfig"
)

func TestAuthDefaultTag(t *testing.T) {
	t.Parallel()

	// go-nomos allocates optional struct pointers during defaulting unless they opt out.
	field, ok := reflect.TypeFor[proxyconfig.Config]().FieldByName("Auth")
	require.True(t, ok)
	require.Equal(t, "-", field.Tag.Get("default"))
}

func TestLoadProxyYAML(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		settings string
		mode     proxyconfig.Mode
		auth     *proxyconfig.Auth
		err      error
	}{
		{"direct", "{mode: none}", proxyconfig.ModeNone, nil, nil},
		{"url", "{mode: url, url: 'http://proxy.example:3128'}", proxyconfig.ModeURL, nil, nil},
		{"host", "{mode: host, host: proxy.example, port: 3128}", proxyconfig.ModeHost, nil, nil},
		{"credentials", "{mode: host, host: proxy.example, port: 3128, auth: {username: alice, password: secret}}",
			proxyconfig.ModeHost, &proxyconfig.Auth{Username: "alice", Password: "secret"}, nil},
		{"forbidden", "{mode: none, auth: {}}", proxyconfig.ModeNone, &proxyconfig.Auth{}, proxyconfig.ErrConflict},
		{"missing username", "{mode: host, host: proxy.example, port: 3128, auth: {}}", proxyconfig.ModeHost, &proxyconfig.Auth{}, proxyconfig.ErrUsername},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var cfg struct {
				Proxy *proxyconfig.Config `yaml:"proxy" default:"-"`
			}

			require.NoError(t, yaml.Unmarshal([]byte("proxy: "+tc.settings), &cfg))
			require.NotNil(t, cfg.Proxy)
			require.Equal(t, tc.mode, cfg.Proxy.Mode)
			require.ErrorIs(t, cfg.Proxy.Validate(), tc.err)
			require.Equal(t, tc.auth, cfg.Proxy.Auth)
		})
	}
}

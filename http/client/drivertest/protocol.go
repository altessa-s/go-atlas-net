// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package drivertest

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/altessa-s/go-atlas-net/http/client"
	"github.com/altessa-s/go-atlas-net/proxydial"
)

func proxyDial(t *testing.T, s *httptest.Server) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()

	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		upstream, err := (&net.Dialer{}).DialContext(r.Context(), "tcp", s.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		downstream, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}

		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		done := make(chan struct{})

		go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); close(done) }()

		_, _ = io.Copy(downstream, upstream)
		_ = downstream.Close()

		<-done
	}))
	t.Cleanup(p.Close)
	u, err := url.Parse(p.URL)
	require.NoError(t, err)
	dial, err := proxydial.FromURL(u)
	require.NoError(t, err)

	return dial
}

func testH2Replay(t *testing.T, f func(*testing.T) client.TransportDriver) {
	for _, goAway := range []bool{false, true} {
		for _, rewindable := range []bool{false, true} {
			name := map[bool]string{false: "REFUSED_STREAM", true: "GOAWAY"}[goAway] + map[bool]string{false: "/stream", true: "/rewindable"}[rewindable]
			t.Run(name, func(t *testing.T) {
				// Use httptest only for its local certificate; framing below deliberately
				// refuses the first fully received stream before processing it.
				certServer := server(t, nil, nil, true)
				ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certServer.TLS.Certificates, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
				require.NoError(t, err)

				var streams atomic.Int32

				done := make(chan struct{})
				ctx, cancel := context.WithCancel(t.Context())

				go func() {
					defer close(done)

					for {
						conn, err := ln.Accept()
						if err != nil {
							return
						}

						stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

						serveFrames(conn, &streams, goAway)
						stop()

						_ = conn.Close()
					}
				}()

				t.Cleanup(func() { cancel(); _ = ln.Close(); <-done })

				env := environment(certServer)
				env.TLS.NextProtos = []string{"h2"}
				env.Dial = func(ctx context.Context, n, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, n, ln.Addr().String())
				}
				tr := transport(t, f(t), env, false)

				var body io.Reader = strings.NewReader("payload")
				if !rewindable {
					body = io.LimitReader(body, 7)
				}

				resp, err := roundTrip(t, tr, "POST", body)
				require.Positive(t, streams.Load())

				if rewindable {
					require.LessOrEqual(t, streams.Load(), int32(2), "at most one safe transport retry")

					if err == nil {
						require.Equal(t, http.StatusOK, resp.StatusCode)
						require.EqualValues(t, 2, streams.Load())
					}
				} else {
					require.Error(t, err)
					require.EqualValues(t, 1, streams.Load(), "streaming body cannot be replayed")
				}
			})
		}
	}
}

func serveFrames(conn net.Conn, streams *atomic.Int32, goAway bool) {
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
		return
	}

	fr := http2.NewFramer(conn, conn)
	if err := fr.WriteSettings(); err != nil {
		return
	}

	for {
		frame, err := fr.ReadFrame()
		if err != nil {
			return
		}

		var stream uint32

		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				if err := fr.WriteSettingsAck(); err != nil {
					return
				}
			}
		case *http2.HeadersFrame:
			if frame.StreamEnded() {
				stream = frame.StreamID
			}
		case *http2.DataFrame:
			if frame.StreamEnded() {
				stream = frame.StreamID
			}
		}

		if stream == 0 {
			continue
		}

		if streams.Add(1) == 1 {
			if goAway {
				_ = fr.WriteGoAway(0, http2.ErrCodeNo, nil)
				return
			}

			if err := fr.WriteRSTStream(stream, http2.ErrCodeRefusedStream); err != nil {
				return
			}

			continue
		}

		var block bytes.Buffer

		enc := hpack.NewEncoder(&block)
		if err := enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
			return
		}

		if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: stream, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
			return
		}
	}
}

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/altessa-s/go-atlas-net/http/client"
	"github.com/altessa-s/go-atlas-net/proxydial"
)

const (
	replayTimeout     = 500 * time.Millisecond
	replayMargin      = 500 * time.Millisecond
	replayCancelDelay = 250 * time.Millisecond
	replaySettling    = 100 * time.Millisecond
	// Observe beyond the first HTTP/2 retry backoff to catch detached retries.
	replayObservation = 1200 * time.Millisecond
)

// replayEvents counts connections and stream starts so retries can be checked
// after in-flight work has settled following the request's return.
type replayEvents struct {
	mu                   sync.Mutex
	connections, streams int
}

func (e *replayEvents) record(connection bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if connection {
		e.connections++
	} else {
		e.streams++
	}
}

func (e *replayEvents) counts() (connections, streams int) {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.connections, e.streams
}

func (e *replayEvents) check(t *testing.T) {
	t.Helper()

	// A connection or stream already opening when the context ends may reach
	// the peer later. Allow it to settle before checking that retrying stops.
	time.Sleep(replaySettling)

	connections, streams := e.counts()
	require.Positive(t, streams, "fixture must receive a request")

	time.Sleep(replayObservation)

	finalConnections, finalStreams := e.counts()
	require.Equal(t, connections, finalConnections, "connections grew after request returned and settled")
	require.Equal(t, streams, finalStreams, "streams grew after request returned and settled")
}

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

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			return
		}

		downstream, rw, err := hijacker.Hijack()
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
		for _, mode := range []string{refuseOnce, "always", modeWithoutRetry} {
			for _, rewindable := range []bool{false, true} {
				name := map[bool]string{false: "REFUSED_STREAM", true: "GOAWAY"}[goAway] + map[bool]string{false: streamSuffix, true: rewindableSuffix}[rewindable]
				t.Run(mode+"/"+name, func(t *testing.T) {
					t.Parallel()

					if mode == refuseOnce {
						testH2ReplayCase(t, f, goAway, mode, rewindable, false)
						return
					}

					for _, callerCancel := range []bool{false, true} {
						t.Run(map[bool]string{false: "deadline", true: "cancel"}[callerCancel], func(t *testing.T) {
							testH2ReplayCase(t, f, goAway, mode, rewindable, callerCancel)
						})
					}
				})
			}
		}
	}
}

func testH2ReplayCase(t *testing.T, f func(*testing.T) client.TransportDriver, goAway bool, mode string, rewindable, callerCancel bool) {
	var streams atomic.Int32

	var events replayEvents

	env := h2ReplayEnvironment(t, &events, func(conn net.Conn) {
		serveFrames(conn, &streams, &events, goAway, mode != refuseOnce)
	})

	var tr http.RoundTripper

	if mode == modeWithoutRetry {
		c := client.New(client.WithoutSSRFProtection(), client.WithoutProxy(), client.WithoutRetry(), client.WithTransportDriver(f(t)),
			client.WithTransport(&http.Transport{DialContext: env.Dial, TLSClientConfig: env.TLS}))
		t.Cleanup(c.CloseIdleConnections)
		tr = c.Transport
	} else {
		tr = transport(t, f(t), env, false)
	}

	var body io.Reader = strings.NewReader(payload)
	if !rewindable {
		body = io.LimitReader(body, int64(len(payload)))
	}

	// Persistent refusals are bounded by this request, not a retry count.
	timeout := replayTimeout
	if mode == refuseOnce {
		timeout = testTimeout
	}

	requestCtx, requestCancel := context.WithTimeout(t.Context(), timeout)
	defer requestCancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "https://"+authority+"/", body)
	require.NoError(t, err)

	if mode != refuseOnce {
		checkPersistentRefusal(t, tr, req, requestCancel, callerCancel, &events)

		if !rewindable {
			require.EqualValues(t, 1, streams.Load(), "streaming body cannot be replayed")
		}

		return
	}

	resp, err := tr.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}

	require.NoError(t, requestCtx.Err())
	require.Positive(t, streams.Load())

	if rewindable {
		if err == nil {
			require.Equal(t, http.StatusOK, resp.StatusCode)
		}
	} else {
		require.Error(t, err)
		require.EqualValues(t, 1, streams.Load(), "streaming body cannot be replayed")
	}
}

func checkPersistentRefusal(t *testing.T, tr http.RoundTripper, req *http.Request,
	cancel context.CancelFunc, callerCancel bool, events *replayEvents) {
	t.Helper()

	type result struct {
		err      error
		returned time.Time
	}

	returned := make(chan result, 1)

	go func() {
		resp, err := tr.RoundTrip(req)
		if resp != nil {
			_ = resp.Body.Close()
		}

		returned <- result{err: err, returned: time.Now()}
	}()

	ended, _ := req.Context().Deadline()

	if callerCancel {
		time.Sleep(replayCancelDelay)

		ended = time.Now()

		cancel()
	} else {
		<-req.Context().Done()
	}

	// A driver may give up earlier. It must return an error by this bound even
	// if it ignores the context, so run RoundTrip separately from the watchdog.
	timer := time.NewTimer(time.Until(ended.Add(replayMargin)))
	defer timer.Stop()

	select {
	case result := <-returned:
		require.Error(t, result.err)
		require.False(t, result.returned.After(ended.Add(replayMargin)), "request returned too late after context ended")
	case <-timer.C:
		t.Fatal("persistent refusal did not return promptly after context ended")
	}

	// Keep the peer live after RoundTrip returns, so background transmissions
	// remain observable instead of being hidden by fixture cleanup.
	events.check(t)
}

func serveFrames(conn net.Conn, streams *atomic.Int32, events *replayEvents, goAway, always bool) {
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

		if _, ok := frame.(*http2.HeadersFrame); ok {
			events.record(false)
		}

		stream, ok := endedStream(fr, frame)
		if !ok {
			return
		}

		if stream == 0 {
			continue
		}

		if count := streams.Add(1); count == 1 || always {
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

func endedStream(fr *http2.Framer, frame http2.Frame) (uint32, bool) {
	var stream uint32

	switch frame := frame.(type) {
	case *http2.SettingsFrame:
		if !frame.IsAck() {
			if err := fr.WriteSettingsAck(); err != nil {
				return 0, false
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

	return stream, true
}

const refuseOnce = "once"

func h2ReplayEnvironment(t *testing.T, events *replayEvents, serve func(net.Conn)) client.DriverEnv {
	t.Helper()

	// Use httptest only for its local certificate; serve handles HTTP/2 framing.
	certServer := server(t, nil, nil, true)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: certServer.TLS.Certificates,
		NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12})
	require.NoError(t, err)

	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		defer close(done)

		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}

			events.record(true)

			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

			serve(conn)
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

	return env
}

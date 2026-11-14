// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool

import (
	"container/list"
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Selector selects a transport key and its lazy builder for a request context.
// The builder is called only on a cache miss, serialized with other builders.
type Selector func(ctx context.Context) (key string, build func() http.RoundTripper)

// Retirer is implemented by a transport that can stop keeping connections.
// Retire switches it to closing every connection as soon as that connection
// becomes idle; it must keep serving requests, both those in flight and
// those that arrive after Retire (a request the pool acquired the transport
// for just before evicting it can still be dispatched), on connections it
// does not keep. The pool calls Retire once, when it evicts the transport.
// A transport that does not implement it is cleaned up
// with CloseIdleConnections as described on [Pool]; the connections that
// cleanup misses live until the transport's own idle timeout, so such a
// transport should have one.
type Retirer interface {
	Retire()
}

// Pool is a concurrent, bounded LRU of transports. Construct it with New.
//
// Eviction does not interrupt active requests. An evicted transport is
// retired: it serves no new request and the pool keeps owning its cleanup.
// A [Retirer] is told at eviction. Its idle connections are closed at
// eviction, whenever one of its responses ends or is closed, [WithRetireDelay]
// after the latest of those events (a transport may hand a connection back
// only after the body was closed), on every later eviction and on every
// [Pool.CloseIdleConnections]. The pool keeps as many retired transports as
// it caches; the oldest is dropped, after one more cleanup, when a newer one
// retires.
type Pool struct {
	mu          sync.Mutex
	selector    Selector
	max         int
	retireDelay time.Duration
	entries     map[string]*list.Element
	lru         list.List
	retired     []*entry // oldest first, at most max
}

type entry struct {
	key       string
	transport http.RoundTripper
	// evicted is set once; timer is the delayed cleanup of the retired entry.
	// Both are guarded by Pool.mu.
	evicted bool
	timer   *time.Timer
}

// New constructs a pool. A nil selector panics. Nonpositive limits use 64.
func New(selector Selector, opts ...Option) *Pool {
	if selector == nil {
		panic("transportpool: nil selector")
	}

	o := newOptions(opts...)
	if o.maxEntries <= 0 {
		o.maxEntries = DefaultMaxEntries
	}

	if o.retireDelay <= 0 {
		o.retireDelay = DefaultRetireDelay
	}

	return &Pool{
		selector: selector, max: o.maxEntries, retireDelay: o.retireDelay,
		entries: make(map[string]*list.Element),
	}
}

// RoundTrip selects a transport and sends the request exactly once.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	key, build := p.selector(req.Context())

	e, evicted := p.acquire(key, build)
	if evicted != nil {
		p.retire(evicted)
	}

	resp, err := e.transport.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		p.cleanup(e)

		return resp, err
	}

	body := &responseBody{ReadCloser: resp.Body, cleanup: func() { p.cleanup(e) }}
	if writer, ok := resp.Body.(io.Writer); ok {
		resp.Body = &responseReadWriteBody{responseBody: body, Writer: writer}
	} else {
		resp.Body = body
	}

	return resp, nil
}

// acquire returns the entry of key, building it on a miss, and the entry that
// had to be evicted for it, if any.
func (p *Pool) acquire(key string, build func() http.RoundTripper) (*entry, *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if el := p.entries[key]; el != nil {
		p.lru.MoveToFront(el)

		return el.Value.(*entry), nil //nolint:errcheck // Only *entry values enter this private list.
	}

	tr := build()
	if tr == nil {
		panic("transportpool: builder returned nil")
	}

	e := &entry{key: key, transport: tr}
	p.entries[key] = p.lru.PushFront(e)

	var old *entry

	if p.lru.Len() > p.max {
		el := p.lru.Back()
		old = el.Value.(*entry) //nolint:errcheck // Only *entry values enter this private list.
		old.evicted = true
		delete(p.entries, old.key)
		p.lru.Remove(el)
	}

	return e, old
}

// retire takes an evicted entry into the retired set, dropping the oldest
// retired entry beyond the limit, and cleans every retired transport up.
func (p *Pool) retire(e *entry) {
	if r, ok := e.transport.(Retirer); ok {
		r.Retire()
	}

	p.mu.Lock()
	p.retired = append(p.retired, e)

	var dropped *entry

	if len(p.retired) > p.max {
		dropped = p.retired[0]
		p.retired = slices.Delete(p.retired, 0, 1)

		// The pool stops owning dropped: its pending delayed cleanup must not
		// keep it alive. A response of it still in flight schedules its own.
		if dropped.timer != nil {
			dropped.timer.Stop()
		}
	}

	retired := slices.Clone(p.retired)
	p.mu.Unlock()

	if dropped != nil {
		closeIdle(dropped.transport)
	}

	for _, r := range retired {
		if r != e { // cleanup below takes care of e
			closeIdle(r.transport)
		}
	}

	p.cleanup(e)
}

// cleanup closes the idle connections of e when it is retired and schedules
// one more cleanup after the retire delay. It runs at eviction, at the end of
// a body and again at its Close, so the delay counts from the latest of them
// whether or not other responses of e are still open. The delayed cleanup is
// one timer per entry, restarted rather than multiplied.
func (p *Pool) cleanup(e *entry) {
	p.mu.Lock()
	evicted := e.evicted

	if evicted {
		if e.timer == nil {
			e.timer = time.AfterFunc(p.retireDelay, func() { closeIdle(e.transport) })
		} else {
			e.timer.Reset(p.retireDelay)
		}
	}
	p.mu.Unlock()

	if evicted {
		closeIdle(e.transport)
	}
}

// CloseIdleConnections closes idle connections on every cached transport and
// on every retired one. Active requests continue and the pool remains usable.
func (p *Pool) CloseIdleConnections() {
	p.mu.Lock()

	transports := make([]http.RoundTripper, 0, len(p.entries)+len(p.retired))
	for _, el := range p.entries {
		transports = append(transports, el.Value.(*entry).transport) //nolint:errcheck // Only *entry values enter this private list.
	}

	for _, e := range p.retired {
		transports = append(transports, e.transport)
	}
	p.mu.Unlock()

	for _, tr := range transports {
		closeIdle(tr)
	}
}

func closeIdle(tr http.RoundTripper) {
	if c, ok := tr.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// responseBody asks for the cleanup of its transport when the body ends
// (once: later terminal reads repeat the error and change nothing) and again
// when it is closed: a transport may still hold the connection busy at the
// end of the body and release it only in Close.
type responseBody struct {
	io.ReadCloser
	eof     sync.Once
	cleanup func()
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.eof.Do(b.cleanup)
	}

	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()

	b.cleanup()

	return err
}

// responseReadWriteBody preserves duplex upgraded responses and release tracking.
type responseReadWriteBody struct {
	*responseBody
	io.Writer
}

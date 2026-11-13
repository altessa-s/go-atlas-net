// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package transportpool

import (
	"container/list"
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Selector selects a transport key and its lazy builder for a request context.
// The builder is called only on a cache miss, serialized with other builders.
type Selector func(ctx context.Context) (key string, build func() http.RoundTripper)

// Pool is a concurrent, bounded LRU of transports. Construct it with New.
//
// Eviction does not interrupt active requests. An evicted transport is
// retired: it serves no new request, its idle connections are closed at once,
// again whenever one of its responses ends or is closed, and a final time
// [WithRetireDelay] after its last response finished — a transport may hand a
// connection back only after the body was closed. Until that final cleanup a
// retired transport stays reachable by [Pool.CloseIdleConnections].
type Pool struct {
	mu          sync.Mutex
	selector    Selector
	max         int
	retireDelay time.Duration
	entries     map[string]*list.Element
	lru         list.List
	retired     map[*entry]struct{}
}

type entry struct {
	key       string
	transport http.RoundTripper
	// inflight counts the requests sent on the transport whose response has
	// not finished; evicted is set once.
	inflight int
	evicted  bool
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
		entries: make(map[string]*list.Element), retired: make(map[*entry]struct{}),
	}
}

// RoundTrip selects a transport and sends the request exactly once.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	key, build := p.selector(req.Context())

	e, evicted := p.acquire(key, build)
	if evicted != nil {
		p.cleanup(evicted)
	}

	resp, err := e.transport.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		p.finish(e)
		p.cleanup(e)

		return resp, err
	}

	body := &responseBody{ReadCloser: resp.Body, finish: func() { p.finish(e) }, cleanup: func() { p.cleanup(e) }}
	if writer, ok := resp.Body.(io.Writer); ok {
		resp.Body = &responseReadWriteBody{responseBody: body, Writer: writer}
	} else {
		resp.Body = body
	}

	return resp, nil
}

// acquire returns the entry of key with one more request in flight, building
// it on a miss, and the entry that had to be evicted for it, if any.
func (p *Pool) acquire(key string, build func() http.RoundTripper) (*entry, *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if el := p.entries[key]; el != nil {
		p.lru.MoveToFront(el)

		e := el.Value.(*entry) //nolint:errcheck // Only *entry values enter this private list.
		e.inflight++

		return e, nil
	}

	tr := build()
	if tr == nil {
		panic("transportpool: builder returned nil")
	}

	e := &entry{key: key, transport: tr, inflight: 1}
	p.entries[key] = p.lru.PushFront(e)

	var old *entry

	if p.lru.Len() > p.max {
		el := p.lru.Back()
		old = el.Value.(*entry) //nolint:errcheck // Only *entry values enter this private list.
		old.evicted = true
		delete(p.entries, old.key)
		p.lru.Remove(el)
		p.retired[old] = struct{}{}
	}

	return e, old
}

// finish records the end of one response of e.
func (p *Pool) finish(e *entry) {
	p.mu.Lock()
	e.inflight--
	p.mu.Unlock()
}

// cleanup closes the idle connections of e when it is retired and, while no
// response of it is in flight, schedules the delayed cleanup that forgets it.
// It runs at the end of a body and again at its Close, so the delay counts
// from whichever comes last.
func (p *Pool) cleanup(e *entry) {
	p.mu.Lock()
	evicted := e.evicted
	last := evicted && e.inflight == 0
	p.mu.Unlock()

	if !evicted {
		return
	}

	closeIdle(e.transport)

	if last {
		time.AfterFunc(p.retireDelay, func() {
			closeIdle(e.transport)

			p.mu.Lock()
			delete(p.retired, e)
			p.mu.Unlock()
		})
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

	for e := range p.retired {
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

// responseBody reports the end of a response once (finish) and asks for the
// cleanup of its transport both when the body ends and when it is closed: a
// transport may still hold the connection busy at the end of the body and
// release it only in Close.
type responseBody struct {
	io.ReadCloser
	once    sync.Once
	finish  func()
	cleanup func()
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.finish)
		b.cleanup()
	}

	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()

	b.once.Do(b.finish)
	b.cleanup()

	return err
}

// responseReadWriteBody preserves duplex upgraded responses and release tracking.
type responseReadWriteBody struct {
	*responseBody
	io.Writer
}

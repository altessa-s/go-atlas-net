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
)

// Selector selects a transport key and its lazy builder for a request context.
// The builder is called only on a cache miss, serialized with other builders.
type Selector func(ctx context.Context) (key string, build func() http.RoundTripper)

// Pool is a concurrent, bounded LRU of transports. Construct it with New.
// Eviction does not interrupt active requests; their idle connections are
// closed again when their response bodies finish.
type Pool struct {
	mu       sync.Mutex
	selector Selector
	max      int
	entries  map[string]*list.Element
	lru      list.List
}

type entry struct {
	key       string
	transport http.RoundTripper
	evicted   bool
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

	return &Pool{selector: selector, max: o.maxEntries, entries: make(map[string]*list.Element)}
}

// RoundTrip selects a transport and sends the request exactly once.
func (p *Pool) RoundTrip(req *http.Request) (*http.Response, error) {
	key, build := p.selector(req.Context())

	e, evicted := p.acquire(key, build)
	if evicted != nil {
		closeIdle(evicted.transport)
	}

	resp, err := e.transport.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		p.release(e)
	} else {
		body := &responseBody{ReadCloser: resp.Body, done: func() { p.release(e) }}
		if writer, ok := resp.Body.(io.Writer); ok {
			resp.Body = &responseReadWriteBody{responseBody: body, Writer: writer}
		} else {
			resp.Body = body
		}
	}

	return resp, err
}

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

func (p *Pool) release(e *entry) {
	p.mu.Lock()
	evicted := e.evicted
	p.mu.Unlock()

	if evicted {
		closeIdle(e.transport)
	}
}

// CloseIdleConnections closes idle connections on every cached transport.
// Active requests continue and the pool remains usable.
func (p *Pool) CloseIdleConnections() {
	p.mu.Lock()

	transports := make([]http.RoundTripper, 0, len(p.entries))
	for _, el := range p.entries {
		transports = append(transports, el.Value.(*entry).transport) //nolint:errcheck // Only *entry values enter this private list.
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

// responseBody runs done when the body ends and again when it is closed. A
// transport may still hold the connection busy at the end of the body (an
// HTTP/2 stream is cleaned up asynchronously) and release it only in Close,
// so the cleanup of an evicted transport is repeated there; done is cheap and
// idempotent.
type responseBody struct {
	io.ReadCloser
	done func()
}

func (b *responseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.done()
	}

	return n, err
}

func (b *responseBody) Close() error {
	err := b.ReadCloser.Close()
	b.done()

	return err
}

// responseReadWriteBody preserves duplex upgraded responses and release tracking.
type responseReadWriteBody struct {
	*responseBody
	io.Writer
}

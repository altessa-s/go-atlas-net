// Copyright 2021-2026 ALTESSA SOLUTIONS INC.
// Use of this source code is governed by the MIT license that can be found in
// the LICENSE file.

package client

import "io"

// closeWriter is the optional half-close of a writable (101 Switching
// Protocols) body, as net/http's provides.
type closeWriter interface {
	CloseWrite() error
}

// writeSide is a body wrapper rc given the write side of the stream it wraps.
type writeSide struct {
	io.ReadCloser
	io.Writer
}

// writeSideCW is a [writeSide] that also keeps the stream's CloseWrite.
type writeSideCW struct {
	io.ReadCloser
	io.Writer

	cw closeWriter
}

// CloseWrite half-closes the wrapped stream.
func (b *writeSideCW) CloseWrite() error { return b.cw.CloseWrite() }

// withWriteSide returns rc, a wrapper of stream, extended with the write side
// of stream: its io.Writer and, only when stream has it, its CloseWrite. A
// stream that cannot be written is returned as rc.
func withWriteSide(rc, stream io.ReadCloser) io.ReadCloser {
	w, ok := stream.(io.Writer)
	if !ok {
		return rc
	}

	if cw, ok := stream.(closeWriter); ok {
		return &writeSideCW{ReadCloser: rc, Writer: w, cw: cw}
	}

	return &writeSide{ReadCloser: rc, Writer: w}
}

// readSide returns the wrapper withWriteSide extended, or body itself.
func readSide(body io.ReadCloser) io.ReadCloser {
	switch b := body.(type) {
	case *writeSide:
		return b.ReadCloser
	case *writeSideCW:
		return b.ReadCloser
	}

	return body
}

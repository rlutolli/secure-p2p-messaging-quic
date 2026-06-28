package main

import (
	"io"
	"net"

	"github.com/quic-go/quic-go"
)

type PeerConnection interface {
	io.Reader
	io.Writer
	Close() error
	RemoteAddr() net.Addr
}

type QuicConnectionWrapper struct {
	Stream *quic.Stream
	Conn   *quic.Conn
}

func (w *QuicConnectionWrapper) Read(p []byte) (n int, err error) {
	return w.Stream.Read(p)
}

func (w *QuicConnectionWrapper) Write(p []byte) (n int, err error) {
	return w.Stream.Write(p)
}

func (w *QuicConnectionWrapper) Close() error {
	// Close the stream gracefully. This sends a FIN and reliably delivers
	// all buffered data. We intentionally do NOT close the QUIC connection
	// with CloseWithError here — that would send a CONNECTION_CLOSE frame
	// which can cause unacknowledged stream data to be lost (RFC 9000 §10.2.2).
	// The QUIC connection will be cleaned up by idle timeout (60s) or when
	// the peer closes its side.
	return w.Stream.Close()
}

func (w *QuicConnectionWrapper) RemoteAddr() net.Addr {
	return w.Conn.RemoteAddr()
}

type NetConnWrapper struct {
	Conn net.Conn
}

func (w *NetConnWrapper) Read(p []byte) (n int, err error) {
	return w.Conn.Read(p)
}

func (w *NetConnWrapper) Write(p []byte) (n int, err error) {
	return w.Conn.Write(p)
}

func (w *NetConnWrapper) Close() error {
	return w.Conn.Close()
}

func (w *NetConnWrapper) RemoteAddr() net.Addr {
	return w.Conn.RemoteAddr()
}

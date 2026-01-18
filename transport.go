package main

import (
	"io"
	"net"

	"github.com/quic-go/quic-go"
)

// PeerConnection abstracts over QUIC streams and TCP connections
type PeerConnection interface {
	io.Reader
	io.Writer
	Close() error
	RemoteAddr() net.Addr
}

// QuicConnectionWrapper wraps a QUIC stream and its connection to satisfy PeerConnection
type QuicConnectionWrapper struct {
	Stream  *quic.Stream
	Session *quic.Conn // Keep reference to close session if needed, though usually we close stream
	Conn    *quic.Conn
}

func (w *QuicConnectionWrapper) Read(p []byte) (n int, err error) {
	return w.Stream.Read(p)
}

func (w *QuicConnectionWrapper) Write(p []byte) (n int, err error) {
	return w.Stream.Write(p)
}

func (w *QuicConnectionWrapper) Close() error {
	// Close the stream. In QUIC, closing the stream doesn't close the underlying connection,
	// but for our chat simplified model, one stream per peer usually suffices.
	// However, if we want to kill the connection entirely, we might need to access Session.
	// For this interface, closing the stream is the primary "close" action for I/O.
	// If we want to hard close the connection, we check implementation details elsewhere.
	// Let's just cancel the write side of the stream which sends FIN.
	return w.Stream.Close()
}

func (w *QuicConnectionWrapper) RemoteAddr() net.Addr {
	return w.Conn.RemoteAddr()
}

// NetConnWrapper wraps a standard net.Conn (TCP)
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

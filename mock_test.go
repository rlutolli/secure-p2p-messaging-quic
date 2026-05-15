package main

import (
	"io"
	"net"
	"sync"
)

type mockConn struct {
	mu       sync.Mutex
	written  []byte
	closed   bool
	readData []byte
	readPos  int
	remote   net.Addr
}

func newMockConn(remote net.Addr) *mockConn {
	return &mockConn{remote: remote}
}

func (m *mockConn) Read(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readPos >= len(m.readData) {
		return 0, io.EOF
	}
	n := copy(p, m.readData[m.readPos:])
	m.readPos += n
	return n, nil
}

func (m *mockConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.written = append(m.written, p...)
	return len(p), nil
}

func (m *mockConn) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockConn) RemoteAddr() net.Addr {
	return m.remote
}

func (m *mockConn) setReadData(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readData = data
	m.readPos = 0
}

func (m *mockConn) getWritten() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]byte(nil), m.written...)
}

func (m *mockConn) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

package main

import (
	"io"
	"net"
	"testing"
)

func TestNetConnWrapperReadWrite(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	w := &NetConnWrapper{Conn: c}

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 5)
		n, err := s.Read(buf)
		if err != nil {
			t.Errorf("server read error: %v", err)
			return
		}
		if string(buf[:n]) != "hello" {
			t.Errorf("expected hello, got %s", string(buf[:n]))
		}
	}()

	n, err := w.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n != 5 {
		t.Errorf("expected 5 bytes written, got %d", n)
	}
	<-done
}

func TestNetConnWrapperClose(t *testing.T) {
	t.Parallel()
	c, s := net.Pipe()
	w := &NetConnWrapper{Conn: c}

	if err := w.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	_, err := s.Read([]byte{0})
	if err != io.EOF && err != io.ErrClosedPipe {
		t.Errorf("expected EOF after close, got %v", err)
	}
	s.Close()
}

func TestNetConnWrapperRemoteAddr(t *testing.T) {
	t.Parallel()
	c, _ := net.Pipe()
	defer c.Close()

	w := &NetConnWrapper{Conn: c}
	if w.RemoteAddr() == nil {
		t.Fatal("expected non-nil RemoteAddr")
	}
	if w.RemoteAddr().String() != c.RemoteAddr().String() {
		t.Errorf("RemoteAddr mismatch: got %s, want %s", w.RemoteAddr().String(), c.RemoteAddr().String())
	}
}

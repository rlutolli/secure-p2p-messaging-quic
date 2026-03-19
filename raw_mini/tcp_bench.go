package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run tcp_bench.go [server|client] [addr] [port]")
		return
	}
	mode := os.Args[1]
	port := "9000"
	if mode == "server" {
		if len(os.Args) >= 3 { port = os.Args[2] }
		runServer(port)
	} else {
		addr := "127.0.0.1"
		if len(os.Args) >= 3 { addr = os.Args[2] }
		if len(os.Args) >= 4 { port = os.Args[3] }
		runClient(addr, port)
	}
}

func runServer(port string) {
	l, _ := net.Listen("tcp", ":"+port)
	fmt.Printf("TCP Server on :%s\n", port)
	for {
		conn, _ := l.Accept()
		go func(c net.Conn) {
			defer c.Close()
			io.Copy(io.Discard, c)
		}(conn)
	}
}

func runClient(addr, port string) {
	start := time.Now()
	conn, err := net.Dial("tcp", net.JoinHostPort(addr, port))
	handshake := time.Since(start)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer conn.Close()

	fmt.Printf("TCP Handshake: %v\n", handshake)

	// Throughput: Send 10MB
	data := make([]byte, 1024*1024*10)
	start = time.Now()
	conn.Write(data)
	duration := time.Since(start)
	fmt.Printf("TCP 10MB Send: %v\n", duration)
}

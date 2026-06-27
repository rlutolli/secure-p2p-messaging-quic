// bulk_warm.go - diagnostic: is quic-go's loopback bulk limit slow-start ramp or steady state?
// Connects to an existing quic_bench server (ALPN "bench"), opens ONE stream, writes 10 MB
// (cold, fresh cwnd) then 10 MB again (warm, ramped cwnd) on the SAME connection, timing each.
// If warm << cold, the limit is congestion-control slow-start, not CPU/syscalls.
//
//	go run bulk_warm.go 127.0.0.1:9001
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"time"

	"github.com/quic-go/quic-go"
)

func main() {
	addr := "127.0.0.1:9001"
	if len(os.Args) >= 2 {
		addr = os.Args[1]
	}
	ctx := context.Background()
	tlsConf := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"bench"}}
	qconf := &quic.Config{
		InitialStreamReceiveWindow:     10 * 1024 * 1024,
		InitialConnectionReceiveWindow: 15 * 1024 * 1024,
		MaxStreamReceiveWindow:         64 * 1024 * 1024,
		MaxConnectionReceiveWindow:     64 * 1024 * 1024,
		MaxIdleTimeout:                 30 * time.Second,
	}
	conn, err := quic.DialAddr(ctx, addr, tlsConf, qconf)
	if err != nil {
		fmt.Printf("dial: %v\n", err)
		return
	}
	defer conn.CloseWithError(0, "")
	s, err := conn.OpenStreamSync(ctx)
	if err != nil {
		fmt.Printf("stream: %v\n", err)
		return
	}
	data := make([]byte, 10*1024*1024)

	t := time.Now()
	s.Write(data)
	cold := time.Since(t)

	t = time.Now()
	s.Write(data)
	warm := time.Since(t)

	s.Close()
	mbps := func(d time.Duration) float64 { return 10.0 / d.Seconds() }
	fmt.Printf("cold 10MB: %v (%.0f MB/s)\n", cold, mbps(cold))
	fmt.Printf("warm 10MB: %v (%.0f MB/s)\n", warm, mbps(warm))
}

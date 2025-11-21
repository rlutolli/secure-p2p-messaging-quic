package main

import (
	"context"
	"fmt"
	"log"

	"github.com/quic-go/quic-go"
)

func startServer(addr string) {
	tlsConfig := generateTLSConfig()

	listener, err := quic.ListenAddr(addr, tlsConfig, nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("🚀 Server listening on", addr)

	for {
		sess, err := listener.Accept(context.Background())
		if err != nil {
			log.Fatal(err)
		}

		go func() {
			stream, err := sess.AcceptStream(context.Background())
			if err != nil {
				return
			}
			fmt.Printf("✅ Accepted connection from %s\n", sess.RemoteAddr())

			// Echo logic
			buf := make([]byte, 1024)
			n, _ := stream.Read(buf)
			fmt.Printf("📩 Received: %s\n", buf[:n])
			stream.Write(buf[:n])
		}()
	}
}

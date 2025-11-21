package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"

	"github.com/quic-go/quic-go"
)

func runClient(addr string) {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-echo-example"},
	}

	fmt.Println("Connecting to server at", addr)

	conn, err := quic.DialAddr(context.Background(), addr, tlsConf, nil)
	if err != nil {
		log.Fatal(err)
	}

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		log.Fatal(err)
	}

	message := "Hello QUIC from Golang!"
	fmt.Printf("📤 Sending: '%s'\n", message)
	_, err = stream.Write([]byte(message))
	if err != nil {
		log.Fatal(err)
	}

	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("re: 📥 Server Responded: '%s'\n", buf[:n])
}

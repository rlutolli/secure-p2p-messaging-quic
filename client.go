package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"strings"

	"github.com/quic-go/quic-go"
)

// sendMessage connects to a peer and sends a message with room name
func sendMessage(addr string, roomName string, message string) error {
	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"quic-echo-example"},
	}

	// Format message with room name: "ROOM:roomName|message content"
	formattedMsg := fmt.Sprintf("ROOM:%s|%s", roomName, message)

	conn, err := quic.DialAddr(context.Background(), addr, tlsConf, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %v", addr, err)
	}
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to open stream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Write([]byte(formattedMsg))
	if err != nil {
		return fmt.Errorf("failed to send message: %v", err)
	}

	// Read response (echo/broadcast from server)
	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		// It should be okay if we don't get a response
		return nil
	}

	response := strings.TrimSpace(string(buf[:n]))
	if response != "" {
		fmt.Printf("Response from %s: %s\n", addr, response)
	}

	return nil
}

// runClient is kept for backward compatibility but is not used in main flow
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
	fmt.Printf("Sending: '%s'\n", message)
	_, err = stream.Write([]byte(message))
	if err != nil {
		log.Fatal(err)
	}

	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("re: Server Responded: '%s'\n", buf[:n])
}

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type App struct {
	server      *Server
	discovery   *DiscoveryService
	connManager *ConnectionManager
	roomName    string
	isPrivate   bool
}

func main() {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("╔════════════════════════════════════════════╗")
	fmt.Println("║  Secure P2P Messenger (v0.1)               ║")
	fmt.Println("╚════════════════════════════════════════════╝")

	// 1. Get room configuration
	fmt.Print("Enter Room Name (or 'private' for hidden mode): ")
	roomName, _ := reader.ReadString('\n')
	roomName = strings.TrimSpace(roomName)

	if roomName == "" {
		roomName = "default"
	}

	isPrivate := strings.ToLower(roomName) == "private"

	// 2. Initialize application
	app, err := initializeApp(roomName, isPrivate)
	if err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown()

	// 3. Handle glorious shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		app.shutdown()
		os.Exit(0)
	}()

	// 4. Print status
	fmt.Printf("\n[Started] Port: %d | Room: %s | Private: %v\n",
		app.server.Port(), roomName, isPrivate)
	fmt.Println("\nCommands:")
	fmt.Println("  send <message>     - Send to all peers in room")
	fmt.Println("  peers              - List discovered peers")
	fmt.Println("  connect <ip:port>  - Connect to specific peer")
	fmt.Println("  quit               - Exit")
	fmt.Println()

	// 5. Interactive CLI
	app.runCLI()
}

func initializeApp(roomName string, isPrivate bool) (*App, error) {
	app := &App{
		roomName:  roomName,
		isPrivate: isPrivate,
	}

	// Message handler callback
	onMessage := func(from, room, message string) {
		fmt.Printf("\n[%s@%s]: %s\n> ", from, room, message)
	}

	// Start server
	server, err := NewServer("0.0.0.0:0", onMessage)
	if err != nil {
		return nil, fmt.Errorf("server start failed: %w", err)
	}
	app.server = server

	// Initialize connection manager
	app.connManager = NewConnectionManager(server.Port(), roomName)

	// Start discovery (if not private)
	if !isPrivate {
		time.Sleep(100 * time.Millisecond) // Give time for the server to bind

		discovery, err := NewDiscoveryService(server.Port(), roomName)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("discovery start failed: %w", err)
		}
		app.discovery = discovery
	}

	return app, nil
}

func (app *App) runCLI() {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		cmd := strings.ToLower(parts[0])

		switch cmd {
		case "send":
			if len(parts) < 2 {
				fmt.Println("Usage: send <message>")
				break
			}
			app.sendToRoom(parts[1])

		case "peers":
			app.listPeers()

		case "connect":
			if len(parts) < 2 {
				fmt.Println("Usage: connect <ip:port>")
				break
			}
			// Parse: connect ip:port [message]
			connectParts := strings.SplitN(parts[1], " ", 2)
			addr := connectParts[0]
			msg := ""
			if len(connectParts) > 1 {
				msg = connectParts[1]
			}
			app.connectTo(addr, msg)

		case "quit", "exit":
			return

		default:
			fmt.Println("Unknown command. Try: send, peers, connect, quit")
		}

		fmt.Print("> ")
	}
}

func (app *App) sendToRoom(message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Discover peers
	var peers []string
	if app.discovery != nil {
		var err error
		peers, err = app.discovery.LookupPeers(ctx)
		if err != nil {
			fmt.Printf("Discovery error: %v\n", err)
		}
	}

	if len(peers) == 0 {
		fmt.Println("No peers found in room.")
		return
	}

	fmt.Printf("Found %d peer(s). Sending...\n", len(peers))

	for _, addr := range peers {
		if err := app.connManager.Send(ctx, addr, message); err != nil {
			fmt.Printf("  ✗ %s: %v\n", addr, err)
		} else {
			fmt.Printf("  ✓ %s\n", addr)
		}
	}
}

func (app *App) listPeers() {
	if app.discovery == nil {
		fmt.Println("Discovery disabled (private mode)")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	fmt.Println("Searching for peers...")
	peers, err := app.discovery.LookupPeers(ctx)
	if err != nil {
		fmt.Printf("Discovery error: %v\n", err)
		return
	}

	if len(peers) == 0 {
		fmt.Println("No peers found.")
		return
	}

	fmt.Printf("Found %d peer(s):\n", len(peers))
	for _, addr := range peers {
		fmt.Printf("  • %s\n", addr)
	}
}

func (app *App) connectTo(addr, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if message == "" {
		message = "Hello!"
	}

	if err := app.connManager.Send(ctx, addr, message); err != nil {
		fmt.Printf("Connection failed: %v\n", err)
	} else {
		fmt.Printf("Connected to %s\n", addr)
	}
}

func (app *App) shutdown() {
	if app.discovery != nil {
		app.discovery.Shutdown()
	}
	if app.connManager != nil {
		app.connManager.Close()
	}
	if app.server != nil {
		app.server.Close()
	}
}

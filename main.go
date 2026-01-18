/*
Package main implements a secure peer-to-peer messaging application using QUIC or TCP.

Secure P2P Messenger is a decentralized chat application that enables real-time text
messaging between peers on a Local Area Network (LAN). Leverages QUIC or TCP for transport-layer
security (TLS 1.3) and uses UDP broadcasts for automatic peer discovery.

Key Features:
  - Automatic LAN peer discovery via UDP broadcasts
  - Room-based chat with logical peer grouping
  - Bidirectional message relay (mesh networking)
  - TLS 1.3 encryption (QUIC or TCP)
  - Persistent connection pooling for efficiency
  - Private mode to hide from discovery

Architecture Overview:
  - main.go:               Application entry point, CLI, and orchestration (this file)
  - server.go:             QUIC/TCP server for incoming connections and room management
  - connection_manager.go: Outgoing connection pool/dialer
  - transort.go:           Connection abstraction interface
  - discovery.go:          UDP broadcast-based LAN peer discovery
  - security.go:           TLS certificate generation (ECDSA P-256)
  - client.go:             Legacy client functions for backward compatibility

Usage:

	./p2p-messenger [--debug | -d] [--use-tcp | -t]

Commands:
  - Type any text to send to all peers in the room
  - /help      - Show available commands
  - /peers     - List connected peers
  - /connect   - Connect to a specific peer
  - /room      - Show room info
  - exit/quit  - Exit application
*/
package main

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// debugMode controls verbose logging output. When false, log output is discarded.
// Enable with --debug or -d command-line flag.
var debugMode = false

// ANSI color codes for usernames (shared with connection_manager.go)
const (
	colorReset   = "\033[0m"
	colorRed     = "\033[1;31m"
	colorGreen   = "\033[1;32m"
	colorYellow  = "\033[1;33m"
	colorBlue    = "\033[1;34m"
	colorMagenta = "\033[1;35m"
	colorCyan    = "\033[1;36m"
)

var usernameColors = []string{colorRed, colorGreen, colorYellow, colorBlue, colorMagenta, colorCyan}

// getUsernameColor returns a consistent color for a username based on hash
func getUsernameColor(username string) string {
	h := fnv.New32a()
	h.Write([]byte(username))
	idx := h.Sum32() % uint32(len(usernameColors))
	return usernameColors[idx]
}

// formatMessage formats a message with colored username: <Username> message
func formatMessage(username, message string) string {
	color := getUsernameColor(username)
	return fmt.Sprintf("%s<%s>%s %s", color, username, colorReset, message)
}

// formatSystemMessage formats a system message with [System] in yellow
func formatSystemMessage(message string) string {
	return fmt.Sprintf("%s[System]%s %s", colorYellow, colorReset, message)
}

// logColored prints a log message with colored port (only in debug mode)
func logColored(format string, args ...interface{}) {
	if debugMode {
		log.Printf(format, args...)
	}
}

// colorPort returns a port number with cyan color
func colorPort(port int) string {
	return fmt.Sprintf("%s%d%s", colorCyan, port, colorReset)
}

type App struct {
	server      *Server
	discovery   *DiscoveryService
	connManager *ConnectionManager
	roomName    string
	isPrivate   bool
	useTCP      bool
}

// Spinner for discovery progress
var spinnerChars = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

func runSpinner(done chan bool, message string) {
	i := 0
	for {
		select {
		case <-done:
			// Clear the spinner line
			fmt.Printf("\r%s\r", strings.Repeat(" ", len(message)+5))
			return
		default:
			fmt.Printf("\r%s %c ", message, spinnerChars[i%len(spinnerChars)])
			i++
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func main() {
	var useTCP bool

	// Parse command line arguments
	for _, arg := range os.Args[1:] {
		if arg == "--debug" || arg == "-d" {
			debugMode = true
		}
		if arg == "--use-tcp" || arg == "-t" {
			useTCP = true
		}
	}

	// If not in debug mode, suppress log output
	if !debugMode {
		log.SetOutput(io.Discard)
	}

	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("Secure P2P Messenger (v0.3 - TCP Support: %v)\n", useTCP)

	// 1. Discover available rooms with spinner (UDP is fast, 4 second timeout)
	done := make(chan bool)
	go runSpinner(done, "Scanning LAN for rooms")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	rooms, _ := DiscoverAllRooms(ctx)
	cancel()

	done <- true                      // Stop spinner
	time.Sleep(50 * time.Millisecond) // Let spinner cleanup
	fmt.Println()                     // New line after discovery

	var roomName string
	var isPrivate bool
	var peersToConnect []string // Peers to auto-connect to

	if len(rooms) > 0 {
		fmt.Println("\nAvailable rooms:")
		for i, room := range rooms {
			fmt.Printf("  %s%d%s. %s (%d peer(s))\n", colorCyan, i+1, colorReset, room.Name, len(room.Peers))
		}
		fmt.Println("  n. Create new room")	
		fmt.Println("  p. Private mode (hidden)")
		fmt.Print("\nSelect option: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		if choice == "n" || choice == "N" {
			fmt.Print("Enter new room name: ")
			roomName, _ = reader.ReadString('\n')
			roomName = strings.TrimSpace(roomName)
			if roomName == "" {
				roomName = "default"
			}
			isPrivate = false
		} else if choice == "p" || choice == "P" {
			roomName = "private"
			isPrivate = true
		} else {
			// Try to parse as number
			var idx int
			if _, err := fmt.Sscanf(choice, "%d", &idx); err == nil && idx > 0 && idx <= len(rooms) {
				roomName = rooms[idx-1].Name
				peersToConnect = rooms[idx-1].Peers // Store peers for auto-connect
				isPrivate = false
			} else {
				// Treat as room name
				roomName = choice
				isPrivate = false
			}
		}
	} else {
		// No rooms found, ask for room name
		fmt.Print("\nNo rooms found. Enter room name (or 'private' for hidden mode): ")
		roomName, _ = reader.ReadString('\n')
		roomName = strings.TrimSpace(roomName)
		if roomName == "" {
			roomName = "default"
		}
		isPrivate = strings.ToLower(roomName) == "private"
	}

	// 2. Initialize application
	app, err := initializeApp(roomName, isPrivate, useTCP)
	if err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown()

	// 3. Auto-connect to discovered peers (if any)
	if len(peersToConnect) > 0 {
		fmt.Printf("Connecting to %d peer(s)...\n", len(peersToConnect))
		connectCtx, connectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		connectedCount := 0
		for _, peerAddr := range peersToConnect {
			_, err := app.connManager.GetOrCreate(connectCtx, peerAddr)
			if err == nil {
				connectedCount++
			}
		}
		connectCancel()
		if connectedCount > 0 {
			fmt.Printf("Connected to %s%d%s peer(s)\n", colorGreen, connectedCount, colorReset)
		}
	}

	// 4. Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		app.shutdown()
		os.Exit(0)
	}()

	// 5. Print status and show commands
	fmt.Printf("\nStarted | Port: %s%d%s | Room: %s | You: %s%s%s | TCP: %v\n",
		colorCyan, app.server.Port(), colorReset,
		roomName,
		colorGreen, app.connManager.GetLocalAlias(), colorReset,
		useTCP)
	app.showHelp()

	// 6. Interactive CLI
	app.runCLI()
}

func initializeApp(roomName string, isPrivate bool, useTCP bool) (*App, error) {
	app := &App{
		roomName:  roomName,
		isPrivate: isPrivate,
		useTCP:    useTCP,
	}

	// Initialize connection manager first (needed by server)
	app.connManager = NewConnectionManager(0, roomName, useTCP) // Port will be set after server starts

	// Message handler callback - displays messages received via Server (incoming connections)
	onMessage := func(from, room, message string) {
		// Format and display with colored username
		formatted := formatMessage(from, message)
		fmt.Printf("\n%s\n> ", formatted)
	}

	// System message callback - displays join/leave messages locally
	onSystemMessage := func(message string) {
		formatted := formatSystemMessage(message)
		fmt.Printf("\n%s\n> ", formatted)
	}

	// Start server (pass ConnectionManager for bidirectional messaging)
	// Server listens on both TCP and QUIC always
	server, err := NewServer("0.0.0.0:0", onMessage, onSystemMessage, app.connManager)
	if err != nil {
		return nil, fmt.Errorf("server start failed: %w", err)
	}
	app.server = server

	// Update connection manager with actual port
	app.connManager.localPort = server.Port()

	// Start discovery (if not private)
	if !isPrivate {
		time.Sleep(200 * time.Millisecond) // Give time for the server to bind

		discovery, err := NewDiscoveryService(server.Port(), roomName)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("discovery start failed: %w", err)
		}
		app.discovery = discovery

		// Give mDNS a moment to start advertising
		time.Sleep(300 * time.Millisecond)
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

		// Handle "exit" or "quit" typed alone (without /)
		if line == "exit" || line == "quit" {
			fmt.Println("Bye")
			return
		}

		// Check if it's a command (starts with /)
		if strings.HasPrefix(line, "/") {
			// Remove the / and parse command
			cmdLine := strings.TrimPrefix(line, "/")
			parts := strings.SplitN(cmdLine, " ", 2)
			cmd := strings.ToLower(parts[0])

			switch cmd {
			case "help", "?":
				app.showHelp()

			case "peers":
				app.listPeers()

			case "connect":
				if len(parts) < 2 {
					fmt.Println("Usage: /connect <ip:port> [message]")
				} else {
					// Parse: connect ip:port [message]
					connectParts := strings.SplitN(parts[1], " ", 2)
					addr := connectParts[0]
					msg := ""
					if len(connectParts) > 1 {
						msg = connectParts[1]
					}
					app.connectTo(addr, msg)
				}

			case "room", "rooms":
				app.showRoomInfo()

			case "quit", "exit":
				fmt.Println("Bye")
				return

			default:
				fmt.Printf("Unknown command: /%s. Type /help for available commands.\n", cmd)
			}
		} else {
			// Raw text - auto-send as message
			app.sendToRoom(line)
		}

		fmt.Print("> ")
	}
}

func (app *App) sendToRoom(message string) {
	// PRIORITY 1: Use existing connections (fast, no discovery delay)
	app.connManager.mu.RLock()
	connectedPeers := make([]string, 0, len(app.connManager.connections))
	for addr := range app.connManager.connections {
		connectedPeers = append(connectedPeers, addr)
	}
	app.connManager.mu.RUnlock()

	var peers []string
	peers = append(peers, connectedPeers...)

	// PRIORITY 2: Only do discovery if no existing connections (background, non-blocking)
	if len(peers) == 0 && app.discovery != nil {
		// Quick discovery with short timeout
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		discovered, err := app.discovery.LookupPeers(ctx)
		cancel()

		if err == nil {
			peers = append(peers, discovered...)
		}
		// Don't show discovery errors - they're expected if no peers are available
	}

	if len(peers) == 0 {
		fmt.Println("No peers connected. Use /connect <ip:port> to connect to a peer.")
		return
	}

	// Send immediately to all peers (no verbose output for performance)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	successCount := 0
	for _, addr := range peers {
		if err := app.connManager.Send(ctx, addr, message); err != nil {
			// Silent failure for performance - connection will be retried next time
		} else {
			successCount++
		}
	}

	// Only show error if ALL failed
	if successCount == 0 {
		fmt.Println("Failed to send message. Peers may be offline.")
	}
}

func (app *App) listPeers() {
	// Show connected peers from ConnectionManager
	app.connManager.mu.RLock()
	connectedCount := len(app.connManager.connections)
	connections := make([]string, 0, connectedCount)
	for addr := range app.connManager.connections {
		connections = append(connections, addr)
	}
	app.connManager.mu.RUnlock()

	if connectedCount > 0 {
		fmt.Printf("Connected peers (%d):\n", connectedCount)
		for _, addr := range connections {
			fmt.Printf("  • %s%s%s\n", colorCyan, addr, colorReset)
		}
	} else {
		fmt.Println("No peers connected. Use /connect <ip:port> to connect.")
	}
}

func (app *App) connectTo(addr, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Connect (this SHOULD send JOIN: handshake automatically)
	_, err := app.connManager.GetOrCreate(ctx, addr)
	if err != nil {
		fmt.Printf("Connection failed: %v\n", err)
		return
	}

	fmt.Printf("Connected to %s\n", addr)

	// Send message if provided
	if message != "" {
		if err := app.connManager.Send(ctx, addr, message); err != nil {
			fmt.Printf("Failed to send message: %v\n", err)
		}
	}
}

func (app *App) showHelp() {
	fmt.Println("\nCommands:")
	fmt.Println("  Type any message to send it automatically")
	fmt.Println("  /help            - Show this help")
	fmt.Println("  /peers           - List connected peers")
	fmt.Println("  /connect <addr>  - Connect to peer")
	fmt.Println("  /room            - Show room info")
	fmt.Println("  exit or quit     - Exit application")
}

func (app *App) showRoomInfo() {
	fmt.Printf("\nRoom: %s\n", app.roomName)
	fmt.Printf("Your alias: %s%s%s\n", colorGreen, app.connManager.GetLocalAlias(), colorReset)
	fmt.Printf("Port: %s%d%s\n", colorCyan, app.server.Port(), colorReset)

	// Show connected peers count
	app.connManager.mu.RLock()
	connectedCount := len(app.connManager.connections)
	app.connManager.mu.RUnlock()
	fmt.Printf("Connected peers: %d\n", connectedCount)
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

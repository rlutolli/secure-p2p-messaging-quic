/*
Package main implements a secure peer-to-peer messaging application using QUIC or TCP.

Usage:

	./p2p-messenger [--debug | -d] [--use-tcp | -t] [--rendezvous <url>] [--discovery-port <n>]

Commands:
  - Type any text to send to all peers in the room
  - /help            - Show available commands
  - /peers           - List connected peers
  - /connect <addr>  - Connect to a specific peer
  - /room            - Show room info
  - /myip            - Show your public IP via STUN
  - exit/quit        - Exit application
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

	"github.com/pion/stun/v3"
)

var debugMode = false

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

func getUsernameColor(username string) string {
	h := fnv.New32a()
	h.Write([]byte(username))
	idx := h.Sum32() % uint32(len(usernameColors))
	return usernameColors[idx]
}

func formatMessage(username, message string) string {
	color := getUsernameColor(username)
	return fmt.Sprintf("%s<%s>%s %s", color, username, colorReset, message)
}

func formatSystemMessage(message string) string {
	return fmt.Sprintf("%s[System]%s %s", colorYellow, colorReset, message)
}

func colorPort(port int) string {
	return fmt.Sprintf("%s%d%s", colorCyan, port, colorReset)
}

type App struct {
	server        *Server
	discovery     *DiscoveryService
	connManager   *ConnectionManager
	roomName      string
	isPrivate     bool
	useTCP        bool
	rendezvousURL string
}

var spinnerChars = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

func runSpinner(done chan bool, message string) {
	i := 0
	for {
		select {
		case <-done:
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
	var rendezvousURL string
	discPort := 19999

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--debug", "-d":
			debugMode = true
		case "--use-tcp", "-t":
			useTCP = true
		case "--rendezvous":
			if i+1 < len(args) {
				i++
				rendezvousURL = args[i]
			}
		case "--discovery-port":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &discPort)
			}
		}
	}

	if !debugMode {
		log.SetOutput(io.Discard)
	}

	CheckUDPBuffers()

	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("Secure P2P Messenger (v0.4 — TCP: %v)\n", useTCP)

	done := make(chan bool)
	go runSpinner(done, "Scanning LAN for rooms")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	rooms, _ := DiscoverAllRooms(ctx, discPort)
	cancel()

	done <- true
	time.Sleep(50 * time.Millisecond)
	fmt.Println()

	var roomName string
	var isPrivate bool
	var peersToConnect []string

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

		switch choice {
		case "n", "N":
			fmt.Print("Enter new room name: ")
			roomName, _ = reader.ReadString('\n')
			roomName = strings.TrimSpace(roomName)
			if roomName == "" {
				roomName = "default"
			}
			isPrivate = false
		case "p", "P":
			roomName = "private"
			isPrivate = true
		default:
			var idx int
			if _, err := fmt.Sscanf(choice, "%d", &idx); err == nil && idx > 0 && idx <= len(rooms) {
				roomName = rooms[idx-1].Name
				peersToConnect = rooms[idx-1].Peers
				isPrivate = false
			} else {
				roomName = choice
				isPrivate = false
			}
		}
	} else {
		fmt.Print("\nNo rooms found. Enter room name (or 'private' for hidden mode): ")
		roomName, _ = reader.ReadString('\n')
		roomName = strings.TrimSpace(roomName)
		if roomName == "" {
			roomName = "default"
		}
		isPrivate = strings.ToLower(roomName) == "private"
	}

	if rendezvousURL != "" {
		rCtx, rCancel := context.WithTimeout(context.Background(), 4*time.Second)
		rPeers, err := FetchPeers(rCtx, rendezvousURL, roomName)
		rCancel()
		if err == nil {
			peersToConnect = append(peersToConnect, rPeers...)
		}
	}

	app, err := initializeApp(roomName, isPrivate, useTCP, discPort, rendezvousURL)
	if err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown()

	if len(peersToConnect) > 0 {
		fmt.Printf("Connecting to %d peer(s)...\n", len(peersToConnect))
		connectCtx, connectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		connectedCount := 0
		for _, peerAddr := range peersToConnect {
			if _, err := app.connManager.GetOrCreate(connectCtx, peerAddr); err == nil {
				connectedCount++
			}
		}
		connectCancel()
		if connectedCount > 0 {
			fmt.Printf("Connected to %s%d%s peer(s)\n", colorGreen, connectedCount, colorReset)
		}
	}

	if rendezvousURL != "" && !isPrivate {
		publicAddr := fmt.Sprintf("?:%d", app.server.Port())
		if ip := resolvePublicIP(); ip != "" {
			publicAddr = fmt.Sprintf("%s:%d", ip, app.server.Port())
		}
		go func() {
			for {
				rCtx, rCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := RenewRegistration(rCtx, rendezvousURL, roomName, publicAddr); err != nil {
					log.Printf("[Rendezvous] Registration failed: %v", err)
				}
				rCancel()
				time.Sleep(30 * time.Second)
			}
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		app.shutdown()
		os.Exit(0)
	}()

	fmt.Printf("\nStarted | Port: %s%d%s | Room: %s | You: %s%s%s | TCP: %v\n",
		colorCyan, app.server.Port(), colorReset,
		roomName,
		colorGreen, app.connManager.GetLocalAlias(), colorReset,
		useTCP)
	app.showHelp()

	app.runCLI()
}

func initializeApp(roomName string, isPrivate bool, useTCP bool, discPort int, rendezvousURL string) (*App, error) {
	app := &App{
		roomName:      roomName,
		isPrivate:     isPrivate,
		useTCP:        useTCP,
		rendezvousURL: rendezvousURL,
	}

	app.connManager = NewConnectionManager(0, roomName, useTCP)

	onMessage := func(from, room, message string) {
		fmt.Printf("\n%s\n> ", formatMessage(from, message))
	}

	onSystemMessage := func(message string) {
		fmt.Printf("\n%s\n> ", formatSystemMessage(message))
	}

	server, err := NewServer("0.0.0.0:0", onMessage, onSystemMessage, app.connManager)
	if err != nil {
		return nil, fmt.Errorf("server start failed: %w", err)
	}
	app.server = server

	app.connManager.localPort = server.Port()

	if !isPrivate {
		time.Sleep(200 * time.Millisecond)

		discovery, err := NewDiscoveryService(server.Port(), roomName, discPort)
		if err != nil {
			server.Close()
			return nil, fmt.Errorf("discovery start failed: %w", err)
		}
		app.discovery = discovery

		time.Sleep(300 * time.Millisecond)
	}

	return app, nil
}

func (app *App) runCLI() {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Print("> ")

	noArgCommands := map[string]bool{
		"help":  true,
		"?":     true,
		"peers": true,
		"room":  true,
		"rooms": true,
		"myip":  true,
		"exit":  true,
		"quit":  true,
		"q":     true,
		"bye":   true,
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}

		// Normalize: if the bare word is a known no-arg command, treat it as /command.
		normalized := strings.ToLower(strings.SplitN(line, " ", 2)[0])
		isSlashCmd := strings.HasPrefix(line, "/")
		if !isSlashCmd && noArgCommands[normalized] {
			line = "/" + line
		}

		if strings.HasPrefix(line, "/") {
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

			case "myip":
				app.showPublicIP()

			case "exit", "quit", "q", "bye":
				fmt.Println("Bye")
				return

			default:
				fmt.Printf("Unknown command: /%s. Type /help for available commands.\n", cmd)
			}
		} else {
			app.sendToRoom(line)
		}

		fmt.Print("> ")
	}
}

func (app *App) sendToRoom(message string) {
	peers := app.connManager.ListConnected()

	if len(peers) == 0 && app.discovery != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		discovered, err := app.discovery.LookupPeers(ctx)
		cancel()

		if err == nil {
			peers = append(peers, discovered...)
		}
	}

	if len(peers) == 0 {
		fmt.Println("No peers connected. Use /connect <ip:port> to connect to a peer.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)

	successCount := 0
	for _, addr := range peers {
		if err := app.connManager.Send(ctx, addr, message); err == nil {
			successCount++
		}
	}
	cancel()

	if successCount == 0 {
		fmt.Println("Failed to send message. Peers may be offline.")
	}
}

func (app *App) listPeers() {
	connections := app.connManager.ListConnected()
	connectedCount := len(connections)

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

	if _, err := app.connManager.GetOrCreate(ctx, addr); err != nil {
		fmt.Printf("Connection failed: %v\n", err)
		return
	}

	fmt.Printf("Connected to %s\n", addr)

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
	fmt.Println("  /myip            - Show your public IP (STUN)")
	fmt.Println("  /exit or /quit   - Exit application")
}

func (app *App) showRoomInfo() {
	fmt.Printf("\nRoom: %s\n", app.roomName)
	fmt.Printf("Your alias: %s%s%s\n", colorGreen, app.connManager.GetLocalAlias(), colorReset)
	fmt.Printf("Port: %s%d%s\n", colorCyan, app.server.Port(), colorReset)
	fmt.Printf("Connected peers: %d\n", len(app.connManager.ListConnected()))
}

func (app *App) showPublicIP() {
	ip := resolvePublicIP()
	if ip == "" {
		fmt.Println("Could not determine public IP.")
		return
	}
	fmt.Printf("Public address: %s%s:%d%s\n", colorCyan, ip, app.server.Port(), colorReset)
}

func resolvePublicIP() string {
	conn, err := stun.Dial("udp4", "stun.l.google.com:19302")
	if err != nil {
		return ""
	}
	defer conn.Close()

	message, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		return ""
	}

	var publicAddr string
	if err := conn.Do(message, func(res stun.Event) {
		if res.Error != nil {
			return
		}
		var xorAddr stun.XORMappedAddress
		if err := xorAddr.GetFrom(res.Message); err != nil {
			return
		}
		publicAddr = xorAddr.IP.String()
	}); err != nil {
		return ""
	}

	return publicAddr
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

/*
Package main implements a secure peer-to-peer messaging application using QUIC or TCP.

Usage:

	./p2p-messenger [--debug | -d] [--use-tcp | -t] [--rendezvous <url>] [--discovery-port <n>] [--relay] [--relay-port <n>] [--no-upnp] [--disable-gso] [--disable-ecn]

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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pion/stun/v3"
)

// parsePortArg validates a CLI-supplied port number (0-65535). A port of 0
// means "let the OS pick a free port" (the historical default).
func parsePortArg(s string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("parsePortArg: invalid port %q: %w", s, err)
	}
	if p < 0 || p > 65535 {
		return 0, fmt.Errorf("parsePortArg: port %d out of range 0-65535", p)
	}
	return p, nil
}

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
	roomPassword  string
	isPrivate     bool
	useTCP        bool
	rendezvousURL string
	isRelay       bool
	upnpCleanup   func()
	externalAddr  string
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
	var isRelay bool
	discPort := 19999
	relayPort := 0

	upnpEnabled := false

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
		case "--relay-port":
			if i+1 < len(args) {
				i++
				p, err := parsePortArg(args[i])
				if err != nil {
					fmt.Printf("Invalid --relay-port: %v\n", err)
					os.Exit(1)
				}
				relayPort = p
			}
		case "--relay":
			isRelay = true
			upnpEnabled = true
		case "--no-upnp":
			upnpEnabled = false
		case "--disable-gso":
			// WAN remedy: some paths silently drop GSO-coalesced UDP datagrams.
			// quic-go reads this env var when the socket is created.
			os.Setenv("QUIC_GO_DISABLE_GSO", "true")
		case "--disable-ecn":
			// WAN remedy: some paths drop ECN-marked datagrams.
			os.Setenv("QUIC_GO_DISABLE_ECN", "true")
		}
	}

	if !debugMode {
		log.SetOutput(io.Discard)
	}

	CheckUDPBuffers()

	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("Secure P2P Messenger (v0.4 — TCP: %v)\n", useTCP)

	done := make(chan bool, 1)
	go runSpinner(done, "Scanning LAN for rooms")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	rooms, _ := DiscoverAllRooms(ctx, discPort)
	cancel()

	done <- true
	time.Sleep(50 * time.Millisecond)
	fmt.Println()

	var roomName string
	var roomPassword string
	var isPrivate bool
	var peersToConnect []string
	var isCreatingRoom bool

	if len(rooms) > 0 {
		fmt.Println("\nAvailable rooms:")
		for i, room := range rooms {
			indicator := ""
			if room.HasPassword {
				indicator = " [password protected]"
			}
			fmt.Printf("  %s%d%s. %s (%d peer(s))%s\n", colorCyan, i+1, colorReset, room.Name, len(room.Peers), indicator)
		}
		fmt.Println("  n. Create new room")
		fmt.Println("  p. Private mode (hidden)")
		fmt.Print("\nSelect option: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		switch choice {
		case "n", "N":
			isCreatingRoom = true
			fmt.Print("Enter new room name: ")
			roomName, _ = reader.ReadString('\n')
			roomName = strings.TrimSpace(roomName)
			if roomName == "" {
				roomName = "default"
			}
			isPrivate = false
			fmt.Print("Set a password? (leave empty for none): ")
			password, _ := reader.ReadString('\n')
			roomPassword = strings.TrimSpace(password)
		case "p", "P":
			isCreatingRoom = true
			roomName = "private"
			isPrivate = true
		default:
			var idx int
			if _, err := fmt.Sscanf(choice, "%d", &idx); err == nil && idx > 0 && idx <= len(rooms) {
				roomName = rooms[idx-1].Name
				peersToConnect = rooms[idx-1].Peers
				isPrivate = false
				if rooms[idx-1].HasPassword {
					fmt.Print("Enter room password: ")
					password, _ := reader.ReadString('\n')
					roomPassword = strings.TrimSpace(password)
				}
			} else {
				roomName = choice
				isPrivate = false
			}
		}
	} else {
		isCreatingRoom = true
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

	app, err := initializeApp(roomName, isPrivate, useTCP, discPort, rendezvousURL, roomPassword, isRelay && isCreatingRoom, upnpEnabled, relayPort)
	if err != nil {
		fmt.Printf("Failed to start: %v\n", err)
		os.Exit(1)
	}
	defer app.shutdown()

	if len(peersToConnect) > 0 || (!app.isRelay && app.discovery != nil) {
		if !app.isRelay && app.discovery != nil {
			connectCtx, connectCancel := context.WithTimeout(context.Background(), 3*time.Second)
			relayPeers, _ := app.discovery.LookupRelays(connectCtx)
			connectCancel()
			if len(relayPeers) > 0 {
				peersToConnect = relayPeers
			} else {
				peersToConnect = nil
			}
		}
		if len(peersToConnect) > 0 {
			fmt.Printf("Connecting to %d peer(s)...\n", len(peersToConnect))
			connectCtx, connectCancel := context.WithTimeout(context.Background(), 5*time.Second)
			connectedCount := 0
			for _, peerAddr := range peersToConnect {
				if _, err := app.connManager.GetOrCreate(connectCtx, peerAddr); err == nil {
					if !app.isRelay {
						app.connManager.MarkRelay(peerAddr)
					}
					connectedCount++
				}
			}
			connectCancel()
			if connectedCount > 0 {
				fmt.Printf("Connected to %s%d%s peer(s)\n", colorGreen, connectedCount, colorReset)
			}
		}
	}

	if rendezvousURL != "" && !isPrivate {
		publicAddr := app.externalAddr
		if publicAddr == "" {
			publicAddr = fmt.Sprintf("?:%d", app.server.Port())
			if ip := resolvePublicIP(); ip != "" {
				publicAddr = fmt.Sprintf("%s:%d", ip, app.server.Port())
			}
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

func initializeApp(roomName string, isPrivate bool, useTCP bool, discPort int, rendezvousURL string, roomPassword string, isRelay bool, upnpEnabled bool, relayPort int) (*App, error) {
	app := &App{
		roomName:      roomName,
		roomPassword:  roomPassword,
		isPrivate:     isPrivate,
		useTCP:        useTCP,
		rendezvousURL: rendezvousURL,
		isRelay:       isRelay,
	}

	app.connManager = NewConnectionManager(0, roomName, useTCP, roomPassword, isRelay)

	onMessage := func(from, room, message string) {
		fmt.Printf("\n%s\n> ", formatMessage(from, message))
	}

	onSystemMessage := func(message string) {
		fmt.Printf("\n%s\n> ", formatSystemMessage(message))
	}

	// relayPort of 0 binds to an OS-assigned ephemeral port (historical
	// default). A fixed port keeps the relay reachable at a known address
	// across restarts, which benchmark orchestration relies on.
	bindAddr := fmt.Sprintf("0.0.0.0:%d", relayPort)
	server, err := NewServer(bindAddr, onMessage, onSystemMessage, app.connManager)
	if err != nil {
		return nil, fmt.Errorf("server start failed: %w", err)
	}
	app.server = server

	app.connManager.localPort = server.Port()

	if isRelay && upnpEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		externalAddr, cleanup, err := TryPortMapping(ctx, server.Port(), "p2p-messenger-relay")
		cancel()
		if err == nil && externalAddr != "" {
			app.externalAddr = externalAddr
			app.upnpCleanup = cleanup
			fmt.Printf("UPnP/NAT-PMP mapped port: %s\n", externalAddr)
		}
	}

	if !isPrivate {
		time.Sleep(200 * time.Millisecond)

		discovery, err := NewDiscoveryService(server.Port(), roomName, discPort, func() bool { return app.roomPassword != "" }, func() bool { return app.isRelay })
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
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	fmt.Print("> ")

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			fmt.Print("> ")
			continue
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

	if err := scanner.Err(); err != nil {
		fmt.Printf("\nInput error: %v\n", err)
	}
}

func (app *App) sendToRoom(message string) {
	var peers []string
	if app.isRelay {
		peers = app.connManager.ListConnected()
	} else {
		peers = app.connManager.GetRelayAddrs()
	}

	if len(peers) == 0 && app.discovery != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var discovered []string
		var err error
		if app.isRelay {
			discovered, err = app.discovery.LookupPeers(ctx)
		} else {
			discovered, err = app.discovery.LookupRelays(ctx)
		}
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
	defer cancel()

	successCount := 0
	for _, addr := range peers {
		if err := app.connManager.Send(ctx, addr, message); err == nil {
			successCount++
		}
	}

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
	if app.upnpCleanup != nil {
		app.upnpCleanup()
	}
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

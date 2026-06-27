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
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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
	server         *Server
	discovery      *DiscoveryService
	connManager    *ConnectionManager
	roomName       string
	roomPassword   string
	isPrivate      bool
	useTCP         bool
	rendezvousURL  string
	isRelay        bool
	isCreatingRoom bool
	upnpCleanup    func()
	externalAddr   string
	// userRequestedExit is set to true when the user types /exit or /quit.
	// runCLI returns, and the main loop checks this flag to decide whether
	// to break (exit program) or continue (go back to discovery).
	userRequestedExit bool
	// shutdownOnce ensures shutdown is idempotent across multiple calls.
	shutdownOnce sync.Once
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

// roomSelection holds the user's choice at the discovery/room-selection prompt.
type roomSelection struct {
	roomName       string
	roomPassword   string
	isPrivate      bool
	peersToConnect []string
	isCreatingRoom bool
	quit           bool
}

// promptRoomSelection runs the LAN discovery scan and shows the interactive
// room-selection menu. It returns the user's chosen room details or sets
// quit=true when the user selects 'q'.
func promptRoomSelection(reader *bufio.Reader, discPort int) roomSelection {
	done := make(chan bool, 1)
	go runSpinner(done, "Scanning LAN for rooms")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	rooms, _ := DiscoverAllRooms(ctx, discPort)
	cancel()

	done <- true
	time.Sleep(50 * time.Millisecond)
	fmt.Println()

	var result roomSelection

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
		fmt.Println("  q. Quit")
		fmt.Print("\nSelect option: ")

		choice, _ := reader.ReadString('\n')
		choice = strings.TrimSpace(choice)

		switch choice {
		case "q", "Q":
			result.quit = true
			return result
		case "n", "N":
			result.isCreatingRoom = true
			fmt.Print("Enter new room name: ")
			result.roomName, _ = reader.ReadString('\n')
			result.roomName = strings.TrimSpace(result.roomName)
			if result.roomName == "" {
				result.roomName = "default"
			}
			result.isPrivate = false
			fmt.Print("Set a password? (leave empty for none): ")
			password, _ := reader.ReadString('\n')
			result.roomPassword = strings.TrimSpace(password)
		case "p", "P":
			result.isCreatingRoom = true
			result.roomName = "private"
			result.isPrivate = true
			// Always offer password prompt, even for private rooms.
			fmt.Print("Set a password? (leave empty for none): ")
			pw, _ := reader.ReadString('\n')
			result.roomPassword = strings.TrimSpace(pw)
		default:
			var idx int
			if _, err := fmt.Sscanf(choice, "%d", &idx); err == nil && idx > 0 && idx <= len(rooms) {
				result.roomName = rooms[idx-1].Name
				result.peersToConnect = rooms[idx-1].Peers
				result.isPrivate = false
				// Always prompt for a password when joining. The [password protected]
				// discovery label is unreliable (non-relay peers don't advertise it),
				// but the server will correctly reject incorrect passwords.
				fmt.Print("Enter room password (leave empty if none): ")
				password, _ := reader.ReadString('\n')
				result.roomPassword = strings.TrimSpace(password)
			} else {
				result.roomName = choice
				result.isPrivate = false
				// Treating a typed name as a new room — prompt for password
				// just like the "n" (create new) path does.
				fmt.Print("Set a password? (leave empty for none): ")
				pw, _ := reader.ReadString('\n')
				result.roomPassword = strings.TrimSpace(pw)
			}
		}
	} else {
		fmt.Print("\nNo rooms found. Enter room name (or 'private' for hidden mode, 'q' to quit): ")
		nameInput, _ := reader.ReadString('\n')
		nameInput = strings.TrimSpace(nameInput)
		// Check for quit first
		if nameInput == "q" || nameInput == "Q" || strings.EqualFold(nameInput, "quit") {
			result.quit = true
			return result
		}
		result.isCreatingRoom = true
		result.roomName = nameInput
		if result.roomName == "" {
			result.roomName = "default"
		}
		result.isPrivate = strings.ToLower(result.roomName) == "private"
		// Always offer the password prompt, even for private rooms. The user
		// can leave it empty for no password — it's optional either way.
		fmt.Print("Set a password? (leave empty for none): ")
		pw, _ := reader.ReadString('\n')
		result.roomPassword = strings.TrimSpace(pw)
	}

	return result
}

func main() {
	var useTCP bool
	var rendezvousURL string
	var isRelay bool
	var raceMode bool
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
		case "--race":
			// Happy-Eyeballs style: dial QUIC and TCP in parallel, keep the winner.
			raceMode = true
		}
	}

	if !debugMode {
		log.SetOutput(io.Discard)
	}

	CheckUDPBuffers()

	reader := bufio.NewReader(os.Stdin)

	fmt.Printf("Secure P2P Messenger (v0.5 — TCP: %v)\n", useTCP)

	// Signal handler — SIGINT/SIGTERM always exits the program.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	racePrinted := false

	for {
		// Check whether the user hit Ctrl+C at a prompt.
		select {
		case <-sigChan:
			fmt.Println("\nShutting down...")
			return
		default:
		}

		sel := promptRoomSelection(reader, discPort)
		if sel.quit {
			fmt.Println("Bye")
			return
		}

		roomName := sel.roomName
		roomPassword := sel.roomPassword
		isPrivate := sel.isPrivate
		peersToConnect := sel.peersToConnect
		isCreatingRoom := sel.isCreatingRoom

		if rendezvousURL != "" {
			rCtx, rCancel := context.WithTimeout(context.Background(), 4*time.Second)
			rPeers, err := FetchPeers(rCtx, rendezvousURL, roomName)
			rCancel()
			if err == nil {
				peersToConnect = append(peersToConnect, rPeers...)
			}
		}

		app, err := initializeApp(roomName, isPrivate, useTCP, discPort, rendezvousURL, roomPassword, isRelay && isCreatingRoom, upnpEnabled, relayPort, isCreatingRoom)
		if err != nil {
			fmt.Printf("Failed to start: %v\n", err)
			continue
		}
		app.userRequestedExit = false

		if raceMode && !racePrinted {
			app.connManager.raceTransports = true
			fmt.Println("Transport: racing QUIC and TCP (Happy-Eyeballs); first handshake wins")
			racePrinted = true
		}

		if !app.tryConnect(reader, peersToConnect) {
			app.shutdown()
			continue
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

		// Only require at least one connection if we were trying to JOIN an existing
		// room (i.e. we're not a relay and didn't create the room). Room creators
		// and relays don't need outgoing connections at startup — they wait for
		// incoming ones.
		isJoiner := !isRelay && !isCreatingRoom
		hasPeer := len(app.connManager.ListConnected()) > 0
		if hasPeer || !isJoiner {
			fmt.Printf("\nStarted | Port: %s%d%s | Room: %s | You: %s%s%s | TCP: %v\n",
				colorCyan, app.server.Port(), colorReset,
				roomName,
				colorGreen, app.connManager.GetLocalAlias(), colorReset,
				useTCP)
			app.showHelp()

			app.runCLI()

			if app.userRequestedExit {
				app.shutdown()
				return // exit program
			}

			// kicked or disconnected — cleanup and loop back to discovery
			app.shutdown()
		} else {
			fmt.Println("\nNo connections established. Returning to room selection...")
			app.shutdown()
		}
	}
}

func initializeApp(roomName string, isPrivate bool, useTCP bool, discPort int, rendezvousURL string, roomPassword string, isRelay bool, upnpEnabled bool, relayPort int, isCreatingRoom bool) (*App, error) {
	app := &App{
		roomName:       roomName,
		roomPassword:   roomPassword,
		isPrivate:      isPrivate,
		useTCP:         useTCP,
		rendezvousURL:  rendezvousURL,
		isRelay:        isRelay,
		isCreatingRoom: isCreatingRoom,
	}

	app.connManager = NewConnectionManager(0, roomName, useTCP, roomPassword, isRelay, "")

	// Optional persistent TOFU store (SSH-style known_hosts). Off by default so
	// behaviour and benchmarks are unchanged unless P2P_KNOWN_PEERS is set.
	if kp := os.Getenv("P2P_KNOWN_PEERS"); kp != "" {
		if err := app.connManager.LoadKnownPeers(kp); err != nil {
			log.Printf("[TOFU] could not load known peers from %s: %v", kp, err)
		}
	}

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

	// Pre-create room in the local server so incoming JOINs from peers
	// pass through proper auth checks. This is critical for non-relay
	// (direct P2P) mode where the first peer IS the room authority.
	if isCreatingRoom && roomPassword != "" {
		keys := DeriveRoomKeys(roomName, roomPassword, nil)
		server.CreateRoom(roomName, keys, app.connManager, roomPassword)
	}

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

// tryConnect attempts to connect to the given peers. It includes the password
// retry loop from the original flow. Returns true if at least one connection
// succeeded, false otherwise (in which case the caller should go back to
// discovery).
func (app *App) tryConnect(reader *bufio.Reader, peersToConnect []string) bool {
	if len(peersToConnect) == 0 && (app.isRelay || app.discovery == nil) {
		// Nothing to connect to — that's fine for room creators and relays.
		return true
	}

	if !app.isRelay && app.discovery != nil {
		connectCtx, connectCancel := context.WithTimeout(context.Background(), 3*time.Second)
		relayPeers, _ := app.discovery.LookupRelays(connectCtx)
		connectCancel()
		if len(relayPeers) > 0 {
			peersToConnect = relayPeers
		}
		// If no relays found, keep the originally discovered peers.
		// Don't discard them — connect directly to those peers instead.
	}
	if len(peersToConnect) == 0 {
		return true
	}

	connectRoom := app.roomName
	if app.isPrivate {
		connectRoom = "(private)"
	}
	fmt.Printf("Connecting to room %s%s%s (%d peer(s))...\n", colorCyan, connectRoom, colorReset, len(peersToConnect))
	connectedCount := 0
	var lastErr error
	// First attempt
	{
		connectCtx, connectCancel := context.WithTimeout(context.Background(), 5*time.Second)
		for _, peerAddr := range peersToConnect {
			if _, err := app.connManager.GetOrCreate(connectCtx, peerAddr); err != nil {
				fmt.Printf("Failed to connect to %s: %v\n", peerAddr, err)
				lastErr = err
			} else {
				connectedCount++
			}
		}
		connectCancel()
	}
	if connectedCount == 0 && lastErr != nil {
		if errors.Is(lastErr, ErrBanned) {
			fmt.Println("\n[System] You are banned from this room. Returning to room selection...")
			return false
		}
		// Connection failed. Offer to retry with a different password.
		fmt.Printf("\nConnection failed: %v\n", lastErr)
		if !app.isRelay && len(peersToConnect) > 0 {
			// Re-prompt for password and retry.
			for {
				fmt.Print("\nRe-enter password (or empty to abort): ")
				newPw, _ := reader.ReadString('\n')
				newPw = strings.TrimSpace(newPw)
				if newPw == "" {
					fmt.Println("Aborted. Returning to room selection...")
					return false
				}
				// Update CM's password and retry the connection.
				app.roomPassword = newPw
				app.connManager.roomPassword = newPw
				// Re-derive keys with new password.
				keys := DeriveRoomKeys(app.roomName, newPw, nil)
				if cm := app.connManager; cm != nil {
					cm.UpdateRoomKeys(app.roomName, newPw, fmt.Sprintf("%x", cm.roomSecret))
					_ = keys
				}
				fmt.Printf("Retrying with new password...\n")
				connectedCount = 0
				var firstErr error
				// New context for each retry
				connectCtx, connectCancel := context.WithTimeout(context.Background(), 5*time.Second)
				for _, peerAddr := range peersToConnect {
					_, err := app.connManager.GetOrCreate(connectCtx, peerAddr)
					if err != nil {
						fmt.Printf("Failed to connect to %s: %v\n", peerAddr, err)
						if firstErr == nil {
							firstErr = err
						}
					} else {
						connectedCount++
					}
				}
				connectCancel()
				if connectedCount > 0 {
					break
				}
				if firstErr != nil {
					if errors.Is(firstErr, ErrBanned) {
						fmt.Println("\n[System] You are banned from this room. Returning to room selection...")
						return false
					}
					if errors.Is(firstErr, ErrWrongPassword) {
						fmt.Println("\n[System] Wrong password. Please try again.")
						// Don't exit; let the password retry loop continue
					}
					if errors.Is(firstErr, ErrTimeout) {
						fmt.Println("\n[System] Connection timed out. The relay may be offline or unreachable.")
						// Don't exit; let the user decide whether to retry
					}
				}
			}
		} else {
			fmt.Println("No peers to retry. Returning to room selection...")
			return false
		}
	}
	if connectedCount > 0 {
		fmt.Printf("Connected to %s%d%s peer(s)\n", colorGreen, connectedCount, colorReset)
		return true
	}
	return false
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

			case "nick", "name", "alias":
				if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
					fmt.Println("Usage: /nick <new_alias>")
				} else {
					newAlias := strings.TrimSpace(parts[1])
					oldAlias := app.connManager.GetLocalAlias()
					app.connManager.SetLocalAlias(newAlias)
					// Persist so we keep this alias on next launch
					if err := SaveAlias(app.connManager.GetDeviceID(), newAlias); err != nil {
						// Non-fatal; just log
						fmt.Printf("[Warning] could not persist alias: %v\n", err)
					}
					fmt.Printf("\n%s\n> ", formatSystemMessage(fmt.Sprintf("You changed your alias from %s to %s", oldAlias, newAlias)))
					// Notify peers: write a cleartext SYSTEM line to each connected peer.
					// For relay mode, the relay's handleMessage → SYSTEM: handler
					// broadcasts to the room. For direct P2P, the peer's readLoop
					// displays it directly.
					notify := fmt.Sprintf("SYSTEM:%s is now known as %s\n", oldAlias, newAlias)
					for _, addr := range app.connManager.ListConnected() {
						app.connManager.SendRaw(addr, notify)
					}
				}

			case "room", "rooms":
				app.showRoomInfo()

			case "myip":
				app.showPublicIP()

			case "kick":
				if len(parts) < 2 {
					fmt.Println("Usage: /kick <alias>")
				} else {
					app.kickPeer(parts[1])
				}

			case "ban":
				if len(parts) < 2 {
					fmt.Println("Usage: /ban <alias>")
				} else {
					app.banPeer(parts[1])
				}

			case "unban":
				if len(parts) < 2 {
					fmt.Println("Usage: /unban <alias|address|deviceID>")
				} else {
					app.unbanPeer(parts[1])
				}

			case "exit", "quit", "q", "bye":
				fmt.Println("Bye")
				app.userRequestedExit = true
				return

			default:
				fmt.Printf("Unknown command: /%s. Type /help for available commands.\n", cmd)
			}
		} else {
			app.sendToRoom(line)
		}

		// Check whether we were kicked or disconnected. If so, print a clear
		// message and return so the caller can go back to discovery.
		select {
		case <-app.connManager.kicked:
			fmt.Println("\n[System] You were removed from the room. Returning to room selection...")
			return
		case <-app.connManager.disconnected:
			fmt.Println("\n[System] Connection to relay lost. Returning to room selection...")
			return
		default:
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
		if len(peers) == 0 {
			peers = app.connManager.ListConnected()
		}
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

func (app *App) kickPeer(alias string) {
	// If user is the creator (server), handle kick directly
	if app.isRelay || app.isCreatingRoom {
		app.server.roomsMu.RLock()
		var targetPeer *Peer
		var targetRoom *Room
		for _, room := range app.server.rooms {
			room.peersMu.RLock()
			for _, p := range room.peers {
				if p.alias == alias {
					targetPeer = p
					targetRoom = room
					break
				}
			}
			room.peersMu.RUnlock()
			if targetPeer != nil {
				break
			}
		}
		app.server.roomsMu.RUnlock()

		if targetPeer != nil {
			// Create a fake requester peer (the creator)
			requester := &Peer{
				addr:  "creator",
				alias: app.connManager.GetLocalAlias(),
				room:  targetRoom,
			}
			app.server.handleKick(requester, alias)
		} else {
			fmt.Printf("Peer '%s' not found\n", alias)
		}
		return
	}

	// Otherwise, send KICK message to relay
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	relays := app.connManager.GetRelayAddrs()
	if len(relays) == 0 {
		fmt.Println("Not connected to a relay. Use /connect <ip:port> to connect.")
		return
	}
	msg := fmt.Sprintf("KICK:%s", alias)
	if err := app.connManager.Send(ctx, relays[0], msg); err != nil {
		fmt.Printf("Failed to send kick: %v\n", err)
	}
}

func (app *App) banPeer(alias string) {
	// If user is the creator (server), handle ban directly
	if app.isRelay || app.isCreatingRoom {
		app.server.roomsMu.RLock()
		var targetPeer *Peer
		var targetRoom *Room
		for _, room := range app.server.rooms {
			room.peersMu.RLock()
			for _, p := range room.peers {
				if p.alias == alias {
					targetPeer = p
					targetRoom = room
					break
				}
			}
			room.peersMu.RUnlock()
			if targetPeer != nil {
				break
			}
		}
		app.server.roomsMu.RUnlock()

		if targetPeer != nil {
			// Create a fake requester peer (the creator)
			requester := &Peer{
				addr:  "creator",
				alias: app.connManager.GetLocalAlias(),
				room:  targetRoom,
			}
			app.server.handleBan(requester, alias)
		} else {
			fmt.Printf("Peer '%s' not found\n", alias)
		}
		return
	}

	// Otherwise, send BAN message to relay
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	relays := app.connManager.GetRelayAddrs()
	if len(relays) == 0 {
		fmt.Println("Not connected to a relay. Use /connect <ip:port> to connect.")
		return
	}
	msg := fmt.Sprintf("BAN:%s", alias)
	if err := app.connManager.Send(ctx, relays[0], msg); err != nil {
		fmt.Printf("Failed to send ban: %v\n", err)
	}
}

// unbanPeer removes a ban for the given alias, address, or device ID. Only the room owner can unban.
func (app *App) unbanPeer(target string) {
	// If user is the creator (server), handle unban directly
	if app.isRelay || app.isCreatingRoom {
		app.server.roomsMu.RLock()
		var firstRoom *Room
		for _, room := range app.server.rooms {
			firstRoom = room
			break
		}
		app.server.roomsMu.RUnlock()

		if firstRoom == nil {
			fmt.Println("No rooms hosted by this server")
			return
		}

		// Create a fake requester peer (the creator)
		requester := &Peer{
			addr:  "creator",
			alias: app.connManager.GetLocalAlias(),
			room:  firstRoom,
		}
		app.server.handleUnban(requester, target)
		return
	}

	// Otherwise, send UNBAN message to relay
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	relays := app.connManager.GetRelayAddrs()
	if len(relays) == 0 {
		fmt.Println("Not connected to a relay. Use /connect <ip:port> to connect.")
		return
	}
	msg := fmt.Sprintf("UNBAN:%s", target)
	if err := app.connManager.Send(ctx, relays[0], msg); err != nil {
		fmt.Printf("Failed to send unban: %v\n", err)
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
	fmt.Println("  /nick <alias>    - Change your display name")
	fmt.Println("  /room            - Show room info")
	fmt.Println("  /myip            - Show your public IP (STUN)")
	fmt.Println("  /kick <alias>    - Remove a peer from the room (owner only)")
	fmt.Println("  /ban <alias>     - Remove and block a peer from re-joining (owner only)")
	fmt.Println("  /unban <alias|address|deviceID> - Lift a ban (owner only)")
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
	app.shutdownOnce.Do(func() {
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
	})
}

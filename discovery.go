package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"syscall"
	"time"
)

const (
	discoveryPort      = 19999 // Fixed UDP port for discovery broadcasts
	broadcastAddr      = "255.255.255.255:19999"
	localhostBroadcast = "127.255.255.255:19999"
)

// DiscoveryMessage is sent via UDP broadcast
type DiscoveryMessage struct {
	Type    string `json:"type"`    // "announce" or "query"
	Room    string `json:"room"`    // Room name
	Port    int    `json:"port"`    // QUIC server port
	Version string `json:"version"` // Protocol version
}

type DiscoveryService struct {
	roomName   string
	localPort  int
	conn       *net.UDPConn
	peers      map[string]time.Time // addr -> last seen time
	peersMu    sync.RWMutex
	stopCh     chan struct{}
	localAddrs map[string]bool // Our own addresses to filter out
}

type RoomInfo struct {
	Name  string
	Peers []string
}

func NewDiscoveryService(port int, roomName string) (*DiscoveryService, error) {
	// Use ListenConfig with SO_REUSEADDR for multiple instances on same port
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			c.Control(func(fd uintptr) {
				// SO_REUSEADDR allows multiple processes to bind to same port
				opErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
			})
			return opErr
		},
	}

	// Listen on discovery port with address reuse
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", discoveryPort))
	if err != nil {
		return nil, fmt.Errorf("failed to start discovery listener: %w", err)
	}

	conn := pc.(*net.UDPConn)
	conn.SetReadBuffer(65535)

	ds := &DiscoveryService{
		roomName:   roomName,
		localPort:  port,
		conn:       conn,
		peers:      make(map[string]time.Time),
		stopCh:     make(chan struct{}),
		localAddrs: getLocalAddrsMap(port),
	}

	// Start listener
	go ds.listenLoop()

	// Start periodic announcements
	go ds.announceLoop()

	// Initial announcement
	ds.announce()

	log.Printf("[Discovery] Advertising '%s' on port %s", roomName, colorPort(port))
	return ds, nil
}

func getLocalAddrsMap(port int) map[string]bool {
	addrs := make(map[string]bool)

	// Add localhost
	addrs[fmt.Sprintf("127.0.0.1:%d", port)] = true

	// Add all interface IPs
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		ifAddrs, _ := iface.Addrs()
		for _, addr := range ifAddrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ipv4 := ipnet.IP.To4(); ipv4 != nil {
					addrs[fmt.Sprintf("%s:%d", ipv4.String(), port)] = true
				}
			}
		}
	}
	return addrs
}

func (ds *DiscoveryService) listenLoop() {
	buf := make([]byte, 4096)
	for {
		select {
		case <-ds.stopCh:
			return
		default:
		}

		ds.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, remoteAddr, err := ds.conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		var msg DiscoveryMessage
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}

		// Build peer address from message
		peerAddr := fmt.Sprintf("%s:%d", remoteAddr.IP.String(), msg.Port)

		// Skip our own messages
		if ds.localAddrs[peerAddr] {
			continue
		}

		// Handle message types
		switch msg.Type {
		case "announce":
			// Store peer if same room
			if msg.Room == ds.roomName {
				ds.peersMu.Lock()
				ds.peers[peerAddr] = time.Now()
				ds.peersMu.Unlock()
			}
		case "query":
			// Respond with announce if same room or query is for all rooms
			if msg.Room == "" || msg.Room == ds.roomName {
				// Send announce via broadcast AND directly back to queryer
				ds.announce()
				ds.announceToAddr(remoteAddr) // Direct response
			}
		}
	}
}

func (ds *DiscoveryService) announceLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ds.stopCh:
			return
		case <-ticker.C:
			ds.announce()
			ds.cleanupPeers()
		}
	}
}

func (ds *DiscoveryService) announce() {
	msg := DiscoveryMessage{
		Type:    "announce",
		Room:    ds.roomName,
		Port:    ds.localPort,
		Version: "0.2",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	// Broadcast to multiple addresses for reliability
	broadcastAddrs := []string{
		broadcastAddr,      // LAN broadcast
		localhostBroadcast, // Localhost broadcast
		"127.0.0.1:19999",  // Direct localhost
	}

	for _, addrStr := range broadcastAddrs {
		addr, err := net.ResolveUDPAddr("udp4", addrStr)
		if err != nil {
			continue
		}
		ds.conn.WriteToUDP(data, addr)
	}
}

// announceToAddr sends announce directly to a specific address
func (ds *DiscoveryService) announceToAddr(addr *net.UDPAddr) {
	msg := DiscoveryMessage{
		Type:    "announce",
		Room:    ds.roomName,
		Port:    ds.localPort,
		Version: "0.2",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	ds.conn.WriteToUDP(data, addr)
}

func (ds *DiscoveryService) query() {
	msg := DiscoveryMessage{
		Type:    "query",
		Room:    ds.roomName,
		Port:    ds.localPort,
		Version: "0.2",
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	// Broadcast query
	broadcastAddrs := []string{
		broadcastAddr,
		localhostBroadcast,
		"127.0.0.1:19999",
	}

	for _, addrStr := range broadcastAddrs {
		addr, err := net.ResolveUDPAddr("udp4", addrStr)
		if err != nil {
			continue
		}
		ds.conn.WriteToUDP(data, addr)
	}
}

func (ds *DiscoveryService) cleanupPeers() {
	ds.peersMu.Lock()
	defer ds.peersMu.Unlock()

	// Remove peers not seen in 15 seconds
	cutoff := time.Now().Add(-15 * time.Second)
	for addr, lastSeen := range ds.peers {
		if lastSeen.Before(cutoff) {
			delete(ds.peers, addr)
		}
	}
}

// LookupPeers returns peers in the same room
func (ds *DiscoveryService) LookupPeers(ctx context.Context) ([]string, error) {
	// Send query to trigger responses
	ds.query()

	// Wait a bit for responses
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ds.peersMu.RLock()
	defer ds.peersMu.RUnlock()

	peers := make([]string, 0, len(ds.peers))
	for addr := range ds.peers {
		peers = append(peers, addr)
	}
	return peers, nil
}

func (ds *DiscoveryService) Shutdown() {
	close(ds.stopCh)
	if ds.conn != nil {
		ds.conn.Close()
	}
}

// DiscoverAllRooms finds all available rooms on the network
func DiscoverAllRooms(ctx context.Context) ([]RoomInfo, error) {
	// Create temporary listener
	addr := &net.UDPAddr{Port: 0, IP: net.IPv4zero}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	localAddrs := getLocalAddrsMap(0)            // We don't have a port yet, will filter by IP
	roomsMap := make(map[string]map[string]bool) // room -> set of peers

	// Send query for all rooms
	queryMsg := DiscoveryMessage{
		Type:    "query",
		Room:    "", // Empty = query all rooms
		Port:    0,
		Version: "0.2",
	}
	data, _ := json.Marshal(queryMsg)

	// Broadcast query
	broadcastAddrs := []string{
		broadcastAddr,
		localhostBroadcast,
		"127.0.0.1:19999",
	}
	for _, addrStr := range broadcastAddrs {
		bAddr, _ := net.ResolveUDPAddr("udp4", addrStr)
		conn.WriteToUDP(data, bAddr)
	}

	// Listen for responses
	buf := make([]byte, 4096)
	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			goto done
		default:
		}

		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}

		var msg DiscoveryMessage
		if err := json.Unmarshal(buf[:n], &msg); err != nil {
			continue
		}

		if msg.Type != "announce" || msg.Room == "" || msg.Room == "private" {
			continue
		}

		peerAddr := fmt.Sprintf("%s:%d", remoteAddr.IP.String(), msg.Port)

		// Skip if it's our own IP (we check by IP only since we don't know our port yet)
		isLocal := false
		for localAddr := range localAddrs {
			// Properly extract IP from "IP:port" format using net.SplitHostPort
			if localIP, _, err := net.SplitHostPort(localAddr); err == nil {
				if localIP == remoteAddr.IP.String() {
					isLocal = true
					break
				}
			}
		}
		if isLocal {
			continue
		}

		if roomsMap[msg.Room] == nil {
			roomsMap[msg.Room] = make(map[string]bool)
		}
		roomsMap[msg.Room][peerAddr] = true
	}

done:
	// Convert to slice
	var rooms []RoomInfo
	for roomName, peersSet := range roomsMap {
		peers := make([]string, 0, len(peersSet))
		for peer := range peersSet {
			peers = append(peers, peer)
		}
		rooms = append(rooms, RoomInfo{Name: roomName, Peers: peers})
	}
	return rooms, nil
}

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

var discoveryPort = 19999
var broadcastAddresses = []string{
	"255.255.255.255",
	"127.255.255.255",
	"127.0.0.1",
}

type DiscoveryMessage struct {
	Type        string `json:"type"`
	Room        string `json:"room"`
	Port        int    `json:"port"`
	Version     string `json:"version"`
	HasPassword bool   `json:"has_password,omitempty"`
}

type DiscoveryService struct {
	roomName    string
	localPort   int
	conn        *net.UDPConn
	peers       map[string]time.Time
	peersMu     sync.RWMutex
	stopCh      chan struct{}
	localAddrs  map[string]bool
	hasPassword func() bool
}

type RoomInfo struct {
	Name        string
	Peers       []string
	HasPassword bool
}

func NewDiscoveryService(port int, roomName string, discPort int, hasPassword func() bool) (*DiscoveryService, error) {
	discoveryPort = discPort

	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			c.Control(func(fd uintptr) {
				if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
					opErr = err
					return
				}
				opErr = setReusePort(int(fd))
			})
			return opErr
		},
	}

	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", discoveryPort))
	if err != nil {
		return nil, fmt.Errorf("failed to start discovery listener: %w", err)
	}

	conn := pc.(*net.UDPConn)
	conn.SetReadBuffer(65535)

	ds := &DiscoveryService{
		roomName:    roomName,
		localPort:   port,
		conn:        conn,
		peers:       make(map[string]time.Time),
		stopCh:      make(chan struct{}),
		localAddrs:  getLocalAddrsMap(port),
		hasPassword: hasPassword,
	}

	go ds.listenLoop()
	go ds.announceLoop()
	ds.announce()

	log.Printf("[Discovery] Advertising '%s' on port %s", roomName, colorPort(port))
	return ds, nil
}

func getLocalAddrsMap(port int) map[string]bool {
	addrs := make(map[string]bool)
	addrs[fmt.Sprintf("127.0.0.1:%d", port)] = true

	ifaces, err := net.Interfaces()
	if err != nil {
		return addrs
	}
	for _, iface := range ifaces {
		ifAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
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

func (ds *DiscoveryService) sendBroadcast(msg DiscoveryMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	for _, ip := range broadcastAddresses {
		addr, err := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, discoveryPort))
		if err != nil {
			continue
		}
		ds.conn.WriteToUDP(data, addr)
	}
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

		peerAddr := fmt.Sprintf("%s:%d", remoteAddr.IP.String(), msg.Port)

		if ds.localAddrs[peerAddr] {
			continue
		}

		switch msg.Type {
		case "announce":
			if msg.Room == ds.roomName {
				ds.peersMu.Lock()
				ds.peers[peerAddr] = time.Now()
				ds.peersMu.Unlock()
			}
		case "query":
			if msg.Room == "" || msg.Room == ds.roomName {
				ds.announce()
				ds.announceToAddr(remoteAddr)
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
	ds.sendBroadcast(DiscoveryMessage{
		Type:        "announce",
		Room:        ds.roomName,
		Port:        ds.localPort,
		Version:     "0.4",
		HasPassword: ds.hasPassword(),
	})
}

func (ds *DiscoveryService) announceToAddr(addr *net.UDPAddr) {
	msg := DiscoveryMessage{
		Type:        "announce",
		Room:        ds.roomName,
		Port:        ds.localPort,
		Version:     "0.4",
		HasPassword: ds.hasPassword(),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	ds.conn.WriteToUDP(data, addr)
}

func (ds *DiscoveryService) query() {
	ds.sendBroadcast(DiscoveryMessage{
		Type:    "query",
		Room:    ds.roomName,
		Port:    ds.localPort,
		Version: "0.4",
	})
}

func (ds *DiscoveryService) cleanupPeers() {
	ds.peersMu.Lock()
	defer ds.peersMu.Unlock()

	cutoff := time.Now().Add(-15 * time.Second)
	for addr, lastSeen := range ds.peers {
		if lastSeen.Before(cutoff) {
			delete(ds.peers, addr)
		}
	}
}

func (ds *DiscoveryService) LookupPeers(ctx context.Context) ([]string, error) {
	ds.query()

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

func DiscoverAllRooms(ctx context.Context, discPort int) ([]RoomInfo, error) {
	addr := &net.UDPAddr{Port: 0, IP: net.IPv4zero}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	roomsMap := make(map[string]map[string]bool)
	roomHasPassword := make(map[string]bool)

	queryMsg := DiscoveryMessage{
		Type:    "query",
		Room:    "",
		Port:    0,
		Version: "0.4",
	}
	data, _ := json.Marshal(queryMsg)

	for _, ip := range broadcastAddresses {
		bAddr, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", ip, discPort))
		conn.WriteToUDP(data, bAddr)
	}

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

		if roomsMap[msg.Room] == nil {
			roomsMap[msg.Room] = make(map[string]bool)
		}
		roomsMap[msg.Room][peerAddr] = true
		roomHasPassword[msg.Room] = msg.HasPassword
	}

done:
	var rooms []RoomInfo
	for roomName, peersSet := range roomsMap {
		peers := make([]string, 0, len(peersSet))
		for peer := range peersSet {
			peers = append(peers, peer)
		}
		rooms = append(rooms, RoomInfo{Name: roomName, Peers: deduplicatePeers(peers), HasPassword: roomHasPassword[roomName]})
	}
	return rooms, nil
}

// deduplicatePeers removes loopback addresses when equivalent non-loopback peers exist.
// This prevents connecting twice to the same process (once via LAN IP, once via 127.x).
func deduplicatePeers(peers []string) []string {
	hasNonLoopback := false
	for _, p := range peers {
		host, _, _ := net.SplitHostPort(p)
		ip := net.ParseIP(host)
		if ip != nil && !ip.IsLoopback() {
			hasNonLoopback = true
			break
		}
	}
	if !hasNonLoopback {
		return peers
	}
	filtered := peers[:0]
	for _, p := range peers {
		host, _, _ := net.SplitHostPort(p)
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

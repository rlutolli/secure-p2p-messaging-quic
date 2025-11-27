package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/hashicorp/mdns"
)

type DiscoveryService struct {
	server    *mdns.Server
	roomName  string
	localPort int
	localIPs  []net.IP
}

func NewDiscoveryService(port int, roomName string) (*DiscoveryService, error) {
	ds := &DiscoveryService{
		roomName:  roomName,
		localPort: port,
		localIPs:  getLocalIPs(),
	}

	if err := ds.startAdvertising(); err != nil {
		return nil, err
	}

	return ds, nil
}

func (ds *DiscoveryService) startAdvertising() error {
	service := fmt.Sprintf("_%s._p2pmsg._udp", ds.roomName)

	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("hostname error: %w", err)
	}

	info := []string{
		fmt.Sprintf("room=%s", ds.roomName),
		"version=1.0",
	}

	serviceObj, err := mdns.NewMDNSService(
		host,
		service,
		"",
		"",
		ds.localPort,
		ds.localIPs, // Explicitly provide IPs
		info,
	)
	if err != nil {
		return fmt.Errorf("mdns service error: %w", err)
	}

	ds.server, err = mdns.NewServer(&mdns.Config{Zone: serviceObj})
	if err != nil {
		return fmt.Errorf("mdns server error: %w", err)
	}

	log.Printf("[Discovery] Advertising '%s' on port %d", ds.roomName, ds.localPort)
	return nil
}

// LookupPeers finds other peers, excluding self
func (ds *DiscoveryService) LookupPeers(ctx context.Context) ([]string, error) {
	service := fmt.Sprintf("_%s._p2pmsg._udp", ds.roomName)

	entriesCh := make(chan *mdns.ServiceEntry, 10)
	var peers []string

	// Start lookup in background
	go func() {
		params := mdns.DefaultParams(service)
		params.Entries = entriesCh
		params.Timeout = 2 * time.Second

		if err := mdns.Query(params); err != nil {
			log.Printf("[Discovery] Query error: %v", err)
		}
		close(entriesCh)
	}()

	// Collect results with timeout
	timeout := time.After(3 * time.Second)

	for {
		select {
		case entry, ok := <-entriesCh:
			if !ok {
				return peers, nil
			}
			if entry == nil || entry.Port == 0 {
				continue
			}

			// Determine IP to use
			var ip net.IP
			if entry.AddrV4 != nil {
				ip = entry.AddrV4
			} else if entry.AddrV6 != nil {
				ip = entry.AddrV6
			} else {
				continue
			}

			addr := fmt.Sprintf("%s:%d", ip.String(), entry.Port)

			// Filter out self
			if !ds.isSelf(ip, entry.Port) {
				peers = append(peers, addr)
			}

		case <-timeout:
			return peers, nil

		case <-ctx.Done():
			return peers, ctx.Err()
		}
	}
}

func (ds *DiscoveryService) isSelf(ip net.IP, port int) bool {
	if port != ds.localPort {
		return false
	}

	for _, localIP := range ds.localIPs {
		if localIP.Equal(ip) {
			return true
		}
	}

	// Also check loopback
	if ip.IsLoopback() {
		return true
	}

	return false
}

func (ds *DiscoveryService) Shutdown() {
	if ds.server != nil {
		ds.server.Shutdown()
	}
}

// Helper to get local IPs
func getLocalIPs() []net.IP {
	var ips []net.IP

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ips
	}

	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP)
			}
		}
	}

	return ips
}

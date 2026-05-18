package main

import (
	"context"
	"fmt"
	"net"

	"github.com/huin/goupnp/dcps/internetgateway2"
	"github.com/jackpal/gateway"
	"github.com/jackpal/go-nat-pmp"
)

func TryPortMapping(ctx context.Context, localPort int, description string) (externalAddr string, cleanup func(), err error) {
	externalAddr, cleanup, err = tryUPnP(ctx, localPort, description)
	if err == nil {
		return externalAddr, cleanup, nil
	}

	externalAddr, cleanup, err = tryNATPMP(localPort)
	if err == nil {
		return externalAddr, cleanup, nil
	}

	return "", nil, nil
}

func tryUPnP(ctx context.Context, localPort int, description string) (string, func(), error) {
	clients, _, err := internetgateway2.NewWANIPConnection2ClientsCtx(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("tryUPnP: %w", err)
	}

	localIP, err := getLocalIP()
	if err != nil {
		return "", nil, fmt.Errorf("tryUPnP: %w", err)
	}

	for _, c := range clients {
		err := c.AddPortMappingCtx(ctx, "", uint16(localPort), "UDP", uint16(localPort), localIP.String(), true, description, 3600)
		if err != nil {
			continue
		}

		ip, err := c.GetExternalIPAddressCtx(ctx)
		if err != nil || ip == "" {
			_ = c.DeletePortMappingCtx(context.Background(), "", uint16(localPort), "UDP")
			continue
		}

		externalAddr := fmt.Sprintf("%s:%d", ip, localPort)
		cleanup := func(client *internetgateway2.WANIPConnection2) func() {
			return func() {
				_ = client.DeletePortMappingCtx(context.Background(), "", uint16(localPort), "UDP")
			}
		}(c)

		return externalAddr, cleanup, nil
	}

	return "", nil, fmt.Errorf("tryUPnP: no client succeeded")
}

func tryNATPMP(localPort int) (string, func(), error) {
	gw, err := gateway.DiscoverGateway()
	if err != nil {
		return "", nil, fmt.Errorf("tryNATPMP: %w", err)
	}

	client := natpmp.NewClient(gw)

	resp, err := client.AddPortMapping("udp", localPort, localPort, 3600)
	if err != nil {
		return "", nil, fmt.Errorf("tryNATPMP: %w", err)
	}

	addrResp, err := client.GetExternalAddress()
	if err != nil {
		_, _ = client.AddPortMapping("udp", localPort, localPort, 0)
		return "", nil, fmt.Errorf("tryNATPMP: %w", err)
	}

	externalAddr := fmt.Sprintf("%d.%d.%d.%d:%d",
		addrResp.ExternalIPAddress[0], addrResp.ExternalIPAddress[1],
		addrResp.ExternalIPAddress[2], addrResp.ExternalIPAddress[3],
		resp.MappedExternalPort)

	cleanup := func() {
		_, _ = client.AddPortMapping("udp", localPort, localPort, 0)
	}

	return externalAddr, cleanup, nil
}

func getLocalIP() (net.IP, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return nil, fmt.Errorf("getLocalIP: %w", err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP, nil
}

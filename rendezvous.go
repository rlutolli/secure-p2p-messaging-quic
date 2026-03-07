package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type rendezvousEntry struct {
	Addr string `json:"addr"`
}

type rendezvousRoom struct {
	Peers []string `json:"peers"`
}

func RenewRegistration(ctx context.Context, serverURL, roomName, addr string) error {
	body, err := json.Marshal(rendezvousEntry{Addr: addr})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/rooms/%s", serverURL, roomName)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("rendezvous server returned %d", resp.StatusCode)
	}
	return nil
}

func FetchPeers(ctx context.Context, serverURL, roomName string) ([]string, error) {
	url := fmt.Sprintf("%s/rooms/%s", serverURL, roomName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rendezvous server returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var room rendezvousRoom
	if err := json.Unmarshal(data, &room); err != nil {
		return nil, err
	}
	return room.Peers, nil
}

/*
deviceid.go - Stable per-install device identifier.

Generates a random UUID once and stores it in ~/.config/p2p-messenger/device-id
(or platform equivalent). The same ID is returned on every call, so peers
can recognise each other across reconnections even when the network
address (IP:port) changes.
*/
package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const deviceIDFile = "device-id"

// getDeviceIDFile returns the platform-appropriate path for the device ID file.
func getDeviceIDFile() (string, error) {
	var configDir string
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			return "", fmt.Errorf("APPDATA not set")
		}
		configDir = filepath.Join(appData, "p2p-messenger")
	case "darwin", "linux", "freebsd", "openbsd", "netbsd":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config", "p2p-messenger")
	default:
		configDir = ".p2p-messenger"
	}
	return filepath.Join(configDir, deviceIDFile), nil
}

// GetDeviceID returns a stable random ID for this device. The ID is generated
// once on first call and persisted to disk. Subsequent calls return the same ID.
func GetDeviceID() (string, error) {
	path, err := getDeviceIDFile()
	if err != nil {
		return "", err
	}

	// Try to read existing ID
	if data, err := os.ReadFile(path); err == nil {
		id := string(data)
		if len(id) == 64 { // 32 bytes hex-encoded
			return id, nil
		}
	}

	// Generate new ID
	idBytes := make([]byte, 32)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("generate device id: %w", err)
	}
	id := hex.EncodeToString(idBytes)

	// Ensure parent directory exists
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", fmt.Errorf("create config dir: %w", err)
		}
	}

	// Persist (best-effort; we still return the ID even if save fails)
	_ = os.WriteFile(path, []byte(id), 0600)

	return id, nil
}

// SaveAlias persists the alias for this device ID at ~/.config/p2p-messenger/alias-<deviceIDShort>.
// deviceIDShort is the first 16 hex chars of the device ID (enough to be unique).
func SaveAlias(deviceID, alias string) error {
	if deviceID == "" {
		return fmt.Errorf("deviceid: empty device ID")
	}
	path, err := aliasPath(deviceID)
	if err != nil {
		return err
	}
	// Validate alias
	if strings.ContainsAny(alias, "|\n\r") {
		return fmt.Errorf("deviceid: alias contains invalid character")
	}
	return os.WriteFile(path, []byte(alias), 0600)
}

// LoadAlias returns the persisted alias for this device ID, or "" if none.
func LoadAlias(deviceID string) string {
	if deviceID == "" {
		return ""
	}
	path, err := aliasPath(deviceID)
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	a := strings.TrimSpace(string(data))
	if a == "" {
		return ""
	}
	return a
}

func aliasPath(deviceID string) (string, error) {
	if len(deviceID) < 16 {
		return "", fmt.Errorf("deviceid: deviceID too short")
	}
	short := deviceID[:16]
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(configDir, "p2p-messenger")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "alias-"+short), nil
}

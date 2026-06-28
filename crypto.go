package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// RoomKeys holds the three derived keys for a password-protected room.
type RoomKeys struct {
	AuthKey []byte // Argon2id output — backward compat for room auth
	EncKey  []byte // HKDF-derived — AES-256-GCM encryption key
	HMACKey []byte // HKDF-derived — HMAC-SHA256 signing key
}

// Encrypt encrypts plaintext with AES-256-GCM and returns a base64-encoded payload.
// The payload format is: base64(12-byte-random-nonce || ciphertext || 16-byte-gcm-tag)
func (rk *RoomKeys) Encrypt(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(rk.EncKey)
	if err != nil {
		return "", fmt.Errorf("Encrypt: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("Encrypt: cipher.NewGCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("Encrypt: read nonce: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a base64-encoded payload produced by Encrypt.
// The payload format is: base64(12-byte-nonce || ciphertext || 16-byte-gcm-tag)
func (rk *RoomKeys) Decrypt(base64Payload string) ([]byte, error) {
	raw, err := base64.RawStdEncoding.DecodeString(base64Payload)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: base64 decode: %w", err)
	}
	block, err := aes.NewCipher(rk.EncKey)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: cipher.NewGCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return nil, fmt.Errorf("Decrypt: ciphertext too short (%d bytes, need at least %d)", len(raw), nonceSize)
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: gcm.Open: %w", err)
	}
	return plaintext, nil
}

// Sign computes HMAC-SHA256 over message and returns the hex-encoded signature.
func (rk *RoomKeys) Sign(message string) string {
	mac := hmac.New(sha256.New, rk.HMACKey)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify checks whether hmacHex is a valid HMAC-SHA256 signature for message.
func (rk *RoomKeys) Verify(message, hmacHex string) bool {
	expected, err := hex.DecodeString(hmacHex)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, rk.HMACKey)
	mac.Write([]byte(message))
	actual := mac.Sum(nil)
	return subtle.ConstantTimeCompare(expected, actual) == 1
}

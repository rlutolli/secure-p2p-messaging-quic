//go:build !linux

package main

func setReusePort(fd int) error {
	// SO_REUSEPORT not available or not needed on this platform
	return nil
}

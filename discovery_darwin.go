//go:build darwin

package main

import "syscall"

const soReusePort = 0x200 // SO_REUSEPORT on Darwin/macOS

func setReusePort(fd int) error {
	return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1)
}

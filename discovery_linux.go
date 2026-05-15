//go:build linux

package main

import "syscall"

const soReusePort = 0xf // SO_REUSEPORT on Linux

func setReusePort(fd int) error {
	return syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, soReusePort, 1)
}

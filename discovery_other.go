//go:build !linux && !darwin

package main

func setReusePort(fd int) error {
	return nil
}

//go:build linux

package main

import "golang.org/x/sys/unix"

// ioctlGetTermios reads a terminal's settings (setup.go: is stdin a terminal).
const ioctlGetTermios = unix.TCGETS

//go:build darwin

package main

import "syscall"

// stopSelf stops the process (fetchTestStop). On macOS, kill(getpid) stopped the caller before its
// next instruction in every observed run (the fetchchild tests).
func stopSelf() {
	syscall.Kill(syscall.Getpid(), syscall.SIGSTOP)
}

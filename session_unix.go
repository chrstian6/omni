//go:build darwin || linux

package main

import "golang.org/x/sys/unix"

// processAlive mirrors the Swift HUD's check: signal 0 tests for existence
// without delivering anything. EPERM still means "it exists, just not ours".
func processAlive(pid int32) bool {
	err := unix.Kill(int(pid), 0)
	return err == nil || err == unix.EPERM
}

// endProcess sends SIGTERM so Claude gets a chance to flush its transcript
// before exiting — a hard kill would strand the .jsonl mid-write.
func endProcess(pid int32) error {
	return unix.Kill(int(pid), unix.SIGTERM)
}

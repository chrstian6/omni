//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// procTable snapshots every process with its parent and exe name via the
// Toolhelp API — the Windows equivalent of the one-shot `ps` on Unix.
func procTable() map[int32]procInfo {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	table := make(map[int32]procInfo, 512)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))

	if err := windows.Process32First(snap, &entry); err != nil {
		return table
	}
	for {
		name := windows.UTF16ToString(entry.ExeFile[:])
		table[int32(entry.ProcessID)] = procInfo{
			ppid: int32(entry.ParentProcessID),
			comm: filepath.Base(strings.TrimSpace(name)),
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	return table
}

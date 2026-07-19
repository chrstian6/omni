//go:build darwin || linux

package main

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// procTable builds the whole process table in one `ps` call, then ancestry
// walks happen in memory — one spawn per refresh, not one per session.
// `comm` on macOS/Linux is the full executable path, so we base-name it.
func procTable() map[int32]procInfo {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return nil
	}

	table := make(map[int32]procInfo, 512)
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		// comm may contain spaces (e.g. "Code Helper (Renderer)"); rejoin.
		comm := strings.Join(fields[2:], " ")
		table[int32(pid)] = procInfo{
			ppid: int32(ppid),
			comm: filepath.Base(comm),
		}
	}
	return table
}

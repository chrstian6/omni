package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// openInNewWindow launches the Omni dashboard in a fresh terminal window of its
// own, rather than taking over the current one. That window is a real OS window,
// so it minimizes and resizes like any other — and Omni re-renders to fit
// whatever size it's dragged to.
func openInNewWindow() error {
	exe := selfPath()
	switch runtime.GOOS {
	case "darwin":
		return openMac(exe)
	case "windows":
		return openWindows(exe)
	default:
		return openLinux(exe)
	}
}

// openMac drives Terminal.app via AppleScript: a new window, sized to something
// comfortable, running the binary. `exec` replaces the shell so quitting Omni
// closes the window cleanly.
func openMac(exe string) error {
	// No `exec`: run Omni as a child of the window's shell so quitting Omni
	// drops back to a shell prompt instead of killing the window. No forced
	// bounds either — the window opens at your normal Terminal size and Omni
	// fills whatever size that is (and re-fills when you resize it).
	script := fmt.Sprintf(`
tell application "Terminal"
	activate
	do script "clear; %s"
	set custom title of tab 1 of front window to "Omni"
end tell`, shellQuote(exe))
	return exec.Command("osascript", "-e", script).Run()
}

// newSessionInDir opens a fresh terminal window running `claude` in the given
// directory, so a new Claude Code session can be started straight from Omni. The
// new session writes its registry file on startup, so it shows up in the list
// within a tick.
func newSessionInDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		dir = homeDir()
	}
	switch runtime.GOOS {
	case "darwin":
		script := fmt.Sprintf(`
tell application "Terminal"
	activate
	do script "cd %s && clear && claude"
	set custom title of tab 1 of front window to "claude — %s"
end tell`, shellQuote(dir), shellQuote(baseName(dir)))
		return exec.Command("osascript", "-e", script).Run()
	case "windows":
		if wt, err := exec.LookPath("wt.exe"); err == nil {
			return exec.Command(wt, "-w", "new", "-d", dir, "claude").Start()
		}
		return exec.Command("cmd", "/c", "start", "claude", "/d", dir, "cmd", "/k", "claude").Start()
	default:
		cmd := "cd " + shellQuote(dir) + " && claude"
		candidates := [][]string{
			{"x-terminal-emulator", "-e", "sh", "-c", cmd},
			{"gnome-terminal", "--", "sh", "-c", cmd},
			{"konsole", "-e", "sh", "-c", cmd},
			{"xfce4-terminal", "-e", "sh -c " + shellQuote(cmd)},
			{"alacritty", "-e", "sh", "-c", cmd},
			{"kitty", "sh", "-c", cmd},
			{"xterm", "-e", "sh", "-c", cmd},
		}
		for _, c := range candidates {
			if _, err := exec.LookPath(c[0]); err == nil {
				return exec.Command(c[0], c[1:]...).Start()
			}
		}
		return fmt.Errorf("no terminal emulator found")
	}
}

// baseName is the last path element, for a friendly window title.
func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func openWindows(exe string) error {
	// Prefer Windows Terminal (tabs, resizable); fall back to a console window.
	if wt, err := exec.LookPath("wt.exe"); err == nil {
		return exec.Command(wt, "-w", "new", "--title", "Omni", exe).Start()
	}
	return exec.Command("cmd", "/c", "start", "Omni", exe).Start()
}

func openLinux(exe string) error {
	// Try the common terminal emulators in turn.
	candidates := [][]string{
		{"x-terminal-emulator", "-e", exe},
		{"gnome-terminal", "--", exe},
		{"konsole", "-e", exe},
		{"xfce4-terminal", "-e", exe},
		{"alacritty", "-e", exe},
		{"kitty", exe},
		{"xterm", "-e", exe},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err == nil {
			return exec.Command(c[0], c[1:]...).Start()
		}
	}
	return fmt.Errorf("no terminal emulator found; run %q directly", exe)
}

// shellQuote wraps a path for safe embedding in the AppleScript `do script`
// string (single-quoted for the shell, with any single quotes escaped).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensureExecutable is a small guard used before launching, so a freshly-copied
// binary that lost its +x bit still runs.
func ensureExecutable(path string) {
	if info, err := os.Stat(path); err == nil {
		_ = os.Chmod(path, info.Mode()|0o111)
	}
}

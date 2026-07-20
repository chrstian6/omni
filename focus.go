package main

import (
	"os/exec"
	"runtime"
	"strings"
)

// Focusing the owning window is the natural companion to treating the
// originating terminal as the source of truth: when a message is waiting on a
// session that's idle, the fix isn't for Omni to force its way in — it's to put
// the user back in the window that owns the conversation, where one keystroke
// flushes the queue.

// focusAppFor maps a detected surface label back to the macOS application name
// to activate. Labels come from surfaceMatchers in surface.go.
var focusAppFor = map[string]string{
	"VS Code":   "Visual Studio Code",
	"Cursor":    "Cursor",
	"Windsurf":  "Windsurf",
	"iTerm":     "iTerm",
	"Terminal":  "Terminal",
	"Warp":      "Warp",
	"WezTerm":   "WezTerm",
	"Alacritty": "Alacritty",
	"kitty":     "kitty",
	"Ghostty":   "Ghostty",
	"Hyper":     "Hyper",
	"Tabby":     "Tabby",
	"Rio":       "Rio",
	"IntelliJ":  "IntelliJ IDEA",
	"JetBrains": "IntelliJ IDEA",
}

// FocusSurface brings the app that owns a session to the front. It returns
// false when there's nothing meaningful to focus — a tmux pane, an SSH session,
// or a surface we couldn't identify — so the caller can say so instead of
// silently doing nothing.
func FocusSurface(label string) (bool, error) {
	app, ok := focusAppFor[label]
	if !ok {
		return false, nil
	}
	if runtime.GOOS != "darwin" {
		// Focusing is best-effort and macOS-only for now; elsewhere the dashboard
		// just reports where the session lives.
		return false, nil
	}
	// `activate` raises the app without stealing focus from a specific document,
	// which is the closest thing to "take me back to that window".
	script := `tell application "` + strings.ReplaceAll(app, `"`, `\"`) + `" to activate`
	return true, exec.Command("osascript", "-e", script).Run()
}

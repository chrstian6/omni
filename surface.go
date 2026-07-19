package main

import "strings"

// The session registry only says "interactive" — it doesn't record which
// terminal or IDE a session lives in. We recover that by walking each claude
// process up its parent chain until we hit an app we recognize. That's what
// turns this from a flat list into a centralized view across VS Code, Cursor,
// iTerm, Terminal, Windows Terminal, tmux, SSH, and so on.

type procInfo struct {
	ppid int32
	comm string // executable base name
}

// procTable is provided per-platform: a snapshot of pid -> (parent, name).
// See surface_unix.go and surface_windows.go.

// knownSurface maps a process base name to a friendly label. Order matters:
// more specific names are checked before generic shells.
var surfaceMatchers = []struct {
	needle string
	label  string
}{
	{"code helper", "VS Code"},
	{"visual studio code", "VS Code"},
	{"cursor helper", "Cursor"},
	{"windsurf", "Windsurf"},
	{"cursor", "Cursor"},
	{"code", "VS Code"},
	{"iterm", "iTerm"},
	{"apple terminal", "Terminal"},
	{"warp", "Warp"},
	{"wezterm", "WezTerm"},
	{"alacritty", "Alacritty"},
	{"kitty", "kitty"},
	{"ghostty", "Ghostty"},
	{"hyper", "Hyper"},
	{"tabby", "Tabby"},
	{"rio", "Rio"},
	{"windowsterminal", "Win Terminal"},
	{"wt", "Win Terminal"},
	{"conhost", "Console"},
	{"powershell", "PowerShell"},
	{"pwsh", "PowerShell"},
	{"cmd", "cmd"},
	{"tmux", "tmux"},
	{"screen", "screen"},
	{"sshd", "SSH"},
	{"jetbrains", "JetBrains"},
	{"idea", "IntelliJ"},
	{"terminal", "Terminal"},
}

func matchSurface(comm string) (string, bool) {
	c := strings.ToLower(comm)
	for _, m := range surfaceMatchers {
		if strings.Contains(c, m.needle) {
			return m.label, true
		}
	}
	return "", false
}

// detectSurface walks up from a claude pid through its ancestors, skipping the
// shells in between, and returns the first app it recognizes.
func detectSurface(pid int32, table map[int32]procInfo) string {
	seen := map[int32]bool{}
	cur := pid
	for depth := 0; depth < 24; depth++ {
		info, ok := table[cur]
		if !ok || seen[cur] {
			break
		}
		seen[cur] = true

		// Don't match on claude itself; look at what launched it.
		if cur != pid {
			if label, ok := matchSurface(info.comm); ok {
				return label
			}
		}
		if info.ppid <= 1 || info.ppid == cur {
			break
		}
		cur = info.ppid
	}
	return "shell"
}

// DetectSurfaces resolves a label for each session in one snapshot.
func DetectSurfaces(sessions []Session) map[string]string {
	table := procTable()
	out := make(map[string]string, len(sessions))
	for _, s := range sessions {
		out[s.SessionID] = detectSurface(s.PID, table)
	}
	return out
}

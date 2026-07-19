package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// Session mirrors the JSON that every running Claude Code process writes to
// ~/.claude/sessions/<pid>.json — the same registry file the macOS HUD reads,
// which is what makes this a *centralized* view: every terminal, every IDE
// (VS Code, Cursor, a plain shell) that runs `claude` drops a file here, so
// one dashboard sees all of them regardless of where they were started.
type Session struct {
	PID             int32   `json:"pid"`
	SessionID       string  `json:"sessionId"`
	Cwd             string  `json:"cwd"`
	StartedAt       float64 `json:"startedAt"` // epoch millis
	Name            string  `json:"name"`
	Status          string  `json:"status"` // "busy" | "waiting"
	StatusUpdatedAt float64 `json:"statusUpdatedAt"`
	Version         string  `json:"version"`
	Kind            string  `json:"kind"` // e.g. "terminal", "vscode", "cursor" — which surface started it
}

func sessionsDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

func (s Session) Project() string {
	return filepath.Base(s.Cwd)
}

func (s Session) IsBusy() bool { return s.Status == "busy" }

// QuietFor is how long it's been since the session last changed status.
func (s Session) QuietFor() time.Duration {
	if s.StatusUpdatedAt == 0 {
		return 0
	}
	ms := float64(time.Now().UnixMilli()) - s.StatusUpdatedAt
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms) * time.Millisecond
}

func (s Session) IsIdle(threshold time.Duration) bool {
	return !s.IsBusy() && s.QuietFor() >= threshold
}

// Surface is the human label for Kind, defaulting when the registry omits it
// (older claude versions may not have written this field yet).
func (s Session) Surface() string {
	if s.Kind == "" {
		return "terminal"
	}
	return s.Kind
}

// LoadSessions scans the registry directory and drops any file whose process
// no longer exists — a stale file left behind by a crash, not a live session.
func LoadSessions() []Session {
	entries, err := os.ReadDir(sessionsDir())
	if err != nil {
		return nil
	}

	// Dedupe by sessionId: a session that restarted (or wrote more than one
	// registry file) can leave several live files behind. Keep the freshest.
	bySession := map[string]Session{}
	var noID []Session
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessionsDir(), e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			continue
		}
		if !processAlive(s.PID) {
			continue
		}
		if s.SessionID == "" {
			noID = append(noID, s)
			continue
		}
		if prev, ok := bySession[s.SessionID]; !ok || s.StatusUpdatedAt > prev.StatusUpdatedAt {
			bySession[s.SessionID] = s
		}
	}

	live := make([]Session, 0, len(bySession)+len(noID))
	for _, s := range bySession {
		live = append(live, s)
	}
	live = append(live, noID...)

	// Sort by a STABLE key (start time, then id) so rows don't jump around as
	// sessions flip between busy and waiting each tick.
	sort.SliceStable(live, func(i, j int) bool {
		if live[i].StartedAt != live[j].StartedAt {
			return live[i].StartedAt < live[j].StartedAt
		}
		return live[i].SessionID < live[j].SessionID
	})
	return live
}

func formatDuration(d time.Duration) string {
	s := int(d.Seconds())
	if s < 0 {
		s = 0
	}
	switch {
	case s < 60:
		return strconv.Itoa(s) + "s"
	case s < 3600:
		return strconv.Itoa(s/60) + "m"
	default:
		h, m := s/3600, (s%3600)/60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
	}
}

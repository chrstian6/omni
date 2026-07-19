package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The saved-sessions store. Omni reads live sessions from ~/.claude/sessions,
// which vanish when the process exits. This store keeps a durable record of
// every session Omni has seen, so ended ones stay visible and revisitable —
// and because `claude --resume <id>` still works on a dead session's
// transcript, a saved session can even be prompted back to life.
//
// Persisted at ~/.omni/history.json as { "sessions": { <id>: record } }.

type SavedSession struct {
	SessionID  string   `json:"sessionId"`
	Name       string   `json:"name"`
	Project    string   `json:"project"`
	Cwd        string   `json:"cwd"`
	Surface    string   `json:"surface"`
	FirstSeen  int64    `json:"firstSeen"`  // epoch millis
	LastSeen   int64    `json:"lastSeen"`   // epoch millis
	LastStatus string   `json:"lastStatus"` // busy | waiting
	Title      string   `json:"title,omitempty"`
	LastPrompt string   `json:"lastPrompt,omitempty"`
	Steps      []string `json:"steps,omitempty"` // ordered recap, persisted so it survives the process
}

// EndedAgo is how long since the session was last seen alive.
func (s SavedSession) EndedAgo() time.Duration {
	if s.LastSeen == 0 {
		return 0
	}
	d := time.Duration(time.Now().UnixMilli()-s.LastSeen) * time.Millisecond
	if d < 0 {
		return 0
	}
	return d
}

type historyStore struct {
	Sessions map[string]SavedSession `json:"sessions"`
}

const maxHistory = 200 // prune the oldest beyond this

func historyPath() string { return filepath.Join(deckDir(), "history.json") }

func loadHistory() historyStore {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return historyStore{Sessions: map[string]SavedSession{}}
	}
	var h historyStore
	if err := json.Unmarshal(data, &h); err != nil || h.Sessions == nil {
		return historyStore{Sessions: map[string]SavedSession{}}
	}
	return h
}

func saveHistory(h historyStore) {
	ensureDeckDirs()
	// Prune to the most-recently-seen maxHistory records.
	if len(h.Sessions) > maxHistory {
		type kv struct {
			id   string
			seen int64
		}
		all := make([]kv, 0, len(h.Sessions))
		for id, s := range h.Sessions {
			all = append(all, kv{id, s.LastSeen})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].seen > all[j].seen })
		for _, e := range all[maxHistory:] {
			delete(h.Sessions, e.id)
		}
	}
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return
	}
	_ = writeAtomic(historyPath(), data)
}

// upsertHistory folds the currently-live sessions into the store, updating each
// record's mutable fields and stamping lastSeen. It writes only when something
// actually changed, so the idle tick doesn't hammer the disk. names/surfaces
// come from the model so saved rows show the user's chosen nickname and IDE.
func upsertHistory(live []Session, name func(Session) string, surface func(Session) string) {
	h := loadHistory()
	now := time.Now().UnixMilli()
	changed := false

	for _, s := range live {
		rec, existed := h.Sessions[s.SessionID]
		if !existed {
			rec.SessionID = s.SessionID
			rec.FirstSeen = now
			changed = true
		}
		next := rec
		next.Name = name(s)
		next.Project = s.Project()
		next.Cwd = s.Cwd
		if sf := surface(s); sf != "" {
			next.Surface = sf
		}
		next.LastStatus = s.Status
		next.LastSeen = now
		// Persist only when a meaningful field changed (ignore lastSeen churn),
		// or every ~30s so lastSeen doesn't drift too far for ended detection.
		if next.Name != rec.Name || next.Project != rec.Project ||
			next.Surface != rec.Surface || next.LastStatus != rec.LastStatus ||
			next.Cwd != rec.Cwd || now-rec.LastSeen > 30_000 {
			changed = true
		}
		h.Sessions[s.SessionID] = next
	}

	if changed {
		saveHistory(h)
	}
}

// enrichHistory attaches the transcript-derived title/last-prompt and the
// step-by-step recap to a saved record — called opportunistically when Omni has
// loaded a session's activity, so the summary survives after the process exits.
func enrichHistory(sessionID, title, lastPrompt string, steps []string) {
	if title == "" && lastPrompt == "" && len(steps) == 0 {
		return
	}
	h := loadHistory()
	rec, ok := h.Sessions[sessionID]
	if !ok {
		return
	}
	if rec.Title == title && rec.LastPrompt == lastPrompt && sameSteps(rec.Steps, steps) {
		return
	}
	rec.Title, rec.LastPrompt = title, lastPrompt
	if len(steps) > 0 {
		rec.Steps = steps
	}
	h.Sessions[sessionID] = rec
	saveHistory(h)
}

func sameSteps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// endedSessions returns saved records whose session is not currently live,
// most-recently-seen first.
func endedSessions(live []Session) []SavedSession {
	liveIDs := map[string]bool{}
	for _, s := range live {
		liveIDs[s.SessionID] = true
	}
	h := loadHistory()
	var out []SavedSession
	for _, s := range h.Sessions {
		if !liveIDs[s.SessionID] {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen > out[j].LastSeen })
	return out
}

func removeSaved(sessionID string) {
	h := loadHistory()
	if _, ok := h.Sessions[sessionID]; !ok {
		return
	}
	delete(h.Sessions, sessionID)
	saveHistory(h)
}

// asSession synthesizes a Session from a saved record so ended rows can reuse
// the live-session code paths (prompt/resume, rename, activity).
func (s SavedSession) asSession() Session {
	return Session{
		PID:       0,
		SessionID: s.SessionID,
		Cwd:       s.Cwd,
		StartedAt: float64(s.FirstSeen),
		Name:      s.Name,
		Status:    "ended",
	}
}

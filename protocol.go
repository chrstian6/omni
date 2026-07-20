package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The hook and the dashboard talk through a small shared directory, since the
// hook runs as a short-lived subprocess of an unrelated Claude session and the
// two never share memory. The contract:
//
//   ~/.omni/
//     pending/<requestID>.json    written by the hook, one per waiting tool call
//     decisions/<requestID>.json  written by the dashboard: allow | deny
//     policy.json                 approve-all toggles (global + per session)
//     blocked.log                 append-only audit of auto-blocked dangerous calls
//
// A request is identified by "<sessionID>-<prompt/tool nonce>". The hook polls
// for its decision file; the dashboard polls for pending files. Files, not
// sockets, so it works identically on macOS and Windows with no ports.

const (
	riskSafe  = "safe"
	riskWarn  = "warn"  // flagged: never covered by approve-all
	riskBlock = "block" // hard-denied by the safety layer, never reaches the user
)

type PendingRequest struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionId"`
	PID       int32    `json:"pid"`
	Project   string   `json:"project"`
	Cwd       string   `json:"cwd"`
	Tool      string   `json:"tool"`
	Summary   string   `json:"summary"` // human-readable one-liner of the action
	Risk      string   `json:"risk"`    // safe | warn (block never gets here)
	Reasons   []string `json:"reasons"` // why it was flagged, if warn
	CreatedAt int64    `json:"createdAt"`
}

type Decision struct {
	ID        string `json:"id"`
	Allow     bool   `json:"allow"`
	Reason    string `json:"reason"`
	DecidedAt int64  `json:"decidedAt"`
}

// Policy is the persisted approve-all state. AllGlobal auto-approves every
// non-flagged action in every session; AllSessions lists sessionIDs the user
// chose to auto-approve individually. Flagged (warn) actions are never
// auto-approved — that's the safety guarantee.
type Policy struct {
	AllGlobal   bool            `json:"allGlobal"`
	AllSessions map[string]bool `json:"allSessions"`
}

func deckDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".omni")
}

func pendingDir() string     { return filepath.Join(deckDir(), "pending") }
func decisionsDir() string   { return filepath.Join(deckDir(), "decisions") }
func policyPath() string     { return filepath.Join(deckDir(), "policy.json") }
func blockedLogPath() string { return filepath.Join(deckDir(), "blocked.log") }
func heartbeatPath() string  { return filepath.Join(deckDir(), "dashboard.alive") }

// heartbeatFresh reports whether a dashboard is currently running. The hook
// checks this before it ever considers waiting: if no live dashboard is around
// to answer, tool calls fall straight through to the normal prompt instead of
// blocking. This is what makes a globally-installed hook safe.
const heartbeatTTL = 6 * time.Second

func touchHeartbeat() {
	ensureDeckDirs()
	_ = writeAtomic(heartbeatPath(), []byte(time.Now().Format(time.RFC3339Nano)))
}

func removeHeartbeat() {
	_ = os.Remove(heartbeatPath())
}

func heartbeatFresh() bool {
	info, err := os.Stat(heartbeatPath())
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < heartbeatTTL
}

func ensureDeckDirs() {
	_ = os.MkdirAll(pendingDir(), 0o755)
	_ = os.MkdirAll(decisionsDir(), 0o755)
}

// --- pending requests ---

func writePending(r PendingRequest) error {
	ensureDeckDirs()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(pendingDir(), r.ID+".json"), data)
}

func removePending(id string) {
	_ = os.Remove(filepath.Join(pendingDir(), id+".json"))
}

// pendingTTL is how long a request can live before we treat it as orphaned. The
// hook only waits hookWait for an answer, so once a request is older than that
// (plus a grace margin) no hook is still listening — approving it would do
// nothing. Such requests are pruned so the queue only shows actionable ones.
const pendingTTL = hookWait + 30*time.Second

func loadPending() []PendingRequest {
	entries, err := os.ReadDir(pendingDir())
	if err != nil {
		return nil
	}
	now := time.Now().UnixMilli()
	var out []PendingRequest
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pendingDir(), e.Name()))
		if err != nil {
			continue
		}
		var r PendingRequest
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		// Orphaned? Its hook has given up (too old) or the session is gone. No one
		// is waiting on a decision, so drop the request and clean up its files.
		tooOld := r.CreatedAt > 0 && now-r.CreatedAt > pendingTTL.Milliseconds()
		dead := r.PID > 0 && !processAlive(r.PID)
		if tooOld || dead {
			removePending(r.ID)
			removeDecision(r.ID)
			continue
		}
		out = append(out, r)
	}
	// Oldest first, so the queue reads top-to-bottom in arrival order.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// --- decisions ---

func writeDecision(d Decision) error {
	ensureDeckDirs()
	d.DecidedAt = time.Now().UnixMilli()
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(decisionsDir(), d.ID+".json"), data)
}

func readDecision(id string) (Decision, bool) {
	data, err := os.ReadFile(filepath.Join(decisionsDir(), id+".json"))
	if err != nil {
		return Decision{}, false
	}
	var d Decision
	if err := json.Unmarshal(data, &d); err != nil {
		return Decision{}, false
	}
	return d, true
}

func removeDecision(id string) {
	_ = os.Remove(filepath.Join(decisionsDir(), id+".json"))
}

// --- policy ---

func loadPolicy() Policy {
	data, err := os.ReadFile(policyPath())
	if err != nil {
		return Policy{AllSessions: map[string]bool{}}
	}
	var p Policy
	if err := json.Unmarshal(data, &p); err != nil || p.AllSessions == nil {
		p.AllSessions = map[string]bool{}
	}
	return p
}

func savePolicy(p Policy) error {
	ensureDeckDirs()
	if p.AllSessions == nil {
		p.AllSessions = map[string]bool{}
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(policyPath(), data)
}

func (p Policy) autoApproves(sessionID string) bool {
	return p.AllGlobal || p.AllSessions[sessionID]
}

// appendOverrideLog records that a human deliberately overrode a hard block.
// Blocked actions are already logged when they're caught; this is the second
// half of that story, so the audit trail shows not just what was blocked but
// what was allowed through anyway, and why.
func appendOverrideLog(r PendingRequest, reason string) {
	ensureDeckDirs()
	line := time.Now().Format("2006-01-02 15:04:05") + "  " +
		r.Project + "  " + r.Tool + "  OVERRIDDEN  " +
		strings.Join(r.Reasons, "; ") + "  — " + r.Summary +
		"  [" + reason + "]\n"
	f, err := os.OpenFile(blockedLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// --- audit log ---

func appendBlockedLog(r PendingRequest) {
	ensureDeckDirs()
	line := time.Now().Format("2006-01-02 15:04:05") + "  " +
		r.Project + "  " + r.Tool + "  BLOCKED  " +
		strings.Join(r.Reasons, "; ") + "  — " + r.Summary + "\n"
	f, err := os.OpenFile(blockedLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// writeAtomic writes to a temp file in the same dir then renames, so a reader
// never sees a half-written JSON file.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

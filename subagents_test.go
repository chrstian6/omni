package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An async agent's completion does not arrive as a normal message. It lands in
// its own record shapes, and a scanner that only reads assistant/user records
// silently misses it — leaving a finished agent pinned to "running" forever with
// zero tokens. These tests pin down each shape that carries a completion.

// writeTranscript stands up a fake session transcript under a temp HOME and
// returns the session id to load.
func writeTranscript(t *testing.T, records []string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	const sid = "test-session"
	dir := filepath.Join(home, ".claude", "projects", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return sid
}

// launchRecord is an assistant turn starting a background agent, followed by the
// launch ACK that means "started", not "finished".
func launchRecord(toolID string) []string {
	launch := map[string]any{
		"type": "assistant", "timestamp": "2026-07-19T09:08:41.000Z",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_use", "name": "Agent", "id": toolID,
			"input": map[string]any{"description": "Verify hook injection options", "subagent_type": "claude-code-guide"},
		}}},
	}
	ack := map[string]any{
		"type": "user", "timestamp": "2026-07-19T09:08:42.000Z",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": toolID,
			"content": "Async agent launched successfully.",
		}}},
	}
	return []string{jsonLine(launch), jsonLine(ack)}
}

func notificationText(toolID string) string {
	return "<task-notification>\n<task-id>abc123</task-id>\n<tool-use-id>" + toolID +
		"</tool-use-id>\n<status>completed</status>\n<summary>Agent \"Verify hook injection options\" finished</summary>\n" +
		"<usage><subagent_tokens>71066</subagent_tokens><duration_ms>140755</duration_ms></usage>\n</task-notification>"
}

func jsonLine(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAsyncAgentFinishesViaQueueOperationRecord(t *testing.T) {
	const toolID = "toolu_abc"
	recs := launchRecord(toolID)
	recs = append(recs, jsonLine(map[string]any{
		"type": "queue-operation", "operation": "enqueue",
		"timestamp": "2026-07-19T09:11:01.000Z",
		"content":   notificationText(toolID),
	}))

	agents := loadSubagents(writeTranscript(t, recs))
	assertFinished(t, agents)
}

func TestAsyncAgentFinishesViaAttachmentRecord(t *testing.T) {
	const toolID = "toolu_abc"
	recs := launchRecord(toolID)
	recs = append(recs, jsonLine(map[string]any{
		"type": "attachment", "timestamp": "2026-07-19T09:11:01.000Z",
		"attachment": map[string]any{"type": "queued_command", "prompt": notificationText(toolID)},
	}))

	agents := loadSubagents(writeTranscript(t, recs))
	assertFinished(t, agents)
}

// The same completion is written several times (enqueue, remove, attachment).
// It must collapse to one row rather than showing the agent three times.
func TestDuplicateNotificationsCollapseToOneAgent(t *testing.T) {
	const toolID = "toolu_abc"
	recs := launchRecord(toolID)
	for _, op := range []string{"enqueue", "remove"} {
		recs = append(recs, jsonLine(map[string]any{
			"type": "queue-operation", "operation": op,
			"timestamp": "2026-07-19T09:11:01.000Z",
			"content":   notificationText(toolID),
		}))
	}
	recs = append(recs, jsonLine(map[string]any{
		"type": "attachment", "timestamp": "2026-07-19T09:11:01.000Z",
		"attachment": map[string]any{"prompt": notificationText(toolID)},
	}))

	agents := loadSubagents(writeTranscript(t, recs))
	assertFinished(t, agents)
}

// A transcript that merely mentions the words — reading a source file, showing a
// diff — must not be parsed as a real completion and invent an agent.
func TestMentioningNotificationsDoesNotInventAnAgent(t *testing.T) {
	recs := []string{jsonLine(map[string]any{
		"type": "user", "timestamp": "2026-07-19T09:11:01.000Z",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_read",
			"content": `its completion arrives later as a <task-notification> carrying the same ` +
				"<tool-use-id>, and reNAToolUse = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)",
		}}},
	})}

	if agents := loadSubagents(writeTranscript(t, recs)); len(agents) != 0 {
		t.Fatalf("invented %d agent(s) from text that only mentions notifications: %+v", len(agents), agents)
	}
}

// Without a completion the agent really is still running — the launch ACK alone
// must not be read as "finished".
func TestAgentStaysRunningUntilItReportsIn(t *testing.T) {
	agents := loadSubagents(writeTranscript(t, launchRecord("toolu_abc")))
	if len(agents) != 1 {
		t.Fatalf("found %d agents, want 1", len(agents))
	}
	if !agents[0].Running {
		t.Error("agent marked finished by the launch ACK alone")
	}
}

func assertFinished(t *testing.T, agents []Subagent) {
	t.Helper()
	if len(agents) != 1 {
		t.Fatalf("found %d agents, want exactly 1: %+v", len(agents), agents)
	}
	a := agents[0]
	if a.Running {
		t.Error("agent still marked running after it reported completion")
	}
	if a.Tokens != 71066 {
		t.Errorf("tokens = %d, want 71066", a.Tokens)
	}
	if a.Name != "Verify hook injection options" {
		t.Errorf("name = %q, want %q", a.Name, "Verify hook injection options")
	}
	if a.Elapsed() <= 0 || a.Elapsed().Hours() > 1 {
		t.Errorf("elapsed = %s, want a short real duration (not a runaway clock)", a.Elapsed())
	}
}

// An agent launched by a previous process for this session died with that
// process, so it can never report completion. Left alone it shows as "running"
// with a clock that ticks forever — which is what a resumed session looks like
// after a crash or a kill.
func TestReconcileMarksPreRestartAgentsAsOrphans(t *testing.T) {
	processStart := time.Date(2026, 7, 20, 12, 27, 0, 0, time.UTC)

	agents := []Subagent{
		{ID: "old", Running: true, Started: processStart.Add(-7 * time.Minute)},
		{ID: "new", Running: true, Started: processStart.Add(2 * time.Minute)},
		{ID: "done", Running: false, Started: processStart.Add(-9 * time.Minute), Ended: processStart.Add(-8 * time.Minute)},
	}

	got := ReconcileAgents(agents, processStart)

	if !got[0].Orphan || got[0].Running {
		t.Errorf("pre-restart agent should be an orphan, not running: %+v", got[0])
	}
	if got[0].Ended != processStart {
		t.Errorf("orphan's clock should stop at the process boundary, got %v", got[0].Ended)
	}
	if got[0].Elapsed() > 8*time.Minute {
		t.Errorf("orphan elapsed should be frozen, got %s", got[0].Elapsed())
	}
	if got[1].Orphan || !got[1].Running {
		t.Errorf("agent started after the process must stay running: %+v", got[1])
	}
	if got[2].Orphan {
		t.Errorf("an already-finished agent must not be marked an orphan: %+v", got[2])
	}
}

// Without a known process start we must not guess — leaving an agent running is
// better than falsely reporting real work as dead.
func TestReconcileIsANoOpWithoutAProcessStart(t *testing.T) {
	agents := []Subagent{{ID: "a", Running: true, Started: time.Now().Add(-time.Hour)}}
	got := ReconcileAgents(agents, time.Time{})
	if got[0].Orphan || !got[0].Running {
		t.Error("with no process start, agents must be left untouched")
	}
}

// runningCount must not include orphans, since that count drives "N agents
// running" in the dashboard and on the phone.
func TestOrphansDoNotCountAsRunning(t *testing.T) {
	start := time.Now()
	agents := ReconcileAgents([]Subagent{
		{ID: "ghost", Running: true, Started: start.Add(-time.Hour)},
		{ID: "live", Running: true, Started: start.Add(time.Minute)},
	}, start)
	if n := runningCount(agents); n != 1 {
		t.Fatalf("runningCount = %d, want 1 (the ghost must not count)", n)
	}
}

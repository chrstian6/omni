package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

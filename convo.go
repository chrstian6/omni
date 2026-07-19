package main

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A readable rendering of a session's conversation, reconstructed from its
// transcript. We keep the signal — what the user asked and what Claude said or
// did — and drop the noise: thinking blocks, raw tool results, image blobs, and
// the local-command wrappers around slash-commands.

type convoTurn struct {
	role  string // "you" | "claude" | "tool"
	text  string
	model string // for "claude" turns: the model id that produced it, e.g. "claude-opus-4-8"
}

func loadConvo(sessionID string) []convoTurn {
	path := findTranscript(sessionID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var turns []convoTurn
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		t, _ := rec["type"].(string)
		if t != "user" && t != "assistant" {
			continue
		}
		msg, _ := rec["message"].(map[string]any)
		if msg == nil {
			continue
		}
		for _, turn := range renderMessage(t, msg["content"], str(msg["model"])) {
			// Collapse immediate duplicates (retries re-log the same content).
			if n := len(turns); n > 0 && turns[n-1] == turn {
				continue
			}
			turns = append(turns, turn)
		}
	}
	return turns
}

func renderMessage(typ string, content any, model string) []convoTurn {
	var out []convoTurn
	emit := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// Background-agent completions arrive as giant <task-notification> XML
		// blobs. Collapse each to a one-line status instead of dumping the markup.
		if note, ok := taskNoteLine(raw); ok {
			out = append(out, convoTurn{role: "note", text: note})
			return
		}
		if s := cleanConvoText(raw); s != "" {
			out = append(out, convoTurn{role: roleOf(typ), text: displayText(s), model: model})
		}
	}
	switch c := content.(type) {
	case string:
		emit(c)
	case []any:
		for _, blk := range c {
			b, ok := blk.(map[string]any)
			if !ok {
				continue
			}
			switch b["type"] {
			case "text":
				emit(str(b["text"]))
			case "tool_use":
				if s := toolLine(b); s != "" {
					out = append(out, convoTurn{role: "tool", text: s})
				}
				// thinking, tool_result, image → intentionally dropped
			}
		}
	}
	return out
}

var (
	reTaskNote = regexp.MustCompile(`<summary>(.*?)</summary>`)
	reToolUses = regexp.MustCompile(`<tool_uses>(\d+)</tool_uses>`)
	reDuration = regexp.MustCompile(`<duration_ms>(\d+)</duration_ms>`)
)

// taskNoteLine recognizes a <task-notification> harness blob (a background agent
// finishing) and reduces it to a tidy status line — the summary plus how many
// tools it used and how long it took — dropping the ids, output-file, and usage
// XML that only add noise to the conversation.
func taskNoteLine(s string) (string, bool) {
	if !strings.Contains(s, "<task-notification>") {
		return "", false
	}
	summary := "background task finished"
	if m := reTaskNote.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		summary = strings.TrimSpace(m[1])
	}
	var extra []string
	if m := reToolUses.FindStringSubmatch(s); m != nil {
		extra = append(extra, m[1]+" tools")
	}
	if m := reDuration.FindStringSubmatch(s); m != nil {
		if ms, err := strconv.Atoi(m[1]); err == nil {
			extra = append(extra, formatDuration(time.Duration(ms)*time.Millisecond))
		}
	}
	line := summary
	if len(extra) > 0 {
		line += " · " + strings.Join(extra, " · ")
	}
	return line, true
}

func roleOf(typ string) string {
	if typ == "user" {
		return "you"
	}
	return "claude"
}

// toolLine renders a compact one-liner for a tool call, e.g. "Bash: git status"
// or "Edit app/main.go", so the reader can follow what Claude did.
func toolLine(b map[string]any) string {
	name := str(b["name"])
	input, _ := b["input"].(map[string]any)
	switch name {
	case "Bash":
		return "Bash: " + oneLine(str(input["command"]))
	case "Write", "Edit", "MultiEdit":
		return name + " " + str(input["file_path"])
	case "Read":
		return "Read " + str(input["file_path"])
	case "WebFetch":
		return "WebFetch " + str(input["url"])
	case "Task", "Agent":
		return name + " → " + oneLine(str(input["description"]))
	case "":
		return ""
	default:
		return name
	}
}

// cleanConvoText strips the local-command / attachment wrappers and skips
// content that isn't real conversation (command echoes, tool-result caveats).
func cleanConvoText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// Skip the harness wrappers entirely — they aren't user words.
	if strings.HasPrefix(s, "<local-command-") ||
		strings.HasPrefix(s, "<command-") ||
		strings.HasPrefix(s, "[Image:") ||
		strings.HasPrefix(s, "<system-reminder>") ||
		strings.HasPrefix(s, "Caveat:") {
		return ""
	}
	for _, tag := range []string{
		"<command-name>", "</command-name>", "<command-message>", "</command-message>",
		"<command-args>", "</command-args>", "[Image #1]",
	} {
		s = strings.ReplaceAll(s, tag, "")
	}
	return strings.TrimSpace(s)
}

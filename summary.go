package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// A session summary is the ordered list of steps it went through — one line per
// distinct user prompt, oldest first, ending at the most recent. Claude records
// each prompt as a "last-prompt" entry in the transcript, so walking those in
// order reconstructs the whole arc of the session without any model call.

const maxSteps = 40 // keep the most recent N; a long session shouldn't be unbounded

func loadSteps(sessionID string) []string {
	path := findTranscript(sessionID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var steps []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.Contains(line, "last-prompt") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		if obj["type"] != "last-prompt" {
			continue
		}
		p := cleanStep(str(obj["lastPrompt"]))
		if p == "" {
			continue
		}
		// Collapse consecutive duplicates (the same prompt is re-stamped often).
		if len(steps) > 0 && steps[len(steps)-1] == p {
			continue
		}
		steps = append(steps, p)
	}

	if len(steps) > maxSteps {
		steps = steps[len(steps)-maxSteps:]
	}
	return steps
}

// cleanStep flattens a prompt to a single tidy line and strips the local-command
// / attachment noise Claude wraps around slash-commands and pasted files.
func cleanStep(s string) string {
	s = oneLine(s)
	// Drop wrapper tags that aren't real user intent.
	for _, tag := range []string{
		"<local-command-caveat>", "</local-command-caveat>",
		"<command-name>", "</command-name>",
		"<command-message>", "</command-message>",
		"<command-args>", "</command-args>",
		"<local-command-stdout>", "</local-command-stdout>",
	} {
		s = strings.ReplaceAll(s, tag, " ")
	}
	// Strip image/attachment placeholders.
	s = strings.ReplaceAll(s, "[Image #1]", "")
	s = oneLine(s)
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return strings.TrimSpace(s)
}

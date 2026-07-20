package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// sendTimeout bounds a single --resume send. SendPrompt blocks until the resumed
// turn finishes (that's why a send feels slow — it's waiting for the whole reply,
// not just the delivery), so without a ceiling one stuck turn would wedge the
// serial queue behind it forever. Generous enough for genuinely long turns.
const sendTimeout = 20 * time.Minute

// There is no IPC into a live interactive session. Resuming by session id is
// the real mechanism: it continues the same conversation and appends to the
// same transcript, keeping the same session id. This mirrors PromptRunner.swift.

// findClaude looks on PATH first (true on Windows too, since the CLI is
// normally installed via npm and shimmed onto PATH there), then falls back
// to the common macOS/Linux install locations the HUD app already knew about.
func findClaude() (string, error) {
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}

	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, ".local", "bin", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	}
	if env := os.Getenv("CLAUDE_BIN"); env != "" {
		candidates = append([]string{env}, candidates...)
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("claude binary not found (set CLAUDE_BIN to override)")
}

// SendPrompt is blocking; call it off the UI goroutine.
func SendPrompt(prompt string, s Session) (string, error) {
	exe, err := findClaude()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "--resume", s.SessionID, "-p", "--output-format", "json")
	cmd.Dir = s.Cwd
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", errors.New("send timed out after " + sendTimeout.String() + " (the turn was still running)")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}

	var obj map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &obj); err != nil {
		return "", errors.New("could not parse claude output")
	}
	result, _ := obj["result"].(string)
	if result == "" {
		return "", errors.New("claude returned an empty result")
	}
	return result, nil
}

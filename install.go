package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The hook is registered in ~/.claude/settings.json. That file also holds the
// user's model, plugins, and permissions, so the installer is surgical: it
// reads the existing JSON, adds (or refreshes) only our one PreToolUse entry,
// and writes it back — after taking a timestamped backup the first time.

const hookMarker = "omni" // how we recognize our own hook entry

func settingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

// selfPath is the absolute path to this binary, used as the hook command so it
// works regardless of the user's PATH inside a session.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "omni"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved
	}
	return exe
}

// hookEntry is the matcher group we install: match every tool ("*") and run
// `omni hook`. A generous timeout so a real human has time to answer;
// the hook itself gives up sooner and defers, so this ceiling is rarely hit.
func hookEntry() map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": quoteIfNeeded(selfPath()) + " hook",
				"timeout": 120,
				"_source": hookMarker,
			},
		},
	}
}

// projectSettingsPath returns <projectDir>/.claude/settings.json, the file
// Claude re-reads per turn for a given repo — the right place for a hook scoped
// to just one project.
func projectSettingsPath(projectDir string) string {
	return filepath.Join(projectDir, ".claude", "settings.json")
}

// EnsureHookInstalled installs the hook globally (~/.claude/settings.json).
// Idempotent: safe to call on every dashboard launch.
func EnsureHookInstalled() (bool, error) {
	return installHookAt(settingsPath())
}

// EnsureHookInstalledIn installs the hook only into one project's settings,
// leaving the global config untouched.
func EnsureHookInstalledIn(projectDir string) (bool, error) {
	return installHookAt(projectSettingsPath(projectDir))
}

func installHookAt(path string) (bool, error) {
	settings := readSettings(path)

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	pre, _ := hooks["PreToolUse"].([]any)
	// Drop any prior omni entry so we can refresh the binary path, then
	// re-add. Leave every other PreToolUse hook untouched.
	var kept []any
	for _, group := range pre {
		if !isOurEntry(group) {
			kept = append(kept, group)
		}
	}

	desired := hookEntry()
	newPre := append(kept, desired)

	// If an identical entry was already present, don't rewrite the file.
	if len(pre) == len(newPre) && ourEntryMatches(pre, desired) {
		return false, nil
	}

	hooks["PreToolUse"] = newPre
	settings["hooks"] = hooks

	if err := writeSettings(path, settings); err != nil {
		return false, err
	}
	return true, nil
}

// UninstallHook removes our global entry.
func UninstallHook() (bool, error) {
	return uninstallHookAt(settingsPath())
}

// UninstallHookIn removes our entry from one project's settings.
func UninstallHookIn(projectDir string) (bool, error) {
	return uninstallHookAt(projectSettingsPath(projectDir))
}

func uninstallHookAt(path string) (bool, error) {
	settings := readSettings(path)
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return false, nil
	}
	pre, _ := hooks["PreToolUse"].([]any)
	var kept []any
	for _, group := range pre {
		if !isOurEntry(group) {
			kept = append(kept, group)
		}
	}
	if len(kept) == len(pre) {
		return false, nil // nothing of ours was there
	}
	if len(kept) == 0 {
		delete(hooks, "PreToolUse")
	} else {
		hooks["PreToolUse"] = kept
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	return true, writeSettings(path, settings)
}

func isOurEntry(group any) bool {
	g, ok := group.(map[string]any)
	if !ok {
		return false
	}
	hooks, ok := g["hooks"].([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if hm["_source"] == hookMarker {
			return true
		}
		// Also recognize by command, in case _source was stripped by an editor.
		if cmd, ok := hm["command"].(string); ok && containsSelf(cmd) {
			return true
		}
	}
	return false
}

func ourEntryMatches(pre []any, desired map[string]any) bool {
	for _, group := range pre {
		if !isOurEntry(group) {
			continue
		}
		a, _ := json.Marshal(group)
		b, _ := json.Marshal(desired)
		return string(a) == string(b)
	}
	return false
}

// containsSelf loosely matches "…/omni hook" so our entry is recognized
// even if the binary was moved or the _source marker was stripped by an editor.
func containsSelf(cmd string) bool {
	return strings.Contains(cmd, "omni") && strings.Contains(cmd, "hook")
}

// --- status queries ---

func hookInstalledAt(path string) bool {
	settings := readSettings(path)
	hooks, _ := settings["hooks"].(map[string]any)
	pre, _ := hooks["PreToolUse"].([]any)
	for _, g := range pre {
		if isOurEntry(g) {
			return true
		}
	}
	return false
}

// GlobalHookInstalled reports whether the hook is in ~/.claude/settings.json.
func GlobalHookInstalled() bool { return hookInstalledAt(settingsPath()) }

// ProjectHookInstalled reports whether the hook is in a project's settings.
func ProjectHookInstalled(projectDir string) bool {
	return hookInstalledAt(projectSettingsPath(projectDir))
}

// --- settings.json read / write with backup ---

func readSettings(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

func writeSettings(path string, m map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Back up the existing file once per day before our first change to it.
	if orig, err := os.ReadFile(path); err == nil {
		backup := path + ".omni-backup"
		if _, err := os.Stat(backup); os.IsNotExist(err) {
			_ = os.WriteFile(backup, orig, 0o644)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(data, '\n'))
}

// quoteIfNeeded wraps a path in quotes when it contains spaces, so the command
// string parses correctly on both POSIX shells and Windows.
func quoteIfNeeded(p string) string {
	if strings.Contains(p, " ") {
		return "\"" + p + "\""
	}
	return p
}

// installReport is printed by the CLI subcommands.
func installReport(changed bool, err error, action string) int {
	return installReportAt(changed, err, action, settingsPath())
}

func installReportAt(changed bool, err error, action, path string) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "omni: %s failed: %v\n", action, err)
		return 1
	}
	if changed {
		fmt.Printf("omni: hook %s in %s\n", action, path)
	} else {
		fmt.Printf("omni: hook already %s; no change\n", action)
	}
	return 0
}

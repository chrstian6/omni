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

// hookEvents are the two settings.json arrays we manage, and the subcommand
// each one runs:
//
//   - PreToolUse → `omni hook`, the permission gate.
//   - Stop       → `omni stop-hook`, which delivers prompts queued from the
//     dashboard into this very session, so a message sent from Omni is answered
//     by the terminal that owns the session rather than by a forked process.
//
// Both are installed and removed together — the dashboard's send path depends
// on the Stop hook being present, so a half-install would silently strand
// queued messages.
var hookEvents = []struct {
	event string
	sub   string
	// timeout in seconds; the permission gate may wait on a human, delivery is
	// a local file read and should never linger.
	timeout int
}{
	{"PreToolUse", "hook", 120},
	{"Stop", "stop-hook", 10},
}

// hookEntryFor builds the matcher group for one event: match everything ("*")
// and run the given subcommand.
func hookEntryFor(sub string, timeout int) map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": quoteIfNeeded(selfPath()) + " " + sub,
				"timeout": timeout,
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

	changed := false
	for _, ev := range hookEvents {
		existing, _ := hooks[ev.event].([]any)
		// Drop any prior omni entry so we can refresh the binary path, then
		// re-add. Leave every other hook on this event untouched.
		var kept []any
		for _, group := range existing {
			if !isOurEntry(group) {
				kept = append(kept, group)
			}
		}

		desired := hookEntryFor(ev.sub, ev.timeout)
		updated := append(kept, desired)

		// If an identical entry was already present, this event needs no rewrite.
		if len(existing) == len(updated) && ourEntryMatches(existing, desired) {
			continue
		}
		hooks[ev.event] = updated
		changed = true
	}
	if !changed {
		return false, nil
	}
	settings["hooks"] = hooks

	if err := writeSettings(path, settings); err != nil {
		return false, err
	}
	recordInstall(path) // remember it so quit can restore this file
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
	removed := false
	for _, ev := range hookEvents {
		existing, _ := hooks[ev.event].([]any)
		var kept []any
		for _, group := range existing {
			if !isOurEntry(group) {
				kept = append(kept, group)
			}
		}
		if len(kept) == len(existing) {
			continue // nothing of ours on this event
		}
		removed = true
		if len(kept) == 0 {
			delete(hooks, ev.event)
		} else {
			hooks[ev.event] = kept
		}
	}
	if !removed {
		return false, nil
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	forgetInstall(path)
	return true, writeSettings(path, settings)
}

// --- install tracking + restore-on-quit ---
//
// Every settings.json we add a hook to is recorded in ~/.omni/installs.json, so
// terminating the dashboard can put every one of those files back the way it was
// — remove our hook entry and delete the .omni-backup — even for a project that
// has no live session right now. This is what makes quitting leave no trace.

func installsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".omni", "installs.json")
}

func trackedInstalls() []string {
	data, err := os.ReadFile(installsPath())
	if err != nil {
		return nil
	}
	var paths []string
	if json.Unmarshal(data, &paths) != nil {
		return nil
	}
	return paths
}

func saveInstalls(paths []string) {
	_ = os.MkdirAll(filepath.Dir(installsPath()), 0o755)
	data, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(installsPath(), data, 0o644)
}

func recordInstall(path string) {
	for _, p := range trackedInstalls() {
		if p == path {
			return
		}
	}
	saveInstalls(append(trackedInstalls(), path))
}

func forgetInstall(path string) {
	var kept []string
	for _, p := range trackedInstalls() {
		if p != path {
			kept = append(kept, p)
		}
	}
	saveInstalls(kept)
}

// RestoreAllOnQuit removes every Omni hook entry we installed and deletes our
// settings backups, so terminating the dashboard restores settings.json to what
// it was before Omni. It covers every recorded install plus the global file and
// the given live-session projects as a safety net, and only ever removes our own
// entry — any other hooks the user has are left untouched. Returns the number of
// settings files it changed.
func RestoreAllOnQuit(projectDirs []string) int {
	seen := map[string]bool{}
	var paths []string
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		paths = append(paths, p)
	}
	add(settingsPath())
	for _, p := range trackedInstalls() {
		add(p)
	}
	for _, d := range projectDirs {
		if d != "" {
			add(projectSettingsPath(d))
		}
	}

	n := 0
	for _, p := range paths {
		if changed, err := uninstallHookAt(p); err == nil && changed {
			n++
		}
		_ = os.Remove(p + ".omni-backup") // drop our backup — nothing of ours left
	}
	saveInstalls(nil) // tracker is now empty
	return n
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

// hookInstalledAt reports whether BOTH our hooks are registered. It's all-or-
// nothing on purpose: with only PreToolUse present the permission gate works but
// prompts queued from the dashboard would never be delivered, so reporting that
// as "installed" would hide a broken send path.
func hookInstalledAt(path string) bool {
	settings := readSettings(path)
	hooks, _ := settings["hooks"].(map[string]any)
	for _, ev := range hookEvents {
		found := false
		for _, g := range asSlice(hooks[ev.event]) {
			if isOurEntry(g) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
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

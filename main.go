package main

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	args := os.Args[1:]

	// Subcommands. `hook` must come first and stay silent on stdout except for
	// its JSON verdict — Claude parses that.
	if len(args) > 0 {
		switch args[0] {
		case "hook":
			os.Exit(runHook())
		case "open", "window":
			// Launch the dashboard in its own new, resizable, minimizable window.
			ensureExecutable(selfPath())
			if err := openInNewWindow(); err != nil {
				fmt.Fprintln(os.Stderr, "omni: could not open a new window:", err)
				os.Exit(1)
			}
			fmt.Println("omni: opened in a new window")
			return
		case "install-hook":
			// Optional project path scopes the hook to one repo's
			// .claude/settings.json instead of the global config.
			if len(args) > 1 {
				dir := absOr(args[1])
				changed, err := EnsureHookInstalledIn(dir)
				os.Exit(installReportAt(changed, err, "installed", projectSettingsPath(dir)))
			}
			changed, err := EnsureHookInstalled()
			os.Exit(installReport(changed, err, "installed"))
		case "uninstall-hook":
			if len(args) > 1 {
				dir := absOr(args[1])
				changed, err := UninstallHookIn(dir)
				os.Exit(installReportAt(changed, err, "uninstalled", projectSettingsPath(dir)))
			}
			changed, err := UninstallHook()
			os.Exit(installReport(changed, err, "uninstalled"))
		case "-v", "--version":
			fmt.Println("omni terminal 1.0")
			return
		case "-h", "--help":
			fmt.Print(helpText)
			return
		case "--project", "-p":
			// Launch the dashboard with the hook scoped to one project only —
			// the global config is left untouched.
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "omni: --project needs a directory")
				os.Exit(1)
			}
			dir := absOr(args[1])
			if changed, err := EnsureHookInstalledIn(dir); err != nil {
				fmt.Fprintf(os.Stderr, "omni: could not install hook in %s: %v\n", dir, err)
			} else if changed {
				fmt.Printf("omni: installed permission hook into %s\n", projectSettingsPath(dir))
			}
			runDashboard(false)
			return
		}
	}

	// Bare launch just runs the dashboard. It does NOT force a global hook
	// install — the hook is scoped explicitly (omni install-hook [project]),
	// so opening Omni never silently changes your global config.
	runDashboard(false)
}

// runDashboard launches the TUI. autoInstall controls whether it installs the
// global hook on startup; when the user scoped the hook to a project, we skip
// the global install so nothing outside that project is touched.
func runDashboard(autoInstall bool) {
	if autoInstall {
		if changed, err := EnsureHookInstalled(); err == nil && changed {
			fmt.Println("omni: installed permission hook into ~/.claude/settings.json")
		}
	}

	// AltScreen for a clean full-window canvas. We deliberately do NOT grab the
	// mouse: with mouse reporting off, the terminal's own click-drag selection
	// keeps working, so conversation text stays selectable and copyable — the
	// Claude-Code feel. Navigation is all keyboard (j/k, J/K, tab, 1-4).
	p := tea.NewProgram(NewModel(), tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "omni:", err)
		os.Exit(1)
	}
}

// absOr resolves a path to absolute, falling back to the input on error.
func absOr(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

const helpText = `omni — a terminal dashboard for every running Claude Code session

Reads ~/.claude/sessions, so every session started from any terminal or IDE
(VS Code, Cursor, a plain shell) shows up in one place. Pick one and prompt it,
watch what each is doing, and approve or block the permissions they ask for —
with a safety layer that hard-blocks catastrophic actions automatically.

Usage:
  omni                      launch the dashboard in this terminal
  omni open                 launch it in its own new, resizable window
  omni install-hook [dir]   install the permission hook (global, or scoped to dir)
  omni uninstall-hook [dir] remove the permission hook (global, or from dir)
  omni --project <dir>      run the dashboard with the hook scoped to one project
  omni hook                 (internal) the PreToolUse handler
  omni --version            print version
  omni --help               this message

Dashboard keys:
  tab          switch between Sessions and Permissions
  ↑/↓ or j/k   move
  enter or p   prompt the selected session (Sessions view)
  a / d        approve / deny the selected request (Permissions view)
  A            approve all non-flagged pending requests
  g            toggle global auto-approve (flagged actions still ask)
  n            rename a session
  x            end an idle session
  t            cycle the idle threshold
  r            refresh
  q            quit
`

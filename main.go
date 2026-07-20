package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

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
		case "stop-hook":
			// Runs inside a live session when it finishes a turn; delivers anything
			// the dashboard queued for that session. Same stdout discipline as
			// `hook` — Claude parses the JSON we print.
			os.Exit(runStopHook())
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
		case "telegram-setup":
			// omni telegram-setup <botToken> <chatID> [--all]
			// Verifies by sending a real message, so a wrong token or chat id
			// fails here rather than silently at 2am when something is blocked.
			if len(args) < 3 {
				fmt.Fprintln(os.Stderr, "usage: omni telegram-setup <botToken> <chatID> [--all]")
				fmt.Fprintln(os.Stderr, "  create a bot with @BotFather; get your chat id from @userinfobot")
				fmt.Fprintln(os.Stderr, "  --all  send every pending request, not only flagged ones")
				os.Exit(1)
			}
			chatID, err := strconv.ParseInt(args[2], 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, "omni: chatID must be a number:", args[2])
				os.Exit(1)
			}
			cfg := TelegramConfig{
				BotToken:  args[1],
				ChatID:    chatID,
				NotifyAll: len(args) > 3 && args[3] == "--all",
				Enabled:   true,
			}
			if err := VerifyTelegram(cfg); err != nil {
				fmt.Fprintln(os.Stderr, "omni: could not reach Telegram:", err)
				os.Exit(1)
			}
			if err := cfg.Save(); err != nil {
				fmt.Fprintln(os.Stderr, "omni: could not save config:", err)
				os.Exit(1)
			}
			fmt.Println("omni: telegram bridge configured — check your phone for the test message")
			fmt.Println("      config saved (owner-only) to", telegramConfigPath())
			return
		case "tmux-new":
			// omni tmux-new <dir> [name] — start a claude session inside tmux so it
			// can be answered remotely even while sitting idle at its prompt.
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: omni tmux-new <dir> [session-name]")
				os.Exit(1)
			}
			dir := absOr(args[1])
			name := filepath.Base(dir)
			if len(args) > 2 {
				name = args[2]
			}
			claudeBin, err := findClaude()
			if err != nil {
				fmt.Fprintln(os.Stderr, "omni:", err)
				os.Exit(1)
			}
			if err := TmuxNewSession(name, dir, claudeBin); err != nil {
				fmt.Fprintln(os.Stderr, "omni: could not start tmux session:", err)
				os.Exit(1)
			}
			fmt.Printf("omni: started claude in tmux session %q (%s)\n", name, dir)
			fmt.Printf("      attach with:  tmux attach -t %s\n", name)
			fmt.Println("      it is now answerable from your phone, even when idle")
			return
		case "telegram-off":
			cfg, err := LoadTelegramConfig()
			if err != nil {
				fmt.Fprintln(os.Stderr, "omni: no telegram config to disable")
				os.Exit(1)
			}
			cfg.Enabled = false
			if err := cfg.Save(); err != nil {
				fmt.Fprintln(os.Stderr, "omni:", err)
				os.Exit(1)
			}
			fmt.Println("omni: telegram bridge disabled (token kept; re-enable with telegram-setup)")
			return
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

	// If the window is closed or the process is told to terminate (SIGTERM/SIGHUP)
	// rather than quit with the key, still restore every settings.json we touched
	// so we never leave a hook behind. In-app quit (q / ctrl+c) is handled by the
	// model itself, so we deliberately don't listen for SIGINT here.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		RestoreAllOnQuit(nil)
		removeHeartbeat()
		os.Exit(0)
	}()

	// Phone bridge: if configured, flagged permission requests are mirrored to
	// Telegram and can be answered from there. It runs alongside the TUI and
	// writes the same decision files the dashboard does, so an approval from the
	// phone and one from the desk are indistinguishable to the waiting hook.
	stopBridge := make(chan struct{})
	defer close(stopBridge)
	if cfg, err := LoadTelegramConfig(); err == nil && cfg.Enabled {
		go NewTelegramBridge(cfg).Run(stopBridge)
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
  omni telegram-setup <token> <chatID> [--all]
                            mirror flagged permissions to your phone
  omni telegram-off         disable the phone bridge
  omni tmux-new <dir> [name]
                            start claude inside tmux (answerable from phone
                            even when idle)
  omni hook                 (internal) the PreToolUse permission handler
  omni stop-hook            (internal) delivers queued prompts into a session
  omni --version            print version
  omni --help               this message

Dashboard keys:
  tab          switch between Sessions and Permissions
  ↑/↓ or j/k   move
  enter or p   prompt the selected session (Sessions view)
  f            focus the terminal/IDE window that owns the session
  a / d        approve / deny the selected request (Permissions view)
  A            approve all non-flagged pending requests
  g            toggle global auto-approve (flagged actions still ask)
  n            rename a session
  x            end an idle session
  t            cycle the idle threshold
  r            refresh
  q            quit (restores settings.json — removes every hook Omni added)
`

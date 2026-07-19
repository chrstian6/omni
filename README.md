# Omni

A cross-platform terminal dashboard for every running Claude Code session. One
place to see all your sessions across every terminal and IDE, prompt any of
them, approve or block the permissions they ask for, and manage your skills and
agents.

Runs in any native terminal on **macOS, Windows, and Linux** — a single
dependency-free binary, no runtime to install.

## Build

```
go build -o omni ./...        # current platform
./build.sh                    # all platforms into dist/
```

Requires Go 1.23+.

## Run

```
omni                 # launch the dashboard (auto-installs the permission hook)
omni install-hook    # install the permission hook only
omni uninstall-hook  # remove it
omni --help
```

## What it does

**Sessions** — reads `~/.claude/sessions`, so every session started anywhere
(VS Code, Cursor, a plain shell, over SSH) shows up in one list, tagged with the
app it's running in (detected by walking each process's parent chain). See what
each is doing — current task, todo list, last prompt, and a **step-by-step
summary** of every prompt the session went through, up to the most recent.
Prompt any idle one, give sessions friendly names, and end abandoned ones.

Sessions are also **saved**: Omni keeps a durable record of every session it has
seen in `~/.omni/history.json`, so ones that have ended still appear under
"recently ended" with their summary intact. Because `claude --resume` works on a
finished session's transcript, you can select an ended session and **resume it**
— prompt it straight back to life — or forget it from the history.

The dashboard is a **two-pane, mouse-driven layout**: click the tabs along the
top, click any session in the left sidebar, and read its full detail — including
the **entire conversation** (every You/Claude turn, with a compact line for each
tool call) — in the right panel. Scroll the conversation with the wheel or
`J`/`K`. Everything also works from the keyboard.

**Permission hook — your choice of scope.** Omni never installs anything
globally on its own. For the selected session you can install the hook for
**just that project** (`H`) or **globally** (`G`), and the detail panel always
shows where it's currently active. From the CLI: `omni install-hook` (global) or
`omni install-hook <dir>` (one project).

**Permissions** — when a session asks to run a tool, it appears here for one-key
approve/deny, with an "approve all" for routine work and a global auto-approve
toggle. A **safety layer** classifies every tool call first:

- *Block* — catastrophic actions (`rm -rf /`, force-push to a protected branch,
  `DROP DATABASE`, `curl | sh`, disk formatting…) are denied automatically and
  logged to `~/.omni/blocked.log`. The session never even asks.
- *Flag* — risky-but-legitimate actions (any `sudo`, `git push`, writing to a
  `.env`…) are shown with a **DANGER** badge and always need an explicit,
  individual approval — "approve all" never covers them.

This works through a `PreToolUse` hook Omni installs into `~/.claude/settings.json`
(existing settings are preserved and backed up). The hook is **fail-safe**: if
no dashboard is running it defers to Claude's normal prompt in milliseconds, so
installing it globally never hangs a session.

**Skills** — a skills inventory. Lists what's installed under `~/.claude/skills`
and what's sitting in `~/Downloads` (a folder with a `SKILL.md`, or a `.zip`
containing one). Install with one key.

**Agents** — lists installed agents from `~/.claude/agents`, installs agent
files from `~/Downloads`, and authors new ones from a built-in form.

## Keys

```
tab / 1-4      switch tabs
↑↓ or j/k      move
enter          primary action (prompt / approve / install)
Sessions:      n rename · x end idle · t idle threshold
Permissions:   a approve · d deny · A approve all · g global auto-approve
Skills/Agents: i install · x remove · c create agent
r refresh · q quit
```

## Files it uses

```
~/.claude/sessions/       session registry (read)
~/.claude/settings.json   the permission hook (installed here)
~/.claude/skills/         skills (read/install)
~/.claude/agents/         agents (read/install/create)
~/.omni/                  Omni's own state: pending requests, decisions,
                          policy, heartbeat, blocked.log audit
```

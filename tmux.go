package main

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// tmux is the one mechanism that can reach a session sitting idle at its prompt.
//
// Everything else in Omni delivers by queueing and waiting for the Stop hook,
// which only fires when a turn ENDS. A session that has stopped to ask you a
// question has already fired it, so a queued answer waits forever. `tmux
// send-keys` sidesteps that entirely by typing into the pane — the same thing
// your fingers would do. The terminal stays the single writer, the transcript
// keeps one author, and it works whether the session is busy or idle.
//
// It needs no Accessibility permission and doesn't care which window has focus,
// which is why it beats AppleScript keystroke injection — especially for VS
// Code, whose integrated terminal has no scripting API.
//
// The catch, and the reason this isn't the only path: the session has to have
// been started inside tmux. Sessions started in a bare terminal have no pane to
// address, and fall back to the outbox.

type tmuxPane struct {
	ID  string // e.g. "%3" — stable, unambiguous pane address
	PID int32  // the pane's root process (usually the shell)
}

// tmuxPath finds the binary. Omni is often launched from Finder, where PATH is
// minimal and Homebrew's bin is absent, so PATH alone isn't enough.
func tmuxPath() (string, error) {
	if p, err := exec.LookPath("tmux"); err == nil {
		return p, nil
	}
	for _, c := range []string{
		"/opt/homebrew/bin/tmux",
		"/usr/local/bin/tmux",
		"/usr/bin/tmux",
	} {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	return "", errors.New("tmux not found")
}

func tmuxAvailable() bool {
	_, err := tmuxPath()
	return err == nil
}

// tmuxPanes lists every pane across every tmux server session.
func tmuxPanes() []tmuxPane {
	exe, err := tmuxPath()
	if err != nil {
		return nil
	}
	out, err := exec.Command(exe, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid}").Output()
	if err != nil {
		return nil // no server running is the common case, not an error
	}
	var panes []tmuxPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		id, pidStr, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(pidStr))
		if err != nil {
			continue
		}
		panes = append(panes, tmuxPane{ID: id, PID: int32(pid)})
	}
	return panes
}

// paneForPID finds the pane a process is running in, by walking up its parent
// chain until it meets a pane's root process.
//
// Matching on ancestry rather than on the pane's own command is what makes this
// safe: we start from the known claude pid and find the pane containing it, so
// we can never address a pane that merely looks similar. If the walk finds
// nothing, the session isn't in tmux and the caller must not guess.
func paneForPID(pid int32, table map[int32]procInfo, panes []tmuxPane) (string, bool) {
	byPID := make(map[int32]string, len(panes))
	for _, p := range panes {
		byPID[p.PID] = p.ID
	}
	seen := map[int32]bool{}
	cur := pid
	for depth := 0; depth < 24; depth++ {
		if id, ok := byPID[cur]; ok {
			return id, true
		}
		info, ok := table[cur]
		if !ok || seen[cur] {
			break
		}
		seen[cur] = true
		if info.ppid <= 1 || info.ppid == cur {
			break
		}
		cur = info.ppid
	}
	return "", false
}

// PaneForSession resolves the tmux pane a live session occupies, if any.
func PaneForSession(s Session) (string, bool) {
	if !tmuxAvailable() {
		return "", false
	}
	panes := tmuxPanes()
	if len(panes) == 0 {
		return "", false
	}
	return paneForPID(s.PID, procTable(), panes)
}

// TmuxSendLine types text into a pane and presses Enter — exactly what you'd do
// at the keyboard.
//
// Sent with -l (literal) so the text is never interpreted as tmux key names:
// without it a message containing "Enter", "C-c" or a semicolon would be read as
// keystrokes rather than characters. Enter is a separate call for the same
// reason.
//
// Newlines are flattened to spaces because a raw newline mid-text would submit
// the message early, delivering half of it. Multi-line input needs the terminal.
func TmuxSendLine(paneID, text string) error {
	exe, err := tmuxPath()
	if err != nil {
		return err
	}
	text = strings.TrimSpace(oneLine(text))
	if text == "" {
		return errors.New("nothing to send")
	}
	if paneID == "" {
		return errors.New("no pane to send to")
	}
	if out, err := exec.Command(exe, "send-keys", "-t", paneID, "-l", text).CombinedOutput(); err != nil {
		return errors.New("tmux send-keys: " + strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(exe, "send-keys", "-t", paneID, "Enter").CombinedOutput(); err != nil {
		return errors.New("tmux send-keys Enter: " + strings.TrimSpace(string(out)))
	}
	return nil
}

// TmuxNewSession starts a claude session inside a new detached tmux session, so
// it is remotely addressable from the moment it exists. Returns the tmux session
// name for attaching.
func TmuxNewSession(name, dir, claudeBin string) error {
	exe, err := tmuxPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "new-session", "-d", "-s", name, "-c", dir, claudeBin)
	if out, err := cmd.CombinedOutput(); err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

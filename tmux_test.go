package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Pane resolution decides where keystrokes land. Sending "1" into the wrong
// pane types into someone else's shell, so the negative cases matter as much as
// the positive one.

func TestPaneForPIDWalksAncestryToThePane(t *testing.T) {
	// pane(100) -> shell(200) -> claude(300)
	table := map[int32]procInfo{
		300: {ppid: 200, comm: "claude"},
		200: {ppid: 100, comm: "zsh"},
		100: {ppid: 1, comm: "sh"},
	}
	panes := []tmuxPane{{ID: "%7", PID: 100}}

	got, ok := paneForPID(300, table, panes)
	if !ok || got != "%7" {
		t.Fatalf("got (%q,%v), want (%q,true)", got, ok, "%7")
	}
}

func TestPaneForPIDReturnsPaneWhenProcessIsThePaneItself(t *testing.T) {
	table := map[int32]procInfo{100: {ppid: 1, comm: "claude"}}
	panes := []tmuxPane{{ID: "%1", PID: 100}}
	if got, ok := paneForPID(100, table, panes); !ok || got != "%1" {
		t.Fatalf("got (%q,%v), want (%%1,true)", got, ok)
	}
}

// A process outside tmux must resolve to nothing. If this ever returns a pane,
// Omni would type into a terminal that has nothing to do with the session.
func TestPaneForPIDRefusesProcessesOutsideTmux(t *testing.T) {
	table := map[int32]procInfo{
		300: {ppid: 200, comm: "claude"},
		200: {ppid: 1, comm: "login"}, // ancestry never reaches a pane
		100: {ppid: 1, comm: "sh"},
	}
	panes := []tmuxPane{{ID: "%7", PID: 100}}

	if got, ok := paneForPID(300, table, panes); ok {
		t.Fatalf("resolved a non-tmux process to pane %q", got)
	}
}

func TestPaneForPIDHandlesNoPanesAndCycles(t *testing.T) {
	table := map[int32]procInfo{300: {ppid: 200}, 200: {ppid: 300}} // cycle
	if _, ok := paneForPID(300, table, nil); ok {
		t.Error("resolved a pane when none exist")
	}
	if _, ok := paneForPID(300, table, []tmuxPane{{ID: "%1", PID: 999}}); ok {
		t.Error("resolved despite a parent cycle and no matching pane")
	}
}

func TestTmuxSendLineRejectsEmptyInput(t *testing.T) {
	if err := TmuxSendLine("%0", "   "); err == nil {
		t.Error("empty text should be rejected rather than sending a bare Enter")
	}
	if err := TmuxSendLine("", "hello"); err == nil {
		t.Error("empty pane id should be rejected")
	}
}

// End-to-end against a real tmux server: text typed into a pane must arrive
// verbatim, including characters tmux would otherwise read as key names.
func TestTmuxSendLineDeliversLiterally(t *testing.T) {
	tm, err := tmuxPath()
	if err != nil {
		t.Skip("tmux not installed")
	}
	const sess = "omni-selftest"
	_ = exec.Command(tm, "kill-session", "-t", sess).Run()

	out := t.TempDir() + "/captured.txt"
	if o, err := exec.Command(tm, "new-session", "-d", "-s", sess, "cat > "+out).CombinedOutput(); err != nil {
		t.Skipf("could not start tmux session: %v %s", err, o)
	}
	t.Cleanup(func() { _ = exec.Command(tm, "kill-session", "-t", sess).Run() })
	time.Sleep(400 * time.Millisecond)

	// "Enter" and "C-c" would be interpreted as keystrokes without -l.
	const msg = `option 1: keep "Infobip" & press Enter; C-c $HOME`
	if err := TmuxSendLine(sess, msg); err != nil {
		t.Fatalf("TmuxSendLine: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	_ = exec.Command(tm, "send-keys", "-t", sess, "C-d").Run()
	time.Sleep(400 * time.Millisecond)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading captured output: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != msg {
		t.Errorf("text was altered in transit:\n got %q\nwant %q", got, msg)
	}
}

// The realistic shape — pane -> shell -> command — must resolve, since a claude
// process is always a descendant of the pane's shell rather than the pane pid.
func TestPaneResolutionAgainstRealNestedProcess(t *testing.T) {
	tm, err := tmuxPath()
	if err != nil {
		t.Skip("tmux not installed")
	}
	const sess = "omni-nesttest"
	_ = exec.Command(tm, "kill-session", "-t", sess).Run()
	if o, err := exec.Command(tm, "new-session", "-d", "-s", sess).CombinedOutput(); err != nil {
		t.Skipf("could not start tmux session: %v %s", err, o)
	}
	t.Cleanup(func() { _ = exec.Command(tm, "kill-session", "-t", sess).Run() })
	time.Sleep(400 * time.Millisecond)
	if err := TmuxSendLine(sess, "sleep 60"); err != nil {
		t.Fatalf("TmuxSendLine: %v", err)
	}
	time.Sleep(700 * time.Millisecond)

	panes := tmuxPanes()
	var nested int32
	for _, p := range panes {
		out, _ := exec.Command("pgrep", "-P", strconv.Itoa(int(p.PID))).Output()
		for _, f := range strings.Fields(string(out)) {
			if n, err := strconv.Atoi(f); err == nil {
				nested = int32(n)
			}
		}
	}
	if nested == 0 {
		t.Skip("no nested child appeared")
	}
	if _, ok := paneForPID(nested, procTable(), panes); !ok {
		t.Errorf("nested pid %d did not resolve to its pane", nested)
	}
	// And this test's own process, which is not in tmux, must not resolve.
	if pane, ok := paneForPID(int32(os.Getpid()), procTable(), panes); ok {
		t.Errorf("a process outside tmux resolved to pane %q", pane)
	}
}

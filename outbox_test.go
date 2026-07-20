package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The outbox is the contract between the dashboard and the Stop hook running
// inside a live session, so its ordering and drain behavior are what keep a
// prompt from being lost or delivered twice.

// Arrival order must survive messages queued back-to-back with no delay — the
// realistic case of typing several prompts quickly. Enough of them that any
// same-timestamp tiebreak that isn't arrival-ordered will show up.
func TestOutboxDeliversInArrivalOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const sid = "sess-1"

	var want []string
	for i := 0; i < 50; i++ {
		text := "msg-" + itoa(i)
		want = append(want, text)
		enqueueOrFail(t, sid, text)
	}

	got := loadOutbox(sid)
	if len(got) != len(want) {
		t.Fatalf("queued %d, loaded %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Text != want[i] {
			t.Fatalf("position %d: got %q, want %q (order not preserved)", i, got[i].Text, want[i])
		}
	}
}

// Draining is what makes the Stop hook loop-safe: blocking the stop makes Claude
// respond and fire Stop again, and the second call must find nothing left.
func TestOutboxDrainLeavesNothingBehind(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const sid = "sess-2"

	enqueueOrFail(t, sid, "one")
	enqueueOrFail(t, sid, "two")

	for _, m := range loadOutbox(sid) {
		removeOutbox(sid, m.ID)
	}
	if n := outboxCount(sid); n != 0 {
		t.Fatalf("after drain, %d messages still queued", n)
	}
}

func TestOutboxIgnoresBlankMessages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const sid = "sess-3"

	if _, err := enqueueOutbox(sid, "   \n\t "); err != nil {
		t.Fatalf("enqueue blank: %v", err)
	}
	if n := outboxCount(sid); n != 0 {
		t.Fatalf("blank message was queued (%d present)", n)
	}
}

// A queued message is a promise to deliver into a live process. When that
// process is gone the promise can't be kept, so the queue must not survive and
// mislead the UI into showing a pending delivery.
func TestPruneOutboxDropsQueuesForDeadSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	enqueueOrFail(t, "alive", "keep me")
	enqueueOrFail(t, "dead", "drop me")

	pruneOutbox([]Session{{SessionID: "alive"}})

	if n := outboxCount("alive"); n != 1 {
		t.Errorf("live session's queue was pruned (%d left, want 1)", n)
	}
	if n := outboxCount("dead"); n != 0 {
		t.Errorf("dead session's queue survived (%d left, want 0)", n)
	}
}

// Session ids are used as directory names, so anything path-like in one must not
// let a queue escape the outbox directory.
func TestOutboxSessionIDCannotEscapeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	enqueueOrFail(t, "../../escape", "payload")

	dir := outboxSessionDir("../../escape")
	if rel, err := filepath.Rel(outboxDir(), dir); err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) > 1 && rel[:2] == ".." {
		t.Fatalf("session dir %q escaped outbox root %q", dir, outboxDir())
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("queue not written where expected: %v", err)
	}
}

func enqueueOrFail(t *testing.T, sid, text string) {
	t.Helper()
	if _, err := enqueueOutbox(sid, text); err != nil {
		t.Fatalf("enqueue %q for %s: %v", text, sid, err)
	}
}

// A menu the user already answered must be recognized on the next refresh (the
// transcript still shows it), while a genuinely different menu must not be — the
// difference between suppressing a duplicate pick and swallowing a real one.
func TestChoicesKeyIdentifiesTheSameMenu(t *testing.T) {
	menu := []choiceOpt{{marker: "1", label: "Merge"}, {marker: "2", label: "Rebase"}}
	same := []choiceOpt{{marker: "1", label: "Merge"}, {marker: "2", label: "Rebase"}}
	other := []choiceOpt{{marker: "1", label: "Merge"}, {marker: "2", label: "Squash"}}

	if choicesKey(menu) != choicesKey(same) {
		t.Error("identical menus produced different keys")
	}
	if choicesKey(menu) == choicesKey(other) {
		t.Error("different menus collided on the same key")
	}
	if choicesKey(nil) != "" {
		t.Error("empty menu must produce an empty key so it never matches")
	}
}

// The delivered text has to identify itself as the user's message; a bare "1"
// arriving as tooling output is what made the first version unreadable.
func TestFramePromptLabelsTheSender(t *testing.T) {
	got := framePrompt([]string{"1. Merge"})
	if !strings.Contains(got, "1. Merge") {
		t.Errorf("payload lost the message body: %q", got)
	}
	if !strings.Contains(got, "user") {
		t.Errorf("payload does not attribute the message to the user: %q", got)
	}
}

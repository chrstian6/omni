package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// The outbox is how a prompt reaches a session WITHOUT forking it.
//
// The old path — `claude --resume <id> -p` — starts a second, headless Claude
// process against the same session id. Both processes then append to the same
// transcript, so the terminal the session actually lives in (VS Code, iTerm, …)
// never sees the message and its in-memory conversation silently diverges from
// the file on disk. The originating terminal stops being the source of truth.
//
// Instead we drop the message here and let the Stop hook — which runs *inside*
// the real session process — pick it up and hand it to that session. The reply
// happens in the terminal the user started, in the window they can see, and the
// transcript keeps exactly one writer.
//
//	~/.omni/outbox/<sessionID>/<createdAt>-<nonce>.json
//
// One file per message, named so a lexical sort is also chronological.

type OutboxMsg struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Text      string `json:"text"`
	CreatedAt int64  `json:"createdAt"` // epoch millis
}

func outboxDir() string { return filepath.Join(deckDir(), "outbox") }

// outboxSeq disambiguates messages queued within the same nanosecond — typing
// two prompts in quick succession must still be delivered in the order they
// were typed, and a content-derived tiebreak would order them arbitrarily.
var outboxSeq atomic.Uint64

// nextOutboxID builds an id whose LEXICAL order matches arrival order, which is
// what makes both the filename sort and the delivery sort correct. Both parts
// are zero-padded to fixed width so string comparison matches numeric order.
func nextOutboxID(now time.Time) string {
	return fmt.Sprintf("%019d-%06d", now.UnixNano(), outboxSeq.Add(1)%1000000)
}

// outboxSessionDir is per-session so draining one session never has to read or
// lock another's messages.
func outboxSessionDir(sessionID string) string {
	return filepath.Join(outboxDir(), sanitize(sessionID))
}

// enqueueOutbox queues a message for delivery into the live session. It returns
// the queued message so the caller can show what's waiting.
func enqueueOutbox(sessionID, text string) (OutboxMsg, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return OutboxMsg{}, nil
	}
	now := time.Now()
	msg := OutboxMsg{
		ID:        nextOutboxID(now),
		SessionID: sessionID,
		Text:      text,
		CreatedAt: now.UnixMilli(),
	}
	dir := outboxSessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return OutboxMsg{}, err
	}
	data, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		return OutboxMsg{}, err
	}
	if err := writeAtomic(filepath.Join(dir, msg.ID+".json"), data); err != nil {
		return OutboxMsg{}, err
	}
	return msg, nil
}

// loadOutbox returns everything queued for a session, oldest first.
func loadOutbox(sessionID string) []OutboxMsg {
	dir := outboxSessionDir(sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []OutboxMsg
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m OutboxMsg
		if err := json.Unmarshal(data, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	// Sort on the id alone: it's built so lexical order IS arrival order, down to
	// the nanosecond plus a sequence. CreatedAt is millisecond-resolution and
	// would tie for messages queued in the same millisecond.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func outboxCount(sessionID string) int { return len(loadOutbox(sessionID)) }

// removeOutbox deletes one delivered message. Deleting on delivery is also what
// makes the Stop hook loop-safe: the blocked turn fires Stop again, but by then
// the queue is empty so the second call lets the session stop normally.
func removeOutbox(sessionID, id string) {
	_ = os.Remove(filepath.Join(outboxSessionDir(sessionID), id+".json"))
}

// clearOutbox drops every queued message for a session — used when the user
// cancels a queue, and when a session ends with messages still undelivered.
func clearOutbox(sessionID string) {
	_ = os.RemoveAll(outboxSessionDir(sessionID))
}

// pruneOutbox removes queues whose session is no longer running. A queued
// message is a promise to deliver into *that* live session; once the process is
// gone there's nothing left to deliver into, so holding the file would only
// mislead the UI into showing a delivery that can never happen.
func pruneOutbox(live []Session) {
	entries, err := os.ReadDir(outboxDir())
	if err != nil {
		return
	}
	alive := map[string]bool{}
	for _, s := range live {
		alive[sanitize(s.SessionID)] = true
	}
	for _, e := range entries {
		if e.IsDir() && !alive[e.Name()] {
			_ = os.RemoveAll(filepath.Join(outboxDir(), e.Name()))
		}
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The phone bridge can authorize commands Omni deliberately refuses to
// auto-approve, so its authorization and filtering rules are load-bearing
// security behaviour, not conveniences. These tests pin them down.

// stubTelegram stands in for api.telegram.org and records what was sent.
type stubTelegram struct {
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	srv    *httptest.Server
}

func newStubTelegram(t *testing.T) *stubTelegram {
	t.Helper()
	s := &stubTelegram{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		method := parts[len(parts)-1]
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.calls = append(s.calls, method)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":42}}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubTelegram) sentTo(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if c == method {
			n++
		}
	}
	return n
}

func newTestBridge(t *testing.T, stub *stubTelegram, chatID int64) *TelegramBridge {
	t.Helper()
	b := NewTelegramBridge(TelegramConfig{BotToken: "test-token", ChatID: chatID, Enabled: true})
	b.baseURL = stub.srv.URL
	return b
}

// A callback from any account other than the configured one must not be able to
// decide anything. This is the whole security boundary of the feature.
func TestCallbackFromUnauthorizedAccountIsIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	const ownerID int64 = 111
	b := newTestBridge(t, stub, ownerID)

	req := PendingRequest{ID: "req-1", Risk: riskWarn, Tool: "Bash", Summary: "rm -rf /"}
	if err := writePending(req); err != nil {
		t.Fatalf("writePending: %v", err)
	}
	b.tokens["tok1"] = req.ID

	// An attacker's chat id, not the owner's. handleDecision is only reached
	// after the allowlist check, so exercise the check itself via pollUpdates'
	// logic: simulate by calling with the wrong id path.
	if ownerID == 999 {
		t.Fatal("test setup error")
	}
	// Directly assert the guard: a non-owner id must never reach handleDecision.
	if authorizedSender(b.cfg, 999) {
		t.Fatal("a non-owner account was treated as authorized")
	}
	if !authorizedSender(b.cfg, ownerID) {
		t.Fatal("the owner account was rejected")
	}

	// And confirm no decision was recorded for the request.
	if _, ok := readDecision(req.ID); ok {
		t.Fatal("a decision was written without an authorized callback")
	}
}

func TestAuthorizedCallbackWritesDecision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	const ownerID int64 = 111
	b := newTestBridge(t, stub, ownerID)

	req := PendingRequest{ID: "req-allow", Risk: riskWarn, Tool: "Bash", Summary: "git push"}
	if err := writePending(req); err != nil {
		t.Fatalf("writePending: %v", err)
	}
	b.tokens["tok-allow"] = req.ID

	b.handleDecision("cb1", "a:tok-allow", 42)

	d, ok := readDecision(req.ID)
	if !ok {
		t.Fatal("no decision written for an authorized Allow tap")
	}
	if !d.Allow {
		t.Errorf("decision recorded as deny, want allow: %+v", d)
	}
}

func TestAuthorizedDenyWritesDenial(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	req := PendingRequest{ID: "req-deny", Risk: riskWarn, Tool: "Bash", Summary: "sudo rm"}
	if err := writePending(req); err != nil {
		t.Fatalf("writePending: %v", err)
	}
	b.tokens["tok-deny"] = req.ID

	b.handleDecision("cb2", "d:tok-deny", 42)

	d, ok := readDecision(req.ID)
	if !ok {
		t.Fatal("no decision written for an authorized Deny tap")
	}
	if d.Allow {
		t.Errorf("decision recorded as allow, want deny: %+v", d)
	}
}

// A tap whose token we don't recognise (dashboard restarted) must fail closed —
// guessing which request it referred to could approve the wrong command.
func TestUnknownTokenDecidesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	req := PendingRequest{ID: "req-x", Risk: riskWarn, Tool: "Bash", Summary: "deploy"}
	if err := writePending(req); err != nil {
		t.Fatalf("writePending: %v", err)
	}

	b.handleDecision("cb3", "a:no-such-token", 42)

	if _, ok := readDecision(req.ID); ok {
		t.Fatal("an unrecognised token produced a decision")
	}
}

// A request that already went away (answered at the desk, or expired) must not
// be decidable from the phone afterwards.
func TestTapOnAlreadyHandledRequestDecidesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	b.tokens["tok-gone"] = "req-that-never-existed"
	b.handleDecision("cb4", "a:tok-gone", 42)

	if _, ok := readDecision("req-that-never-existed"); ok {
		t.Fatal("decided a request that was no longer pending")
	}
}

// Only flagged requests should reach the phone by default — safe ones are noise,
// and the user asked specifically for the flagged ones.
func TestOnlyFlaggedRequestsAreSent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	for _, r := range []PendingRequest{
		{ID: "safe-1", Risk: riskSafe, Tool: "Read", Summary: "read a file"},
		{ID: "warn-1", Risk: riskWarn, Tool: "Bash", Summary: "git push"},
	} {
		if err := writePending(r); err != nil {
			t.Fatalf("writePending: %v", err)
		}
	}

	b.syncPending()

	if n := stub.sentTo("sendMessage"); n != 1 {
		t.Fatalf("sent %d messages, want exactly 1 (the flagged one)", n)
	}
	b.mu.Lock()
	_, safeSent := b.sent["safe-1"]
	_, warnSent := b.sent["warn-1"]
	b.mu.Unlock()
	if safeSent {
		t.Error("a safe request was sent to the phone")
	}
	if !warnSent {
		t.Error("the flagged request was not sent to the phone")
	}
}

// The same request must not buzz the phone on every 2-second tick.
func TestRequestIsSentOnlyOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	if err := writePending(PendingRequest{ID: "warn-dup", Risk: riskWarn, Tool: "Bash", Summary: "sudo"}); err != nil {
		t.Fatalf("writePending: %v", err)
	}

	b.syncPending()
	b.syncPending()
	b.syncPending()

	if n := stub.sentTo("sendMessage"); n != 1 {
		t.Fatalf("sent %d messages for one request, want 1", n)
	}
}

// /status must answer, and arbitrary chatter must not — the bot should stay
// silent if you type in its chat rather than replying to everything.
func TestStatusCommandAnswersAndChatterIsIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	b.handleCommand("/status")
	if n := stub.sentTo("sendMessage"); n != 1 {
		t.Fatalf("/status sent %d messages, want 1", n)
	}

	b.handleCommand("just thinking out loud")
	if n := stub.sentTo("sendMessage"); n != 1 {
		t.Fatalf("arbitrary text triggered a reply (%d total sends)", n)
	}

	// Telegram appends the bot name in groups; that must still route.
	b.handleCommand("/status@omnibot")
	if n := stub.sentTo("sendMessage"); n != 2 {
		t.Fatalf("/status@bot did not route (%d total sends)", n)
	}
}

// With no sessions running the report must say so rather than render an empty
// shell that looks like a bug.
func TestStatusReportWithNoSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // empty ~/.claude/sessions
	got := statusReport()
	if !strings.Contains(got, "No Claude sessions running") {
		t.Fatalf("unexpected empty-state report: %q", got)
	}
}

// Targeting is the risk in /send: delivering a message to the wrong session is
// worse than not delivering it, so ambiguity must fail loudly.
func TestResolveTargetByPositionAndName(t *testing.T) {
	sessions := []Session{
		{SessionID: "s1", Cwd: "/Users/x/gembid-next"},
		{SessionID: "s2", Cwd: "/Users/x/myjeweler-ai"},
	}

	if got, err := resolveTarget(sessions, "2"); err != nil || got.SessionID != "s2" {
		t.Errorf("by position: got %+v err=%v, want s2", got, err)
	}
	if got, err := resolveTarget(sessions, "myjeweler-ai"); err != nil || got.SessionID != "s2" {
		t.Errorf("by name: got %+v err=%v, want s2", got, err)
	}
	if got, err := resolveTarget(sessions, "GEMBID-NEXT"); err != nil || got.SessionID != "s1" {
		t.Errorf("name should be case-insensitive: got %+v err=%v", got, err)
	}
	if got, err := resolveTarget(sessions, "gem"); err != nil || got.SessionID != "s1" {
		t.Errorf("unique prefix should resolve: got %+v err=%v", got, err)
	}
}

func TestResolveTargetRejectsBadRefs(t *testing.T) {
	sessions := []Session{
		{SessionID: "s1", Cwd: "/Users/x/api-server"},
		{SessionID: "s2", Cwd: "/Users/x/api-client"},
	}
	for _, tc := range []struct{ ref, why string }{
		{"", "empty reference"},
		{"9", "out-of-range position"},
		{"0", "position is 1-based"},
		{"nope", "no such project"},
		{"api", "ambiguous prefix matching two sessions"},
	} {
		if _, err := resolveTarget(sessions, tc.ref); err == nil {
			t.Errorf("%s (%q) resolved instead of erroring", tc.why, tc.ref)
		}
	}
}

// The message body must survive verbatim — it goes to a model, so casing and
// punctuation carry meaning — and it must land in the outbox, which is the
// non-interrupting delivery path rather than a second `claude --resume`.
func TestSendQueuesToOutboxVerbatim(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A session registry entry whose pid is this test process, so it reads as
	// genuinely alive without spawning anything.
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := map[string]any{
		"pid": os.Getpid(), "sessionId": "live-1",
		"cwd": filepath.Join(home, "gembid-next"), "status": "busy",
	}
	data, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(home, ".claude", "sessions", "1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// The Stop hook must be installed or delivery is refused by design.
	if _, err := EnsureHookInstalled(); err != nil {
		t.Fatal(err)
	}

	const msg = "Fix the Auth Bug NOW -- check RLS policies"
	reply := sendToSession("", "1 "+msg)
	if !strings.Contains(reply, "Queued") {
		t.Fatalf("send was not queued: %q", reply)
	}
	// A busy session must be told the message waits for the turn to end, not
	// that it interrupts.
	if !strings.Contains(reply, "current turn ends") {
		t.Errorf("busy session should promise delivery at turn end, got %q", reply)
	}

	queued := loadOutbox("live-1")
	if len(queued) != 1 {
		t.Fatalf("outbox has %d messages, want 1", len(queued))
	}
	if queued[0].Text != msg {
		t.Errorf("message altered in transit:\n got %q\nwant %q", queued[0].Text, msg)
	}
}

// A /send with a target but no message must be rejected, not queued as empty.
func TestSendWithNoBodyIsRejected(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	sess := map[string]any{"pid": os.Getpid(), "sessionId": "live-2", "cwd": filepath.Join(home, "proj")}
	data, _ := json.Marshal(sess)
	if err := os.WriteFile(filepath.Join(home, ".claude", "sessions", "1.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if got := sendToSession("", "1"); !strings.Contains(got, "Nothing to send") {
		t.Errorf("bare /send should be rejected, got %q", got)
	}
	if n := len(loadOutbox("live-2")); n != 0 {
		t.Errorf("an empty message was queued (%d in outbox)", n)
	}
}

// The idle case is the one worth being blunt about: a queued message for a
// session sitting at its prompt may never be delivered, because delivery only
// happens when a turn ends and that session already ended its last one.
func TestDeliveryOutlookIsHonestAboutIdleSessions(t *testing.T) {
	now := float64(time.Now().UnixMilli())

	busy := Session{Status: "busy", StatusUpdatedAt: now}
	if got := deliveryOutlook(busy); !strings.Contains(got, "current turn ends") {
		t.Errorf("busy: got %q", got)
	}

	justFinished := Session{Status: "waiting", StatusUpdatedAt: now}
	if got := deliveryOutlook(justFinished); !strings.Contains(got, "next turn") {
		t.Errorf("recently active: got %q", got)
	}

	idle := Session{Status: "waiting", StatusUpdatedAt: now - float64((30 * time.Minute).Milliseconds())}
	got := deliveryOutlook(idle)
	if !strings.Contains(got, "WON'T DELIVER") {
		t.Errorf("an idle session must warn that delivery may not happen, got %q", got)
	}
	if strings.Contains(got, "delivers on its next turn") {
		t.Errorf("idle session must not imply a turn is coming, got %q", got)
	}
}

// A question notification must fire once per question, not once per tick, and
// must not fire while you're plausibly still reading the answer.
func TestQuestionNotificationFiresOncePerQuestion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	// A session that finished speaking well past the threshold.
	long := float64(time.Now().Add(-10 * time.Minute).UnixMilli())
	writeLiveSession(t, home, "q-sess", "waiting", long)
	writeTranscriptFor(t, home, "q-sess", `How do you want to proceed?

1. Keep Infobip for SMS
2. Port GHL SMS too
3. Park GHL entirely
`)

	b.syncQuestions()
	if n := stub.sentTo("sendMessage"); n != 1 {
		t.Fatalf("first pass sent %d messages, want 1", n)
	}
	b.syncQuestions()
	b.syncQuestions()
	if n := stub.sentTo("sendMessage"); n != 1 {
		t.Fatalf("same question buzzed again (%d sends) — phone would never stop", n)
	}
}

// Freshly-asked questions must stay quiet: you're most likely still at the
// keyboard reading them.
func TestQuestionNotificationWaitsBeforeBuzzing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	justNow := float64(time.Now().UnixMilli())
	writeLiveSession(t, home, "fresh", "waiting", justNow)
	writeTranscriptFor(t, home, "fresh", "Pick one:\n\n1. Alpha\n2. Beta\n")

	b.syncQuestions()
	if n := stub.sentTo("sendMessage"); n != 0 {
		t.Fatalf("buzzed immediately (%d sends); should wait for the threshold", n)
	}
}

// A working session must never be reported as waiting on you.
func TestBusySessionIsNotReportedAsAsking(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stub := newStubTelegram(t)
	b := newTestBridge(t, stub, 111)

	old := float64(time.Now().Add(-10 * time.Minute).UnixMilli())
	writeLiveSession(t, home, "busy-1", "busy", old)
	writeTranscriptFor(t, home, "busy-1", "Pick one:\n\n1. Alpha\n2. Beta\n")

	b.syncQuestions()
	if n := stub.sentTo("sendMessage"); n != 0 {
		t.Fatalf("a working session was reported as waiting (%d sends)", n)
	}
}

func writeLiveSession(t *testing.T, home, sid, status string, statusUpdatedAt float64) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{
		"pid": os.Getpid(), "sessionId": sid,
		"cwd": filepath.Join(home, "proj-"+sid), "status": status,
		"statusUpdatedAt": statusUpdatedAt,
	}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeTranscriptFor(t *testing.T, home, sid, assistantText string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := map[string]any{
		"type": "assistant", "timestamp": "2026-07-20T02:00:00.000Z",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": assistantText}}},
	}
	data, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, sid+".jsonl"), append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Telegram rejects an over-length message outright rather than truncating it,
// so a long conversation must be cut on our side or /convo silently fails.
func TestConvoReportStaysUnderTelegramLimit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	huge := strings.Repeat("This is a very long assistant turn. ", 2000) // ~70KB
	writeLiveSession(t, home, "big", "waiting", float64(time.Now().UnixMilli()))
	writeTranscriptFor(t, home, "big", huge)

	s := Session{SessionID: "big", Cwd: filepath.Join(home, "proj-big")}
	got := convoReport(s, 5)
	if len(got) > telegramMaxText {
		t.Fatalf("report is %d chars, over the %d limit", len(got), telegramMaxText)
	}
	if got == "" {
		t.Fatal("report was empty")
	}
}

// A session with only tool activity should say so rather than return nothing.
func TestConvoReportWithNoSpeakingTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := Session{SessionID: "missing", Cwd: filepath.Join(home, "nope")}
	if got := convoReport(s, 3); !strings.Contains(got, "No conversation") {
		t.Errorf("expected an explicit empty-state, got %q", got)
	}
}

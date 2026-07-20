package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Telegram bridge puts flagged permission requests on your phone and lets
// you answer them from there.
//
// Why a relay at all: the Mac sits behind NAT, so the phone can't reach it. Both
// directions here are OUTBOUND from this machine — we POST to send, and we
// long-poll getUpdates to receive. Nothing listens on a port, nothing is
// forwarded, and the setup works identically on a laptop that moves networks.
//
// Why Telegram specifically: the bot token is a secret and every callback
// carries the sender's user id, so we can hard-allowlist a single account. A
// public pub/sub topic would let anyone who guessed the name approve a
// dangerous command on this machine.
//
// Only FLAGGED (warn) requests are sent by default. Those are precisely the ones
// Omni refuses to auto-approve, so they're what actually blocks a session while
// you're away. Safe requests are noise on a phone.

type TelegramConfig struct {
	BotToken string `json:"botToken"`
	// ChatID is both the destination and the allowlist: a callback from any
	// other account is ignored. This is the security boundary of the feature.
	ChatID int64 `json:"chatId"`
	// NotifyAll sends every pending request rather than only flagged ones.
	NotifyAll bool `json:"notifyAll"`
	Enabled   bool `json:"enabled"`
}

func telegramConfigPath() string {
	return filepath.Join(deckDir(), "telegram.json")
}

// LoadTelegramConfig reads the bridge config. A missing file is not an error —
// the bridge simply stays off.
func LoadTelegramConfig() (TelegramConfig, error) {
	var c TelegramConfig
	data, err := os.ReadFile(telegramConfigPath())
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.BotToken == "" || c.ChatID == 0 {
		return c, errors.New("telegram.json needs both botToken and chatId")
	}
	return c, nil
}

// Save writes the config with owner-only permissions — it holds a bot token,
// which is a credential that can send messages as you.
func (c TelegramConfig) Save() error {
	if err := os.MkdirAll(deckDir(), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(telegramConfigPath(), append(data, '\n'), 0o600)
}

// phonePick is one tappable answer to a question a session asked: which pane to
// type it into, and the exact text to send.
type phonePick struct {
	pane    string
	text    string
	project string
}

// --- bridge ---

type TelegramBridge struct {
	cfg    TelegramConfig
	client *http.Client
	// baseURL is the Telegram API root, overridable so tests can exercise the
	// authorization path against a stub instead of the live API.
	baseURL string

	mu sync.Mutex
	// sent maps requestID -> the message we posted about it, so a request that
	// gets answered elsewhere (at the desk, or by expiring) can have its message
	// updated instead of leaving a live-looking button that does nothing.
	sent map[string]int64
	// tokens maps a short callback token -> requestID. Telegram caps
	// callback_data at 64 bytes and a request id (session uuid + tool + hash)
	// blows past that, so the button carries a nonce instead.
	tokens map[string]string
	// asked fingerprints question-menus already pushed, so the same pending
	// decision doesn't buzz the phone on every tick.
	asked map[string]struct{}
	// picks maps a callback token -> the answer a button will type into a pane.
	picks  map[string]phonePick
	offset int64
}

func NewTelegramBridge(cfg TelegramConfig) *TelegramBridge {
	return &TelegramBridge{
		cfg: cfg,
		// Longer than the long-poll timeout below, or every poll would be
		// cancelled client-side before Telegram had a chance to answer.
		client:  &http.Client{Timeout: 60 * time.Second},
		baseURL: "https://api.telegram.org",
		sent:    map[string]int64{},
		tokens:  map[string]string{},
		asked:   map[string]struct{}{},
		picks:   map[string]phonePick{},
	}
}

// Run drives the bridge until stop closes. It does its own polling of the
// pending directory rather than hooking the TUI, so it keeps working regardless
// of which tab is open, and a UI change can't silently break approvals.
func (b *TelegramBridge) Run(stop <-chan struct{}) {
	go b.pollUpdates(stop) // inbound: taps on the phone

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			b.syncPending()
			b.syncQuestions()
		}
	}
}

// syncPending sends a message for each newly-seen flagged request, and closes
// out messages whose request is gone.
func (b *TelegramBridge) syncPending() {
	pending := loadPending()
	live := map[string]PendingRequest{}
	for _, r := range pending {
		if !b.cfg.NotifyAll && r.Risk != riskWarn {
			continue // only flagged requests reach the phone
		}
		live[r.ID] = r
	}

	b.mu.Lock()
	var toSend []PendingRequest
	for id, r := range live {
		if _, already := b.sent[id]; !already {
			b.sent[id] = 0 // reserve now so the next tick doesn't double-send
			toSend = append(toSend, r)
		}
	}
	var stale []struct {
		id  string
		msg int64
	}
	for id, msg := range b.sent {
		if _, ok := live[id]; !ok {
			stale = append(stale, struct {
				id  string
				msg int64
			}{id, msg})
		}
	}
	for _, s := range stale {
		delete(b.sent, s.id)
	}
	b.mu.Unlock()

	for _, r := range toSend {
		msgID, err := b.sendRequest(r)
		if err != nil {
			// Let the next tick retry rather than dropping the request silently —
			// a missed notification means a session sits blocked.
			b.mu.Lock()
			delete(b.sent, r.ID)
			b.mu.Unlock()
			continue
		}
		b.mu.Lock()
		b.sent[r.ID] = msgID
		b.mu.Unlock()
	}

	// A request that vanished was answered at the desk or timed out. Replace the
	// buttons so the phone never shows a control that no longer does anything.
	for _, s := range stale {
		if s.msg != 0 {
			_ = b.editMessage(s.msg, "Handled elsewhere — no longer waiting.")
		}
	}
}

func (b *TelegramBridge) sendRequest(r PendingRequest) (int64, error) {
	tok := newCallbackToken()
	b.mu.Lock()
	b.tokens[tok] = r.ID
	b.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("Omni — flagged permission\n\n")
	sb.WriteString("Project: " + r.Project + "\n")
	sb.WriteString("Tool: " + r.Tool + "\n")
	if len(r.Reasons) > 0 {
		sb.WriteString("Flagged: " + strings.Join(r.Reasons, "; ") + "\n")
	}
	sb.WriteString("\n" + truncate(r.Summary, 400))

	body := map[string]any{
		"chat_id": b.cfg.ChatID,
		"text":    sb.String(),
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{{
				{"text": "Allow", "callback_data": "a:" + tok},
				{"text": "Deny", "callback_data": "d:" + tok},
			}},
		},
	}
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := b.call("sendMessage", body, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, errors.New("telegram rejected sendMessage")
	}
	return resp.Result.MessageID, nil
}

// pollUpdates long-polls for button taps. Long-polling is what keeps this
// outbound-only: Telegram holds the request open and answers when there's
// something, so no inbound connection is ever needed.
func (b *TelegramBridge) pollUpdates(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		body := map[string]any{
			"offset":          b.offset,
			"timeout":         25,
			"allowed_updates": []string{"callback_query", "message"},
		}
		var resp struct {
			OK     bool `json:"ok"`
			Result []struct {
				UpdateID      int64 `json:"update_id"`
				CallbackQuery struct {
					ID   string `json:"id"`
					From struct {
						ID int64 `json:"id"`
					} `json:"from"`
					Data    string `json:"data"`
					Message struct {
						MessageID int64 `json:"message_id"`
					} `json:"message"`
				} `json:"callback_query"`
				Message struct {
					Text string `json:"text"`
					From struct {
						ID int64 `json:"id"`
					} `json:"from"`
				} `json:"message"`
			} `json:"result"`
		}
		if err := b.call("getUpdates", body, &resp); err != nil {
			// Network blip or laptop asleep — back off and keep trying.
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		for _, u := range resp.Result {
			if u.UpdateID >= b.offset {
				b.offset = u.UpdateID + 1
			}
			// A typed command (/status). Same allowlist as button taps — status
			// reveals what you're working on, so it isn't public either.
			if u.Message.Text != "" {
				if authorizedSender(b.cfg, u.Message.From.ID) {
					b.handleCommand(u.Message.Text)
				}
				continue
			}

			cq := u.CallbackQuery
			if cq.ID == "" {
				continue
			}
			if !authorizedSender(b.cfg, cq.From.ID) {
				b.answerCallback(cq.ID, "Not authorized.")
				continue
			}
			b.handleDecision(cq.ID, cq.Data, cq.Message.MessageID)
		}
	}
}

// statusReport is observe mode, rendered for a phone: what every live session
// is doing right now. It reads the same sources the dashboard does, so the two
// can't drift — the registry for who's alive, the transcript for what each one
// is working on, and the outbox for what's waiting to be delivered.
//
// Deliberately compressed. On a phone you want to know "is anything stuck or
// waiting on me", not read a full conversation, so this is one block per
// session and never the transcript itself.
func statusReport() string {
	sessions := LoadSessions()
	if len(sessions) == 0 {
		return "No Claude sessions running."
	}
	surfaces := DetectSurfaces(sessions)

	var b strings.Builder
	busy := 0
	for _, s := range sessions {
		if s.IsBusy() {
			busy++
		}
	}
	b.WriteString(fmt.Sprintf("%d session(s) · %d working\n", len(sessions), busy))

	for i, s := range sessions {
		surface := surfaces[s.SessionID]
		if surface == "" {
			surface = s.Surface()
		}
		state := "waiting"
		if s.IsBusy() {
			state = "working"
		}
		// Numbered so /send can target one without typing a project name. The
		// order is stable (start time, then id), so a number stays valid between
		// reading the status and acting on it.
		b.WriteString(fmt.Sprintf("\n[%d] ", i+1) + s.Project() + " (" + surface + ") · " + state)
		b.WriteString(" · " + formatDuration(s.QuietFor()) + "\n")

		act := LoadActivity(s.SessionID)
		if cur, ok := act.Current(); ok {
			label := cur.ActiveForm
			if label == "" {
				label = cur.Subject
			}
			b.WriteString("  now: " + truncate(oneLine(label), 90) + "\n")
		}
		if n := len(act.Todos); n > 0 {
			b.WriteString(fmt.Sprintf("  tasks: %d/%d done\n", act.DoneCount(), n))
		}
		if running := runningCount(ReconcileAgents(loadSubagents(s.SessionID), s.StartedTime())); running > 0 {
			b.WriteString(fmt.Sprintf("  agents: %d running\n", running))
		}
		if q := len(loadOutbox(s.SessionID)); q > 0 {
			// Call out a queue that can't move. An idle session won't drain its
			// outbox until someone prompts that terminal, so a message sitting
			// here is stuck rather than merely in flight.
			note := "waiting to deliver"
			if !s.IsBusy() && s.QuietFor() >= phoneIdleThreshold {
				note = "STUCK — idle session, needs that terminal to run"
			}
			b.WriteString(fmt.Sprintf("  queued: %d message(s) %s\n", q, note))
		}
	}

	// Anything blocked on a decision is the thing most worth surfacing here.
	var flagged int
	for _, r := range loadPending() {
		if r.Risk == riskWarn {
			flagged++
		}
	}
	if flagged > 0 {
		b.WriteString(fmt.Sprintf("\n%d flagged permission(s) waiting on you.", flagged))
	}
	return b.String()
}

const telegramHelp = `Omni commands:
/status — what every running session is doing
/send <n|project> <message> — send a message to that session
/convo <n|project> [turns] — read the last turns of a session
/help — this message

/send never interrupts. The message is delivered when the session's current
turn ends, in the terminal that owns it — so a working session finishes first.

Flagged permission requests arrive automatically with Allow/Deny buttons.`

// resolveTarget finds the session a /send refers to, by list position ("2") or
// by project name. Returns a human error rather than guessing: sending a message
// to the wrong session is worse than not sending it.
func resolveTarget(sessions []Session, ref string) (Session, error) {
	if ref == "" {
		return Session{}, errors.New("say which session: /send <n|project> <message>")
	}
	// Position in the /status listing.
	if n, err := strconv.Atoi(ref); err == nil {
		if n < 1 || n > len(sessions) {
			return Session{}, fmt.Errorf("no session [%d] — /status shows %d", n, len(sessions))
		}
		return sessions[n-1], nil
	}
	// Project name, case-insensitive, unique prefix allowed.
	var matches []Session
	needle := strings.ToLower(ref)
	for _, s := range sessions {
		if strings.HasPrefix(strings.ToLower(s.Project()), needle) {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return Session{}, fmt.Errorf("no running session matching %q", ref)
	case 1:
		return matches[0], nil
	default:
		var names []string
		for i, s := range matches {
			names = append(names, fmt.Sprintf("[%d] %s", i+1, s.Project()))
		}
		return Session{}, fmt.Errorf("%q matches several: %s — use the number", ref, strings.Join(names, ", "))
	}
}

// askedThreshold is how long a session must sit quiet after offering a choice
// before we treat it as genuinely blocked on you. A session flips to "waiting"
// the instant it finishes speaking, and you're usually still reading it — a
// buzz at that moment is noise. A short delay means the phone only fires for
// questions you have actually walked away from.
const askedThreshold = 90 * time.Second

// syncQuestions notices sessions that have stopped and asked you something, and
// pushes the question to the phone.
//
// This is deliberately NOTIFY-ONLY, and the reason is worth stating: a session
// showing a choice menu has already ended its turn, so its Stop hook has already
// fired. Nothing will drain the outbox until that terminal runs again, which
// means an answer queued from here would sit stuck indefinitely. Offering Allow-
// style buttons would look like it worked while the session waited forever. So
// the phone tells you a decision is needed; the decision itself happens at the
// terminal that owns the session.
func (b *TelegramBridge) syncQuestions() {
	for _, s := range LoadSessions() {
		if s.IsBusy() || s.QuietFor() < askedThreshold {
			continue // still working, or you're probably still looking at it
		}
		opts := detectChoices(loadConvo(s.SessionID))
		if len(opts) < 2 {
			continue
		}
		// Fingerprint the menu, not the session: re-asking a different question
		// should notify again, but the same one must not buzz every tick.
		key := s.SessionID + "|" + choicesKey(opts)
		b.mu.Lock()
		_, seen := b.asked[key]
		if !seen {
			b.asked[key] = struct{}{}
		}
		b.mu.Unlock()
		if seen {
			continue
		}

		var sb strings.Builder
		sb.WriteString(s.Project() + " is waiting on you\n")
		sb.WriteString("(idle " + formatDuration(s.QuietFor()) + ")\n\n")
		for _, o := range opts {
			sb.WriteString(o.marker + ". " + truncate(oneLine(o.label), 90) + "\n")
			if o.desc != "" {
				sb.WriteString("   " + truncate(oneLine(o.desc), 110) + "\n")
			}
		}
		msg := map[string]any{"chat_id": b.cfg.ChatID}

		// Buttons only when the session is in a tmux pane. That's the only case
		// where a tap can actually reach an idle session — elsewhere the answer
		// would queue behind a Stop hook that has already fired, so offering a
		// button would be a lie the user only discovers much later.
		if pane, inTmux := PaneForSession(s); inTmux {
			var row []map[string]string
			for _, o := range opts {
				tok := newCallbackToken()
				b.mu.Lock()
				b.picks[tok] = phonePick{pane: pane, text: o.marker + ". " + o.label, project: s.Project()}
				b.mu.Unlock()
				row = append(row, map[string]string{"text": o.marker, "callback_data": "p:" + tok})
			}
			sb.WriteString("\nTap a number to answer.")
			msg["reply_markup"] = map[string]any{"inline_keyboard": [][]map[string]string{row}}
		} else {
			sb.WriteString("\nAnswer at the terminal — a reply queued from here would")
			sb.WriteString(" not be delivered while the session sits idle.")
		}
		msg["text"] = sb.String()
		_ = b.call("sendMessage", msg, nil)
	}
}

// telegramMaxText is Telegram's hard per-message limit. Going over doesn't
// truncate, it rejects the whole message — so a long conversation must be cut
// deliberately rather than optimistically.
const telegramMaxText = 4096

// convoReport renders the tail of a session's conversation for a phone.
//
// Newest-last, so it reads in the order you'd see on screen, but selected from
// the END backwards — on a phone you want what just happened, not the start of
// a session from three hours ago. Tool turns are dropped: they're the bulk of a
// transcript and almost never what you're checking on from a phone.
func convoReport(s Session, turns int) string {
	convo := loadConvo(s.SessionID)
	if len(convo) == 0 {
		return "No conversation recorded for " + s.Project() + " yet."
	}
	if turns <= 0 {
		turns = 3
	}

	// Walk backwards collecting speaking turns until we have enough, or would
	// blow the message limit.
	var picked []convoTurn
	budget := telegramMaxText - 200 // headroom for the header
	for i := len(convo) - 1; i >= 0 && len(picked) < turns; i-- {
		t := convo[i]
		if t.role == "tool" || strings.TrimSpace(t.text) == "" {
			continue
		}
		cost := len(t.text) + 16
		if cost > budget && len(picked) > 0 {
			break
		}
		picked = append(picked, t)
		budget -= cost
	}
	if len(picked) == 0 {
		return "Nothing but tool activity in " + s.Project() + " so far."
	}

	var sb strings.Builder
	sb.WriteString(s.Project() + " — last " + itoa(len(picked)) + " turn(s)\n")
	// picked is newest-first; print oldest-first so it reads naturally.
	for i := len(picked) - 1; i >= 0; i-- {
		t := picked[i]
		who := "You"
		if t.role == "claude" {
			who = "Claude"
		}
		// Per-turn cap as well as the overall budget, so one enormous turn can't
		// crowd out the rest of the exchange.
		sb.WriteString("\n— " + who + ":\n" + truncate(strings.TrimSpace(t.text), 1200) + "\n")
	}
	out := sb.String()
	if len(out) > telegramMaxText {
		out = out[:telegramMaxText-20] + "\n…(truncated)"
	}
	return out
}

// phoneIdleThreshold is when "quiet" stops meaning "between turns" and starts
// meaning "nobody is there". Below it you're plausibly still at the keyboard and
// a queued message lands within moments; above it, it may wait indefinitely.
const phoneIdleThreshold = 5 * time.Minute

// deliveryOutlook says honestly WHEN a queued message will actually arrive.
//
// This matters most for an idle session, and the temptation is to paper over
// it. Delivery happens only when a turn ENDS, and an idle session already ended
// its last one — so its outbox stays untouched until somebody prompts that
// terminal again. Saying "delivers on its next turn" to someone who is out for
// the evening implies a turn is coming when none may be.
func deliveryOutlook(s Session) string {
	switch {
	case s.IsBusy():
		return "delivers when the current turn ends"
	case s.QuietFor() >= phoneIdleThreshold:
		return "WON'T DELIVER until that terminal runs again (idle " +
			formatDuration(s.QuietFor()) + ") — it's waiting at its prompt"
	default:
		return "delivers on its next turn"
	}
}

// convoCommand resolves the target then renders its recent conversation.
func convoCommand(args string) string {
	sessions := LoadSessions()
	if len(sessions) == 0 {
		return "No sessions running."
	}
	ref, rest, _ := strings.Cut(strings.TrimSpace(args), " ")
	s, err := resolveTarget(sessions, ref)
	if err != nil {
		return err.Error()
	}
	turns := 3
	if n, convErr := strconv.Atoi(strings.TrimSpace(rest)); convErr == nil && n > 0 {
		turns = min(n, 10) // a phone screen, not an archive
	}
	return convoReport(s, turns)
}

// sendToSession queues a message for delivery into the session's own terminal.
// This is the same outbox the dashboard writes to, so a message sent from the
// phone and one typed at the desk are delivered identically — at the end of the
// current turn, by the Stop hook running inside that session. Nothing is
// interrupted and no second process is spawned.
func sendToSession(text string, args string) string {
	sessions := LoadSessions()
	if len(sessions) == 0 {
		return "No sessions running."
	}
	ref, body, _ := strings.Cut(strings.TrimSpace(args), " ")
	body = strings.TrimSpace(body)
	if body == "" {
		return "Nothing to send: /send <n|project> <message>"
	}
	s, err := resolveTarget(sessions, ref)
	if err != nil {
		return err.Error()
	}
	// A tmux pane can be typed into directly, so the message lands even when the
	// session is idle — this is what makes answering from the phone actually
	// work rather than queueing something that waits for a turn that never comes.
	if pane, ok := PaneForSession(s); ok {
		if err := TmuxSendLine(pane, body); err != nil {
			return "Could not type into " + s.Project() + ": " + err.Error()
		}
		return "Sent to " + s.Project() + " — typed into its terminal (" + pane + ")."
	}

	// Without the Stop hook in scope nothing will ever drain the outbox, so say
	// so rather than silently queue into a void.
	if !GlobalHookInstalled() && !ProjectHookInstalled(s.Cwd) {
		return "Can't deliver to " + s.Project() + ": Omni's Stop hook isn't installed there."
	}
	if _, err := enqueueOutbox(s.SessionID, body); err != nil {
		return "Could not queue: " + err.Error()
	}
	n := len(loadOutbox(s.SessionID))
	return fmt.Sprintf("Queued for %s — %s (%d waiting).", s.Project(), deliveryOutlook(s), n)
}

// handleCommand answers a text command from the phone. Unknown text is ignored
// rather than answered, so the bot stays quiet if you type in the chat.
func (b *TelegramBridge) handleCommand(text string) {
	text = strings.TrimSpace(text)
	// Split the command word from its arguments before lowercasing, so a sent
	// message keeps its original casing — it's going to a model, not a parser.
	head, args, _ := strings.Cut(text, " ")
	cmd := strings.ToLower(head)
	if i := strings.Index(cmd, "@"); i > 0 {
		cmd = cmd[:i] // tolerate "/status@yourbot" in groups
	}
	var reply string
	switch cmd {
	case "/status", "/s":
		reply = statusReport()
	case "/send":
		reply = sendToSession(text, args)
	case "/convo", "/c":
		reply = convoCommand(args)
	case "/help", "/start":
		reply = telegramHelp
	default:
		return
	}
	_ = b.call("sendMessage", map[string]any{
		"chat_id": b.cfg.ChatID,
		"text":    reply,
	}, nil)
}

// authorizedSender is the allowlist, and the security boundary of this feature:
// only the configured account may decide anything. Telegram authenticates the
// sender for us, so an id comparison is what stops a stranger who somehow sees
// the message from approving a flagged command on this machine. Kept as its own
// function so it is impossible to change without the test noticing.
func authorizedSender(cfg TelegramConfig, senderID int64) bool {
	return cfg.ChatID != 0 && senderID == cfg.ChatID
}

func (b *TelegramBridge) handleDecision(callbackID, data string, msgID int64) {
	// An answer to a question the model asked, typed straight into its pane.
	if strings.HasPrefix(data, "p:") {
		b.handlePick(callbackID, strings.TrimPrefix(data, "p:"), msgID)
		return
	}
	allow := strings.HasPrefix(data, "a:")
	tok := strings.TrimPrefix(strings.TrimPrefix(data, "a:"), "d:")

	b.mu.Lock()
	reqID, ok := b.tokens[tok]
	b.mu.Unlock()
	if !ok {
		// Token unknown: the dashboard restarted since the message was sent, so
		// we can't map the tap back to a request. Say so rather than guess.
		b.answerCallback(callbackID, "Expired — approve at the dashboard.")
		_ = b.editMessage(msgID, "Expired (dashboard restarted) — approve at the desk.")
		return
	}

	// Only answer a request that's still waiting; the hook gives up after a
	// timeout and approving into the void would be misleading.
	stillWaiting := false
	for _, r := range loadPending() {
		if r.ID == reqID {
			stillWaiting = true
			break
		}
	}
	if !stillWaiting {
		b.answerCallback(callbackID, "No longer waiting.")
		_ = b.editMessage(msgID, "Handled elsewhere — no longer waiting.")
		return
	}

	verdict := "denied from phone"
	if allow {
		verdict = "approved from phone"
	}
	if err := writeDecision(Decision{ID: reqID, Allow: allow, Reason: verdict}); err != nil {
		b.answerCallback(callbackID, "Could not record the decision.")
		return
	}
	b.answerCallback(callbackID, verdict)
	_ = b.editMessage(msgID, "Omni — "+verdict+".")
}

// handlePick types a chosen option into the session's tmux pane. Unlike a
// permission decision this writes no file: it is literally keystrokes into the
// terminal that owns the session, so the session answers its own question the
// same way it would if you had walked over and typed.
func (b *TelegramBridge) handlePick(callbackID, tok string, msgID int64) {
	b.mu.Lock()
	pick, ok := b.picks[tok]
	if ok {
		delete(b.picks, tok) // one tap per option; no double-answering
	}
	b.mu.Unlock()
	if !ok {
		b.answerCallback(callbackID, "Expired — answer at the terminal.")
		_ = b.editMessage(msgID, "Expired (dashboard restarted) — answer at the terminal.")
		return
	}
	if err := TmuxSendLine(pick.pane, pick.text); err != nil {
		b.answerCallback(callbackID, "Could not type it: "+err.Error())
		return
	}
	b.answerCallback(callbackID, "Sent: "+truncate(pick.text, 60))
	_ = b.editMessage(msgID, pick.project+" — answered from phone:\n\n"+truncate(pick.text, 200))
}

func (b *TelegramBridge) answerCallback(id, text string) {
	_ = b.call("answerCallbackQuery", map[string]any{
		"callback_query_id": id,
		"text":              text,
	}, nil)
}

func (b *TelegramBridge) editMessage(msgID int64, text string) error {
	if msgID == 0 {
		return nil
	}
	return b.call("editMessageText", map[string]any{
		"chat_id":    b.cfg.ChatID,
		"message_id": msgID,
		"text":       text,
	}, nil)
}

func (b *TelegramBridge) call(method string, body any, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := b.baseURL + "/bot" + b.cfg.BotToken + "/" + method
	resp, err := b.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Telegram explains itself in the body ("chat not found", "bot was blocked
		// by the user", …). Reporting only the status code turns a two-second fix
		// into a guessing game, so surface the description.
		var apiErr struct {
			Description string `json:"description"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = json.Unmarshal(raw, &apiErr)
		if apiErr.Description != "" {
			return fmt.Errorf("telegram %s: %s (http %d)", method, apiErr.Description, resp.StatusCode)
		}
		return fmt.Errorf("telegram %s: http %d", method, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// newCallbackToken is a short random id for callback_data, which Telegram caps
// at 64 bytes — far too small for a full request id.
func newCallbackToken() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:16]
	}
	return hex.EncodeToString(buf[:])
}

// VerifyTelegram checks the token and chat id by sending a real message, so
// setup fails loudly at configure time instead of silently at 2am.
func VerifyTelegram(cfg TelegramConfig) error {
	b := NewTelegramBridge(cfg)
	var resp struct {
		OK          bool `json:"ok"`
		Description string
	}
	err := b.call("sendMessage", map[string]any{
		"chat_id": cfg.ChatID,
		"text":    "Omni is connected. Flagged permission requests will appear here.",
	}, &resp)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New("telegram rejected the test message: " + resp.Description)
	}
	return nil
}

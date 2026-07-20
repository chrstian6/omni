package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runHook is the PreToolUse entrypoint (`omni hook`). Claude runs it
// before every tool call, handing it the call on stdin and reading its verdict
// on stdout. The whole design goal is: never hang a session. Every path that
// can't get a fast, confident answer defers to Claude's normal prompt.

type hookInput struct {
	SessionID string         `json:"session_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	Cwd       string         `json:"cwd"`

	// StopHookActive is set by Claude on the 2nd+ Stop invocation within one
	// turn — i.e. we blocked once already and it came back around.
	StopHookActive bool `json:"stop_hook_active"`
}

// hookOutput is the current PreToolUse contract: permissionDecision of
// allow | deny | ask, under hookSpecificOutput.
type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string `json:"hookEventName"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	} `json:"hookSpecificOutput"`
}

const (
	// How long to wait for a human at the dashboard. Long enough that you can
	// actually walk over and decide — but the loop also bails the instant the
	// dashboard heartbeat goes stale, so a closed dashboard never causes a stall.
	hookWait     = 5 * time.Minute
	hookPollStep = 200 * time.Millisecond
)

func runHook() int {
	// Any panic or malformed input must not block the tool. Default to "ask".
	defer func() {
		if r := recover(); r != nil {
			emit("ask", "omni hook error; deferring to normal prompt")
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		emit("ask", "")
		return 0
	}

	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil || in.ToolName == "" {
		// Can't understand the call — don't get in the way.
		emit("ask", "")
		return 0
	}

	level, reasons := classify(in.ToolName, in.ToolInput)

	// 1. Safety layer: catastrophic actions are denied by default and always
	//    recorded. They are still offered to a human, because a classifier that
	//    is occasionally wrong AND unappealable teaches people to route around
	//    it — the only escape used to be rewording the command. Overriding one
	//    takes a deliberate typed confirmation at the dashboard, never a
	//    keystroke, and is never covered by approve-all.
	if level == riskBlock {
		req := toRequest(in, level, reasons)
		appendBlockedLog(req)

		// No dashboard means no way to confirm deliberately, so a block stays a
		// block. Note this deliberately does NOT fall through to Claude's own
		// prompt the way safe/warn requests do: that prompt is a single keypress,
		// which is far too cheap for a catastrophic action.
		if !heartbeatFresh() {
			emit("deny", "Blocked by Omni safety layer: "+strings.Join(reasons, "; "))
			return 0
		}
		if err := writePending(req); err != nil {
			emit("deny", "Blocked by Omni safety layer: "+strings.Join(reasons, "; "))
			return 0
		}
		defer removePending(req.ID)
		defer removeDecision(req.ID)

		deadline := time.Now().Add(hookWait)
		for time.Now().Before(deadline) {
			if d, ok := readDecision(req.ID); ok {
				if d.Allow {
					appendOverrideLog(req, d.Reason)
					emit("allow", orDefault(d.Reason, "override confirmed in Omni"))
				} else {
					emit("deny", orDefault(d.Reason, "Blocked by Omni safety layer: "+strings.Join(reasons, "; ")))
				}
				return 0
			}
			if !heartbeatFresh() {
				break
			}
			time.Sleep(hookPollStep)
		}
		// Timed out, or the dashboard disappeared: stay blocked. Unlike a warn,
		// the safe default here is deny, not "ask".
		emit("deny", "Blocked by Omni safety layer: "+strings.Join(reasons, "; "))
		return 0
	}

	// 2. No dashboard running? Never wait. Fall through to the normal prompt.
	//    This is what makes a global install harmless when the deck is closed.
	if !heartbeatFresh() {
		emit("ask", "")
		return 0
	}

	// 3. Approve-all policy covers only non-flagged actions. A flagged (warn)
	//    call always needs an explicit, individual decision.
	policy := loadPolicy()
	if level == riskSafe && policy.autoApproves(in.SessionID) {
		emit("allow", "auto-approved by Omni policy")
		return 0
	}

	// 4. Route to the dashboard and wait for a verdict.
	req := toRequest(in, level, reasons)
	if err := writePending(req); err != nil {
		emit("ask", "")
		return 0
	}
	defer removePending(req.ID)
	defer removeDecision(req.ID)

	deadline := time.Now().Add(hookWait)
	for time.Now().Before(deadline) {
		if d, ok := readDecision(req.ID); ok {
			if d.Allow {
				emit("allow", orDefault(d.Reason, "approved in Omni"))
			} else {
				emit("deny", orDefault(d.Reason, "denied in Omni"))
			}
			return 0
		}
		// If the dashboard disappears mid-wait, stop waiting on a ghost.
		if !heartbeatFresh() {
			emit("ask", "")
			return 0
		}
		time.Sleep(hookPollStep)
	}

	// Timed out with no answer — defer to the normal prompt.
	emit("ask", "Omni: no response, showing the normal prompt")
	return 0
}

// stopOutput is the Stop hook contract. Returning decision "block" keeps the
// session going and feeds `reason` back to Claude instead of letting it stop.
type stopOutput struct {
	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// runStopHook is the `omni stop-hook` entrypoint, and the whole reason a prompt
// sent from the dashboard lands in the terminal that owns the session.
//
// It runs INSIDE the real session process — the one started in VS Code — every
// time that session finishes a turn. If the dashboard queued anything for this
// session, we hand it over here and block the stop, so the session picks the
// message up and answers it in its own window. No second process, no forked
// transcript, no divergence: the originating terminal stays the only writer.
//
// The trade-off is honest and worth stating: Stop only fires at the END of a
// turn, so a message queued for a session sitting idle at its prompt waits
// until that session runs again. We queue rather than fork — the dashboard
// shows it as pending so it's never a silent drop.
func runStopHook() int {
	// A Stop hook that panics or hangs would wedge a real session. Every failure
	// path here falls through to "no decision", which just lets it stop normally.
	defer func() {
		if r := recover(); r != nil {
			emitStop(stopOutput{})
		}
	}()

	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		emitStop(stopOutput{})
		return 0
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil || in.SessionID == "" {
		emitStop(stopOutput{})
		return 0
	}

	queued := loadOutbox(in.SessionID)
	if len(queued) == 0 {
		emitStop(stopOutput{}) // nothing waiting — let the session stop
		return 0
	}

	// Deliver everything queued as one turn, in arrival order, and delete each
	// as it goes out. Removing them here is the loop guard: blocking makes Claude
	// respond and fire Stop again, but the queue is empty by then so the next
	// call returns no decision. StopHookActive is only a belt-and-braces check —
	// if we somehow re-enter with messages still present, deliver nothing.
	if in.StopHookActive && len(queued) > 0 {
		// Re-entered with a non-empty queue: something queued mid-turn. That's a
		// legitimate new message, but delivering it now risks a tight loop if the
		// dashboard is enqueuing faster than turns complete. Leave it for the next
		// natural stop instead.
		emitStop(stopOutput{})
		return 0
	}

	var parts []string
	for _, m := range queued {
		parts = append(parts, m.Text)
		removeOutbox(in.SessionID, m.ID)
	}

	emitStop(stopOutput{
		Decision: "block",
		Reason:   framePrompt(parts),
	})
	return 0
}

// framePrompt labels the delivered text as the user speaking. Claude Code
// renders a blocked stop as "Stop hook feedback: <reason>", which on its own
// reads like tooling output — a bare answer such as "1" arrives with no hint
// that a person sent it or what it responds to. The prefix restores that.
func framePrompt(parts []string) string {
	body := strings.Join(parts, "\n\n")
	lead := "The user sent this from the Omni dashboard. Treat it as their next message and respond to it directly:"
	if len(parts) > 1 {
		lead = "The user sent these from the Omni dashboard, in order. Treat them as their next messages and respond directly:"
	}
	return lead + "\n\n" + body
}

func emitStop(out stopOutput) {
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func toRequest(in hookInput, level string, reasons []string) PendingRequest {
	// A stable-ish id: session + tool + a short hash of the input, so retries of
	// the identical call collapse onto one pending entry.
	id := in.SessionID + "-" + sanitize(in.ToolName) + "-" + shortHash(in.ToolInput)
	return PendingRequest{
		ID:        id,
		SessionID: in.SessionID,
		PID:       pidForSession(in.SessionID),
		Project:   filepath.Base(in.Cwd),
		Cwd:       in.Cwd,
		Tool:      in.ToolName,
		Summary:   summarize(in.ToolName, in.ToolInput),
		Risk:      level,
		Reasons:   reasons,
		CreatedAt: time.Now().UnixMilli(),
	}
}

func emit(decision, reason string) {
	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = decision
	out.HookSpecificOutput.PermissionDecisionReason = reason
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, s)
}

// shortHash is a small non-cryptographic digest (FNV-1a) of the tool input, so
// two different commands don't share a pending id.
func shortHash(input map[string]any) string {
	data, _ := json.Marshal(input)
	var h uint64 = 1469598103934665603
	for _, b := range data {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 36)
}

// pidForSession looks up the live pid from the registry so the dashboard can
// show the request under the right row even before its own refresh catches up.
func pidForSession(sessionID string) int32 {
	for _, s := range LoadSessions() {
		if s.SessionID == sessionID {
			return s.PID
		}
	}
	return 0
}

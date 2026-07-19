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

	// 1. Safety layer: catastrophic actions are denied outright, no human in
	//    the loop, and recorded for audit.
	if level == riskBlock {
		req := toRequest(in, level, reasons)
		appendBlockedLog(req)
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

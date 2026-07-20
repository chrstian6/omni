package main

import (
	"bufio"
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A Subagent is one Task/background agent a session has launched. We reconstruct
// them from the transcript: an assistant `tool_use` with name "Task" starts one,
// and the matching `tool_result` (keyed by tool id) finishes it. Anything with a
// start but no result is still running — that's the "waiting for N agents" state
// Claude Code shows in the session itself.
type Subagent struct {
	ID      string    // the tool_use id
	Name    string    // description ("Audit data/schema parity") or the type
	Type    string    // subagent_type, e.g. "auditor"
	Started time.Time // when the Task call was logged
	Ended   time.Time // zero while still running
	Running bool
	Tokens  int // output tokens produced by this agent (see attribution note)
	// Orphan means the agent can never finish: it was launched by an EARLIER
	// process for this session, so whatever was running died with that process
	// and its completion notification will never arrive. Without this it shows
	// as "running" with a clock that ticks forever — which is exactly what a
	// resumed session looks like after a crash or a kill.
	Orphan bool
}

// ReconcileAgents marks agents that predate the session's current process as
// orphaned rather than running. Compare against when the PROCESS started, not
// when the conversation did: a resumed session keeps its id and transcript, so
// only the process boundary reveals that the agent's runtime is gone.
func ReconcileAgents(agents []Subagent, processStart time.Time) []Subagent {
	if processStart.IsZero() {
		return agents
	}
	out := make([]Subagent, len(agents))
	copy(out, agents)
	for i := range out {
		if out[i].Running && !out[i].Started.IsZero() && out[i].Started.Before(processStart) {
			out[i].Running = false
			out[i].Orphan = true
			// Freeze the clock at the process boundary: it stopped being real then.
			if out[i].Ended.IsZero() {
				out[i].Ended = processStart
			}
		}
	}
	return out
}

// Elapsed is how long the agent ran (finished) or has been running (live).
func (a Subagent) Elapsed() time.Duration {
	if a.Started.IsZero() {
		return 0
	}
	end := a.Ended
	if a.Running || end.IsZero() {
		end = time.Now()
	}
	if end.Before(a.Started) {
		return 0
	}
	return end.Sub(a.Started)
}

// loadSubagents walks the transcript once and reconstructs every subagent this
// session launched, tracking whether each is still running.
//
// There are two launch shapes to reconcile, keyed by tool_use id:
//   - Sync (foreground): an Agent/Task tool_use, closed by a tool_result whose
//     content is the agent's actual report — that result is the finish.
//   - Async (background): an Agent tool_use whose tool_result is only a launch
//     ACK ("Async agent launched…"). The agent is NOT finished then — it's now
//     running in the background, and its real completion arrives later as a
//     <task-notification> carrying the same <tool-use-id>, plus the true
//     duration and token totals. Marking it finished at ack time was the bug
//     that made a just-started agent show as done.
//
// Notifications are matched back to their launch by tool-use-id, so an async
// agent is one row that goes running → finished — never a duplicate.
func loadSubagents(sessionID string) []Subagent {
	path := findTranscript(sessionID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var order []string             // launch order, by tool_use id
	byID := map[string]*Subagent{} // tool_use id -> agent

	// Latest task-notification per tool-use-id (a task may notify more than once).
	type notif struct {
		ts     time.Time
		status string
		dur    time.Duration
		tokens int
		name   string
	}
	notifs := map[string]notif{}
	var notifOrder []string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
			// A task-notification does NOT arrive as a normal message. It shows up
			// in its own record shapes, which is why an async agent used to hang
			// on "running" forever: the completion was in the transcript, just not
			// anywhere this scanner looked.
			//   queue-operation → a bare string in "content"
			//   attachment      → attachment.prompt
			Content    json.RawMessage `json:"content"`
			Attachment struct {
				Prompt string `json:"prompt"`
			} `json:"attachment"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		ts := parseTS(rec.Timestamp)

		// An async agent reporting in — record it against its tool-use-id
		// (falling back to task-id for older shapes). Checked before the record
		// type filter below, since these records are neither assistant nor user.
		if blocks := notificationBlocks(rec.Attachment.Prompt, rawString(rec.Content), messageText(rec.Message)); len(blocks) > 0 {
			for _, text := range blocks {
				key := firstGroup(reNAToolUse, text)
				if key == "" {
					key = firstGroup(reNATaskID, text)
				}
				if key == "" {
					continue
				}
				n := notif{ts: ts, status: firstGroup(reNAStatus, text)}
				if d := firstGroup(reNADur, text); d != "" {
					if ms, e := strconv.Atoi(d); e == nil {
						n.dur = time.Duration(ms) * time.Millisecond
					}
				}
				if t := firstGroup(reNATokens, text); t != "" {
					n.tokens, _ = strconv.Atoi(t)
				}
				n.name = firstGroup(reNAName, firstGroup(reNASummary, text))
				// The same notification is written more than once (enqueue, remove,
				// attachment). Keying by tool-use-id collapses them to one row, with
				// the latest winning.
				if _, seen := notifs[key]; !seen {
					notifOrder = append(notifOrder, key)
				}
				notifs[key] = n
			}
			continue
		}

		if rec.Type != "assistant" && rec.Type != "user" {
			continue
		}

		var msg struct {
			Content []map[string]any `json:"content"`
		}
		_ = json.Unmarshal(rec.Message, &msg) // content may be a bare string — ignore
		for _, b := range msg.Content {
			switch b["type"] {
			case "tool_use":
				// Subagents launch via either the classic "Task" tool or the newer
				// "Agent" tool. A launch with no finishing result is still running —
				// the live fan-out we want to surface.
				name := str(b["name"])
				if name != "Task" && name != "Agent" {
					continue
				}
				id := str(b["id"])
				if id == "" {
					continue
				}
				input, _ := b["input"].(map[string]any)
				if _, seen := byID[id]; !seen {
					order = append(order, id)
				}
				byID[id] = &Subagent{
					ID:      id,
					Name:    subagentName(input),
					Type:    str(input["subagent_type"]),
					Started: ts,
					Running: true,
				}
			case "tool_result":
				a, ok := byID[str(b["tool_use_id"])]
				if !ok {
					continue
				}
				// An async-launch ACK means the agent is now working in the
				// background — leave it running; its completion comes via a
				// task-notification. A normal (sync) result is the finish.
				if isAsyncLaunchAck(toolResultText(b)) {
					continue
				}
				a.Ended = ts
				a.Running = false
			}
		}
	}

	// Reconcile notifications with their launches (matched by tool-use-id). When
	// we never saw the launch (a truncated transcript), synthesize the agent from
	// the notification alone so it still shows.
	for _, key := range notifOrder {
		n := notifs[key]
		terminal := n.status == "" || n.status == "completed" || n.status == "failed"
		if a, ok := byID[key]; ok {
			if n.tokens > 0 {
				a.Tokens = n.tokens
			}
			if n.name != "" {
				a.Name = n.name
			}
			if terminal {
				a.Running = false
				if n.ts.After(a.Started) {
					a.Ended = n.ts
				}
			} else {
				a.Running = true // notified but resumable — still in flight
			}
			continue
		}
		if n.name == "" {
			continue // not a genuine agent-completion notification
		}
		byID[key] = &Subagent{ID: key, Name: n.name, Started: n.ts.Add(-n.dur), Ended: n.ts, Running: !terminal, Tokens: n.tokens}
		order = append(order, key)
	}

	out := make([]Subagent, 0, len(order))
	for _, id := range order {
		if a := byID[id]; a != nil {
			out = append(out, *a)
		}
	}
	// Running agents first, then most-recently-started first.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		return out[i].Started.After(out[j].Started)
	})
	return out
}

var (
	reNATaskID  = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	reNAToolUse = regexp.MustCompile(`<tool-use-id>([^<]+)</tool-use-id>`)
	reNAStatus  = regexp.MustCompile(`<status>([^<]+)</status>`)
	reNASummary = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	reNATokens  = regexp.MustCompile(`<subagent_tokens>(\d+)</subagent_tokens>`)
	reNADur     = regexp.MustCompile(`<duration_ms>(\d+)</duration_ms>`)
	reNAName    = regexp.MustCompile(`Agent\s+"([^"]+)"`)
)

// reTaskNotif isolates a complete notification element. Matching the WHOLE
// element (not just the opening tag) is what keeps a transcript that merely
// *mentions* task-notifications — a file read, a diff, this very source file —
// from being parsed as one and inventing an agent that never ran.
var reTaskNotif = regexp.MustCompile(`(?s)<task-notification>(.*?)</task-notification>`)

// notificationBlocks returns every task-notification found across the candidate
// fields of one transcript record. The notification travels in different places
// depending on the record shape, so all of them are checked.
func notificationBlocks(candidates ...string) []string {
	var out []string
	for _, c := range candidates {
		if c == "" || !strings.Contains(c, "<task-notification>") {
			continue
		}
		for _, m := range reTaskNotif.FindAllStringSubmatch(c, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// rawString decodes a JSON field that may be a bare string, returning "" for any
// other shape (objects, arrays, null).
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

// isAsyncLaunchAck recognizes the internal tool_result the harness returns when
// an Agent is launched in the background — it means "started", not "finished".
func isAsyncLaunchAck(s string) bool {
	return strings.Contains(s, "Async agent launched") ||
		strings.Contains(s, "working in the background")
}

// toolResultText pulls the text out of a tool_result block whose content may be
// a bare string or an array of {type:text} blocks.
func toolResultText(b map[string]any) string {
	switch c := b["content"].(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, blk := range c {
			if bb, ok := blk.(map[string]any); ok && bb["type"] == "text" {
				sb.WriteString(str(bb["text"]))
				sb.WriteString(" ")
			}
		}
		return sb.String()
	}
	return ""
}

// messageText pulls the plain text out of a message whose content may be a bare
// string or an array of blocks.
func messageText(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(m.Content, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b["type"] == "text" {
				sb.WriteString(str(b["text"]))
				sb.WriteString(" ")
			}
		}
		return sb.String()
	}
	return ""
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// subagentName prefers the human description, falling back to the agent type.
func subagentName(input map[string]any) string {
	if d := oneLine(str(input["description"])); d != "" {
		return d
	}
	if t := str(input["subagent_type"]); t != "" {
		return t
	}
	return "agent"
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Go parses fractional seconds against this layout automatically.
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// runningCount is how many of the agents are still in flight.
func runningCount(agents []Subagent) int {
	n := 0
	for _, a := range agents {
		if a.Running {
			n++
		}
	}
	return n
}

// totalAgentTokens sums the output tokens across all agents.
func totalAgentTokens(agents []Subagent) int {
	n := 0
	for _, a := range agents {
		n += a.Tokens
	}
	return n
}

// formatTokens renders a token count like Claude Code: "58.3k", "1.2M", "812".
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64) + "M"
	case n >= 1_000:
		return strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64) + "k"
	default:
		return itoa(n)
	}
}

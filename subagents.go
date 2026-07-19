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

// txRec is the slice of a transcript record we need for token attribution.
type txRec struct {
	uuid   string
	parent string
	side   bool // isSidechain
	tokens int  // message.usage.output_tokens
}

// loadSubagents walks the transcript once, pairing Task launches with their
// results and attributing sidechain output-tokens back to each Task.
//
// Token attribution: a subagent's work is logged as `isSidechain` records whose
// parent chain leads back to the assistant message that spawned the Task. We sum
// each sidechain record's output_tokens onto that spawning message, then split it
// across the Tasks launched in that message. For a lone Task the number is exact;
// for several launched together (the parallel case) it's an even split — the per-
// agent figure is approximate but the totals are right.
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

	// order preserves launch order; byID lets the result records find their agent.
	var order []string
	byID := map[string]*Subagent{}

	recs := map[string]txRec{}       // uuid -> record, for parent walking
	var sideRecs []txRec             // sidechain records that produced tokens
	taskSpawn := map[string]string{} // taskID -> spawning assistant uuid
	tasksPerUUID := map[string]int{} // assistant uuid -> number of Tasks in it

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Type        string          `json:"type"`
			UUID        string          `json:"uuid"`
			ParentUUID  string          `json:"parentUuid"`
			IsSidechain bool            `json:"isSidechain"`
			Timestamp   string          `json:"timestamp"`
			Message     json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		var msg struct {
			Content []map[string]any `json:"content"`
			Usage   struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(rec.Message, &msg) // content may be a bare string — ignore

		rc := txRec{uuid: rec.UUID, parent: rec.ParentUUID, side: rec.IsSidechain, tokens: msg.Usage.OutputTokens}
		if rec.UUID != "" {
			recs[rec.UUID] = rc
		}
		if rec.IsSidechain && msg.Usage.OutputTokens > 0 {
			sideRecs = append(sideRecs, rc)
		}

		if rec.Type != "assistant" && rec.Type != "user" {
			continue
		}
		ts := parseTS(rec.Timestamp)
		for _, b := range msg.Content {
			switch b["type"] {
			case "tool_use":
				// Subagents are launched via either the classic "Task" tool or the
				// newer "Agent" tool — both start a subagent and are closed by their
				// matching tool_result. A launch with no result yet is still running:
				// that's the live fan-out we want to surface.
				name := str(b["name"])
				if name != "Task" && name != "Agent" {
					continue
				}
				input, _ := b["input"].(map[string]any)
				// Background Agent launches return an immediate ack and finish
				// asynchronously, reported later via <task-notification> (handled by
				// noteAgents, which has their real duration and token totals). Skip
				// them here so they aren't marked "finished" at ack time or counted
				// twice.
				if name == "Agent" && truthy(input["run_in_background"]) {
					continue
				}
				id := str(b["id"])
				if id == "" {
					continue
				}
				a := &Subagent{
					ID:      id,
					Name:    subagentName(input),
					Type:    str(input["subagent_type"]),
					Started: ts,
					Running: true,
				}
				if _, seen := byID[id]; !seen {
					order = append(order, id)
					tasksPerUUID[rec.UUID]++
					taskSpawn[id] = rec.UUID
				}
				byID[id] = a
			case "tool_result":
				id := str(b["tool_use_id"])
				if a, ok := byID[id]; ok {
					a.Ended = ts
					a.Running = false
				}
			}
		}
	}

	// Fold each sidechain record's tokens onto the message that spawned its Task.
	tokensBySpawn := map[string]int{}
	for _, sr := range sideRecs {
		tokensBySpawn[spawnUUID(sr, recs)] += sr.tokens
	}
	for id, a := range byID {
		if u := taskSpawn[id]; u != "" && tasksPerUUID[u] > 0 {
			a.Tokens = tokensBySpawn[u] / tasksPerUUID[u]
		}
	}

	out := make([]Subagent, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	// Most sessions here don't use the classic Task tool_use — their background
	// agents show up only as <task-notification> messages. Fold those in too.
	out = append(out, noteAgents(path)...)

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
	reNASummary = regexp.MustCompile(`(?s)<summary>(.*?)</summary>`)
	reNATokens  = regexp.MustCompile(`<subagent_tokens>(\d+)</subagent_tokens>`)
	reNADur     = regexp.MustCompile(`<duration_ms>(\d+)</duration_ms>`)
	reNAName    = regexp.MustCompile(`Agent\s+"([^"]+)"`)
)

// noteAgents reconstructs background agents from <task-notification> messages —
// the way agents are recorded when they're launched outside the classic Task
// tool (content is a bare string carrying summary/usage XML). Each notification
// is a finished agent; the record timestamp is its finish time and duration_ms
// how long it ran, so Started = finish − duration. Deduped by task-id (a task can
// notify more than once — the latest wins).
func noteAgents(path string) []Subagent {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	byTask := map[string]*Subagent{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.Contains(line, "task-notification") {
			continue
		}
		var rec struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Type != "user" && rec.Type != "assistant" {
			continue
		}
		text := messageText(rec.Message)
		if !strings.Contains(text, "<task-notification>") {
			continue
		}
		id := firstGroup(reNATaskID, text)
		if id == "" {
			continue
		}
		// Only genuine agent-completion notifications name an agent as
		// Agent "X". Others (e.g. orphaned-agent warnings) are skipped.
		name := firstGroup(reNAName, firstGroup(reNASummary, text))
		if name == "" {
			continue
		}
		ts := parseTS(rec.Timestamp)
		var dur time.Duration
		if d := firstGroup(reNADur, text); d != "" {
			if ms, e := strconv.Atoi(d); e == nil {
				dur = time.Duration(ms) * time.Millisecond
			}
		}
		tokens := 0
		if t := firstGroup(reNATokens, text); t != "" {
			tokens, _ = strconv.Atoi(t)
		}
		a := &Subagent{ID: id, Name: name, Started: ts.Add(-dur), Ended: ts, Running: false, Tokens: tokens}
		if _, seen := byTask[id]; !seen {
			order = append(order, id)
		}
		byTask[id] = a // latest notification wins
	}

	out := make([]Subagent, 0, len(order))
	for _, id := range order {
		out = append(out, *byTask[id])
	}
	return out
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

// spawnUUID walks a sidechain record up its parent chain to the first
// non-sidechain ancestor — the assistant message that launched the Task.
func spawnUUID(r txRec, recs map[string]txRec) string {
	cur := r
	seen := map[string]bool{}
	for cur.parent != "" && !seen[cur.parent] {
		seen[cur.parent] = true
		p, ok := recs[cur.parent]
		if !ok {
			return cur.parent
		}
		if !p.side {
			return p.uuid
		}
		cur = p
	}
	return cur.parent
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

package main

import (
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	L := m.layout()

	tabbar := m.renderTabBar(L)
	div := styleBorder2.Render(strings.Repeat("─", L.w))
	body := m.renderBody(L)
	status := m.renderStatus(L)
	footer := m.footerView(L)

	content := tabbar + "\n" + div + "\n" + body + "\n" + status + "\n" + footer
	// Paint the whole terminal with Omni's background so it fills edge to edge
	// (no black surround) and every row spans the full width, at whatever size
	// the window currently is.
	return lipgloss.NewStyle().
		Width(L.w).
		Height(L.h).
		Background(colBG).
		Render(content)
}

// --- tab bar ---

func (m Model) renderTabBar(L layout) string {
	logo := lipgloss.NewStyle().Foreground(colAccent).Bold(true).Render("◆ OMNI")
	b := strings.Builder{}
	b.WriteString("  " + logo + "   ")
	pendingWaiting := len(m.pending) > 0
	for i := range tabNames {
		label := " " + m.tabLabel(i) + " "
		switch {
		case tab(i) == m.tab && tab(i) == tabPermissions && pendingWaiting:
			// Active Permissions tab with work waiting: highlight in the danger
			// colour so the counter reads as an alert, not just a selected tab.
			b.WriteString(lipgloss.NewStyle().Foreground(colBG).Background(colDanger).Bold(true).Render(label))
		case tab(i) == m.tab:
			b.WriteString(lipgloss.NewStyle().Foreground(colBG).Background(colAccent).Bold(true).Render(label))
		case tab(i) == tabPermissions && pendingWaiting:
			// Inactive but pending: red counter draws the eye from any other tab.
			b.WriteString(styleDanger.Bold(true).Render(label))
		default:
			b.WriteString(styleMuted.Render(label))
		}
		b.WriteString(" ")
	}
	// Right-aligned live-mirror indicator + pending alert.
	right := dot(colBusy) + " " + styleMuted.Render("live mirror")
	if n := len(m.pending); n > 0 {
		right = styleDanger.Render("● "+itoa(n)+" waiting") + "   " + right
	}
	used := lipgloss.Width(stripANSI(b.String()))
	gap := max(1, L.w-used-lipgloss.Width(stripANSI(right))-1)
	b.WriteString(strings.Repeat(" ", gap) + right)
	return b.String()
}

// --- body: sidebar | main ---

func (m Model) renderBody(L layout) string {
	main := m.mainLines(L)

	// Full-width, no sidebar (observe mode): just the main panel.
	if L.sidebarW == 0 {
		var rows []string
		for i := 0; i < L.bodyH; i++ {
			right := ""
			if i < len(main) {
				right = main[i]
			}
			rows = append(rows, " "+clampAnsi(right, L.w-1))
		}
		return strings.Join(rows, "\n")
	}

	sidebar := m.sidebarLines(L)
	vbar := styleBorder2.Render("│")

	var rows []string
	for i := 0; i < L.bodyH; i++ {
		left := ""
		if i < len(sidebar) {
			left = sidebar[i]
		}
		right := ""
		if i < len(main) {
			right = main[i]
		}
		rows = append(rows, padRightAnsi(left, L.sidebarW)+vbar+" "+clampAnsi(right, L.mainW-1))
	}
	return strings.Join(rows, "\n")
}

// sidebarLines builds the scrollable list for the active tab, exactly bodyH
// rows tall (padded/clipped), with the selection highlighted.
func (m Model) sidebarLines(L layout) []string {
	var items []string
	switch m.tab {
	case tabSessions:
		items = m.sessionSidebar()
	case tabPermissions:
		items = m.permissionSidebar()
	case tabSkills:
		items = m.skillSidebar()
	case tabAgents:
		items = m.agentSidebar()
	}

	cursor := m.activeCursor()
	off := m.listOffset
	// Keep the cursor within the visible window.
	if cursor < off {
		off = cursor
	}
	if cursor >= off+L.bodyH {
		off = cursor - L.bodyH + 1
	}
	if off < 0 {
		off = 0
	}

	lines := make([]string, 0, L.bodyH)
	for i := 0; i < L.bodyH; i++ {
		idx := off + i
		if idx >= len(items) {
			lines = append(lines, "")
			continue
		}
		row := items[idx]
		if idx == cursor {
			row = styleSelectedRow.Render(padRight(stripANSI(row), L.sidebarW-1))
		}
		lines = append(lines, " "+clampAnsi(row, L.sidebarW-1))
	}
	return lines
}

// mainLines is the detail panel for whatever is selected, clipped to bodyH rows.
func (m Model) mainLines(L layout) []string {
	// The Sessions detail is split: a sticky header, a scrollable conversation in
	// the middle, and a pinned footer (prompt / choices). Only the middle scrolls.
	if m.tab == tabSessions {
		return m.sessionPanel(L)
	}

	var content []string
	switch m.tab {
	case tabPermissions:
		content = m.permissionMain(L)
	case tabSkills:
		content = m.skillMain(L)
	case tabAgents:
		content = m.agentMain(L)
	}

	bottom := max(0, len(content)-L.bodyH)
	scroll := m.mainScroll
	if scroll > bottom {
		scroll = bottom
	}
	if scroll < 0 {
		scroll = 0
	}
	lines := make([]string, 0, L.bodyH)
	for i := 0; i < L.bodyH; i++ {
		idx := scroll + i
		if idx < len(content) {
			lines = append(lines, content[idx])
		} else {
			lines = append(lines, "")
		}
	}
	return lines
}

// sessionParts builds the three regions of the Sessions detail and the height
// available to the scrollable middle. The header is clipped if it would crowd
// out the footer, so the prompt bar always stays visible.
func (m Model) sessionParts(L layout) (header, convo, footer []string, avail int) {
	header = m.sessionHeader(L)
	convo = m.sessionConvo(L)
	footer = m.sessionFooter(L)
	maxHeader := L.bodyH - len(footer) - 1
	if maxHeader < 0 {
		maxHeader = 0
	}
	if len(header) > maxHeader {
		header = header[:maxHeader]
	}
	avail = L.bodyH - len(header) - len(footer)
	if avail < 1 {
		avail = 1
	}
	return header, convo, footer, avail
}

// sessionPanel stitches the sticky header, the scrolled conversation window, and
// the pinned footer into exactly bodyH rows.
func (m Model) sessionPanel(L layout) []string {
	header, convo, footer, avail := m.sessionParts(L)

	bottom := max(0, len(convo)-avail)
	scroll := m.mainScroll
	if m.follow { // live-follow pins to the newest turn
		scroll = bottom
	}
	if scroll > bottom {
		scroll = bottom
	}
	if scroll < 0 {
		scroll = 0
	}

	out := make([]string, 0, L.bodyH)
	out = append(out, header...)
	for i := 0; i < avail; i++ {
		if idx := scroll + i; idx < len(convo) {
			out = append(out, convo[idx])
		} else {
			out = append(out, "")
		}
	}
	out = append(out, footer...)
	for len(out) < L.bodyH {
		out = append(out, "")
	}
	return out[:L.bodyH]
}

// --- Sessions sidebar + main ---

func (m Model) sessionSidebar() []string {
	rows := m.rows()
	if len(rows) == 0 {
		return []string{styleMuted.Render(" no sessions")}
	}
	var out []string
	for _, r := range rows {
		s := r.s
		var d, name string
		name = m.cfg.DisplayName(s)
		if !r.live {
			if nick, ok := m.cfg.Nicknames[s.SessionID]; ok {
				name = nick
			} else {
				name = r.saved.Name
			}
		}
		switch {
		case !r.live:
			d = styleMuted.Render("○")
		case s.IsBusy():
			d = dot(colBusy)
		case s.IsIdle(m.idleThreshold()):
			d = dot(colAccent)
		default:
			d = dot(colMuted)
		}
		sub := ""
		if r.live {
			if s.IsBusy() {
				sub = styleBusy.Render("working")
			} else {
				sub = styleMuted.Render(m.surfaceOf(s) + " · " + formatDuration(s.QuietFor()))
			}
		} else {
			sub = styleMuted.Render("ended " + formatDuration(r.saved.EndedAgo()) + " ago")
		}
		out = append(out, d+" "+truncate(name, 26)+"\n   "+sub)
	}
	// Flatten two-line rows into single lines by splitting — but the sidebar
	// renderer treats each slice entry as one line, so keep it to one line.
	flat := make([]string, 0, len(out))
	for _, o := range out {
		parts := strings.SplitN(o, "\n", 2)
		flat = append(flat, parts[0])
	}
	return flat
}

// sessionHeader is the sticky top of the detail panel: title, cwd, hook status,
// project skills/agents, and — when not composing — the live status/agents block.
func (m Model) sessionHeader(L layout) []string {
	r, ok := m.selectedRow()
	if !ok {
		return []string{styleMuted.Render("Select a session on the left.")}
	}
	s := r.s
	var b []string
	title := m.cfg.DisplayName(s)
	if !r.live {
		title = r.saved.Name
		if nick, ok := m.cfg.Nicknames[s.SessionID]; ok {
			title = nick
		}
	}
	if m.mode == modeObserve {
		b = append(b, styleHeader.Render(" OBSERVING ")+"  "+styleMuted.Render("read-only · won't interrupt · p to prompt · esc to exit"))
	}
	head := styleTitle.Render(title)
	if r.live {
		if s.IsBusy() {
			head += "  " + styleBusy.Render("● working")
		} else {
			head += "  " + styleMuted.Render("● "+m.surfaceOf(s))
		}
	} else {
		head += "  " + styleMuted.Render("○ ended "+formatDuration(r.saved.EndedAgo())+" ago")
	}
	b = append(b, head, styleMuted.Render(s.Cwd))

	// Live status + AGENTS come first so they survive if the header is clipped —
	// this is the live mirror of what the real session is doing right now.
	if r.live && m.activityOK && m.mode != modeCompose {
		b = append(b, m.liveStatusLine(s))
		if cur, ok := m.activity.Current(); ok {
			label := cur.ActiveForm
			if label == "" {
				label = cur.Subject
			}
			b = append(b, dot(colBusy)+" "+styleBusy.Render("NOW: ")+styleRow.Render(truncate(label, L.mainW-8)))
		} else if act := m.lastAction(); act != "" && s.IsBusy() {
			b = append(b, dot(colBusy)+" "+styleBusy.Render("doing: ")+styleRow.Render(truncate(act, L.mainW-10)))
		}
		if len(m.activity.Todos) > 0 {
			b = append(b, styleMuted.Render("tasks "+itoa(m.activity.DoneCount())+"/"+itoa(len(m.activity.Todos))+" done"))
		}
		b = append(b, m.agentLines(L)...)
	}

	// Secondary context lower down (clipped first when space is tight).
	b = append(b, m.hookStatusLine())
	if len(m.sessSkills) > 0 {
		b = append(b, styleMuted.Render("skills: ")+styleRow.Render(truncate(strings.Join(m.sessSkills, ", "), L.mainW-9)))
	}
	if len(m.sessAgents) > 0 {
		b = append(b, styleMuted.Render("agents: ")+styleRow.Render(truncate(strings.Join(m.sessAgents, ", "), L.mainW-9)))
	}

	// A conversation label ends the sticky region so the scroll starts cleanly.
	label := styleMuted.Render("CONVERSATION")
	if r.live {
		if m.follow {
			label += "   " + dot(colBusy) + " " + styleBusy.Render("● live mirror")
		} else {
			label += "   " + styleMuted.Render("(scrolled up — ↓ to resume live)")
		}
	}
	b = append(b, "", label)
	return b
}

// sessionConvo is the scrollable middle: the recent turns and any REPLY.
func (m Model) sessionConvo(L layout) []string {
	r, ok := m.selectedRow()
	if !ok {
		return nil
	}
	s := r.s
	if m.convoID != s.SessionID && len(m.convo) == 0 {
		return []string{styleMuted.Render("  loading…")}
	}
	turns := collapseConvo(m.convo)
	if len(turns) == 0 {
		return []string{styleMuted.Render("  (no transcript found)")}
	}
	var b []string
	const recentTurns = 40
	if len(turns) > recentTurns {
		turns = turns[len(turns)-recentTurns:]
		b = append(b, styleMuted.Render("  · earlier turns hidden — scroll up in the session itself"))
	}
	for _, t := range turns {
		b = append(b, renderTurn(t, L.mainW)...)
	}
	if m.lastReply != "" {
		b = append(b, "", styleMuted.Render("REPLY"))
		b = append(b, wrapPlainStyled(displayText(m.lastReply), L.mainW-2, styleRow)...)
	}
	return b
}

// sessionFooter is the pinned bottom: the choice selector, or the compose prompt
// bar with the live status and this session's agents.
func (m Model) sessionFooter(L layout) []string {
	r, ok := m.selectedRow()
	if !ok {
		return nil
	}
	s := r.s
	var b []string

	// Observe mode is strictly read-only: no prompt bar, no choice selector.
	if m.mode == modeObserve {
		return nil
	}

	// Choice selector replaces the prompt bar when the model offered options.
	if m.mode != modeCompose && len(m.choices) > 0 {
		b = append(b, "", styleAccent.Render("Choose an option")+styleMuted.Render("  ↑↓ move · enter select · p type · esc dismiss"))
		for i, c := range m.choices {
			line := "  " + styleMuted.Render(c.marker+")") + " " + styleRow.Render(truncate(c.label, L.mainW-8))
			if i == m.choiceCursor {
				line = "  " + styleAccent.Render("▶ "+c.marker+") ") + styleTitle.Render(truncate(c.label, L.mainW-8))
			}
			b = append(b, line)
		}
		return b
	}

	if m.mode == modeCompose {
		b = append(b, "")
		if r.live && m.activityOK {
			b = append(b, "  "+m.liveStatusLine(s))
		}
		b = append(b, m.promptBox(L)...)
		hint := styleMuted.Render("enter send · shift+enter newline · esc cancel")
		if len(m.promptHistory) > 0 {
			hint += styleMuted.Render(" · ↑↓ recall sent")
		}
		if m.sending {
			hint = m.spinnerView(0) + styleMuted.Render("  sending…")
		}
		b = append(b, "  "+hint)
		// Queued messages waiting to send to this session.
		if q := m.queue[s.SessionID]; len(q) > 0 {
			b = append(b, styleMuted.Render("  queued "+itoa(len(q))+":"))
			for i, msg := range q {
				if i >= 3 {
					b = append(b, styleMuted.Render("    · "+itoa(len(q)-3)+" more"))
					break
				}
				b = append(b, "    "+styleMuted.Render(itoa(i+1)+". ")+styleRow.Render(truncate(oneLine(msg), L.mainW-10)))
			}
		}
		b = append(b, m.agentLines(L)...)
	}
	return b
}

// promptBox renders the textarea inside a rounded, coral border — the Claude Code
// input look — painted on the panel background so no foreign white shows through.
func (m Model) promptBox(L layout) []string {
	inner := lipgloss.NewStyle().Background(colBG).Render(m.textarea.View())
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colAccent).
		Background(colBG).
		Width(L.mainW - 4).
		Render(inner)
	return strings.Split(box, "\n")
}

// liveStatusLine is the one-line "what's happening right now" summary: the
// session state (working / waiting / idle), the model actually in action, and
// how long it's been in that state — the same signal Claude shows in-session.
func (m Model) liveStatusLine(s Session) string {
	// While the model is working, lead with the animated thinking spinner.
	if s.IsBusy() {
		parts := []string{m.spinnerView(s.QuietFor())}
		if mdl := m.currentModel(); mdl != "" {
			parts = append(parts, styleMuted.Render(mdl))
		}
		return strings.Join(parts, styleMuted.Render(" · "))
	}
	var state string
	if s.IsIdle(m.idleThreshold()) {
		state = dot(colAccent) + " " + styleAccent.Render("idle")
	} else {
		state = dot(colMuted) + " " + styleMuted.Render("waiting")
	}
	parts := []string{state}
	if mdl := m.currentModel(); mdl != "" {
		parts = append(parts, styleRow.Render(mdl))
	}
	if d := s.QuietFor(); d > 0 {
		parts = append(parts, styleMuted.Render(formatDuration(d)))
	}
	return strings.Join(parts, styleMuted.Render(" · "))
}

// spinnerFrames is a braille throbber; thinkingWords are the whimsical gerunds
// Claude Code cycles through while it works.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
var thinkingWords = []string{
	"Thinking", "Pondering", "Meandering", "Noodling", "Percolating",
	"Conjuring", "Ruminating", "Finagling", "Discombobulating", "Frolicking",
	"Simmering", "Marinating", "Cogitating", "Vibing",
}

// spinnerView is the animated "⠹ Meandering… (1m41s)" status. The braille frame
// advances every anim tick; the word changes roughly every two seconds; elapsed
// is how long the current turn has been running.
func (m Model) spinnerView(elapsed time.Duration) string {
	frame := spinnerFrames[m.frame%len(spinnerFrames)]
	word := thinkingWords[(m.frame/18)%len(thinkingWords)]
	line := styleBusy.Render(frame) + " " + styleBusy.Render(word+"…")
	if elapsed > 0 {
		line += " " + styleMuted.Render("("+formatDuration(elapsed)+")")
	}
	return line
}

// currentModel is the model behind the most recent Claude turn — the one in
// action right now. Walks the loaded conversation backwards.
func (m Model) currentModel() string {
	for i := len(m.convo) - 1; i >= 0; i-- {
		if m.convo[i].role == "claude" && m.convo[i].model != "" {
			return prettyModel(m.convo[i].model)
		}
	}
	return ""
}

// lastAction is the most recent tool line — the concrete thing being done
// (e.g. "Edit view.go", "Bash: go build"). Used when there's no in-progress todo.
func (m Model) lastAction() string {
	for i := len(m.convo) - 1; i >= 0; i-- {
		if m.convo[i].role == "tool" && m.convo[i].text != "" {
			return m.convo[i].text
		}
	}
	return ""
}

// agentLines renders the background/Task agents this session has launched: a
// header with the running count, then one line each (running first) with the
// live or final elapsed time — mirroring "Waiting for N background agents".
func (m Model) agentLines(L layout) []string {
	agents := m.activity.Agents
	if len(agents) == 0 {
		return nil
	}
	running := runningCount(agents)
	header := styleMuted.Render("AGENTS")
	if running > 0 {
		header += "   " + dot(colBusy) + " " + styleBusy.Render("waiting for "+itoa(running)+" to finish")
	} else {
		header += "   " + styleMuted.Render("all finished")
	}
	if tot := totalAgentTokens(agents); tot > 0 {
		header += styleMuted.Render("  · ↓ " + formatTokens(tot) + " tokens")
	}
	out := []string{"", header}
	const maxRows = 10
	for i, a := range agents {
		if i >= maxRows {
			out = append(out, styleMuted.Render("  · "+itoa(len(agents)-maxRows)+" more"))
			break
		}
		out = append(out, "  "+m.agentRow(a, L.mainW-2))
	}
	return out
}

// agentRow formats one agent like Claude Code's list: a live status marker, the
// agent type, its task, then a right-aligned "elapsed · ↓ tokens". A running
// agent shows an animated spinner and a ticking timer in the busy colour so you
// can watch it work in real time; a finished one shows a green check and its
// final elapsed/token totals, muted.
func (m Model) agentRow(a Subagent, w int) string {
	var mark, right string
	elapsed := formatDuration(a.Elapsed())
	tokens := ""
	if a.Tokens > 0 {
		tokens = " · ↓ " + formatTokens(a.Tokens) + " tokens"
	}

	if a.Running {
		// Animated braille throbber + live, ticking timer, both in the busy colour.
		frame := spinnerFrames[m.frame%len(spinnerFrames)]
		mark = styleBusy.Render(frame)
		right = styleBusy.Render(elapsed) + styleMuted.Render(tokens)
	} else {
		mark = styleBusy.Render("✓")
		right = styleMuted.Render(elapsed + tokens + " · done")
	}

	// Plain (unstyled) right text, to measure its printed width for alignment.
	rightPlain := elapsed + tokens
	if !a.Running {
		rightPlain += " · done"
	}

	typ := ""
	if a.Type != "" {
		typ = styleAccent.Render(a.Type) + " "
	}
	// Fit the task label into whatever space is left after mark, type, and right.
	used := lipgloss.Width(stripANSI(mark)) + 1 + lipgloss.Width(stripANSI(typ)) + lipgloss.Width(rightPlain) + 2
	labelW := max(6, w-used)
	label := styleRow.Render(truncate(a.Name, labelW))

	left := mark + " " + typ + label
	gap := max(1, w-lipgloss.Width(stripANSI(left))-lipgloss.Width(rightPlain))
	return left + strings.Repeat(" ", gap) + right
}

// hookStatusLine shows where the permission hook is active for this session and
// the keys to change it — so installing is always an explicit per-project or
// global choice, never automatic.
func (m Model) hookStatusLine() string {
	var state string
	switch {
	case m.hookGlobal:
		state = styleBusy.Render("● global")
	case m.hookProject:
		state = styleBusy.Render("● this project")
	default:
		state = styleMuted.Render("○ off")
	}
	proj := "[H] project"
	if m.hookProject {
		proj = "[H] remove from project"
	}
	glob := "[G] global"
	if m.hookGlobal {
		glob = "[G] remove global"
	}
	return styleMuted.Render("permission hook: ") + state + "   " +
		styleAccent.Render(proj) + styleMuted.Render("   ") + styleAccent.Render(glob)
}

func renderTurn(t convoTurn, w int) []string {
	// Collapsed tool-call group and agent-completion notes render as a single
	// muted status line — the "commands the model ran" folded away, not dumped.
	switch t.role {
	case "tools":
		return []string{"   " + styleMuted.Render(t.text)}
	case "note":
		return []string{"   " + styleMuted.Render("⚙ ") + styleRow.Render(t.text)}
	}

	var tag string
	var style lipgloss.Style
	switch t.role {
	case "you":
		tag = styleAccent.Render("You")
		style = styleRow
	case "claude":
		tag = lipgloss.NewStyle().Foreground(colBusy).Bold(true).Render("Claude")
		if mdl := prettyModel(t.model); mdl != "" {
			tag += " " + styleMuted.Render(mdl)
		}
		style = styleRow
	default:
		tag = styleMuted.Render("  ⚙")
		style = styleMuted
	}
	body := wrapPlain(t.text, w-2)
	out := make([]string, 0, len(body)+1)
	if t.role == "tool" {
		for _, ln := range body {
			out = append(out, "   "+style.Render(ln))
		}
		return out
	}
	out = append(out, tag)
	for _, ln := range body {
		out = append(out, "  "+style.Render(ln))
	}
	out = append(out, "")
	return out
}

// collapseConvo folds each run of consecutive tool calls into one "▸ N tool
// calls · Edit, Bash…" status line, so the conversation reads as a dialogue and
// the model's mechanical steps stay tucked away.
func collapseConvo(in []convoTurn) []convoTurn {
	var out []convoTurn
	for i := 0; i < len(in); {
		if in[i].role == "tool" {
			var names []string
			j := i
			for j < len(in) && in[j].role == "tool" {
				names = append(names, in[j].text)
				j++
			}
			out = append(out, convoTurn{role: "tools", text: summarizeTools(names)})
			i = j
			continue
		}
		out = append(out, in[i])
		i++
	}
	return out
}

func summarizeTools(names []string) string {
	if len(names) == 1 {
		return "▸ " + names[0]
	}
	seen := map[string]bool{}
	var kinds []string
	for _, n := range names {
		k := n
		if idx := strings.IndexAny(n, ": "); idx > 0 {
			k = n[:idx]
		}
		if !seen[k] {
			seen[k] = true
			kinds = append(kinds, k)
		}
	}
	return "▸ " + itoa(len(names)) + " tool calls · " + truncate(strings.Join(kinds, ", "), 40)
}

// --- Permissions sidebar + main ---

func (m Model) permissionSidebar() []string {
	items := m.permItems()
	if len(items) == 0 {
		return []string{styleMuted.Render(" nothing waiting")}
	}
	var out []string
	for _, it := range items {
		if it.pending != nil {
			r := it.pending
			mark := dot(colAccent)
			if r.Risk == riskWarn {
				mark = styleDanger.Render("▲")
			}
			out = append(out, mark+" "+truncate(r.Tool+" · "+r.Project, 28))
			continue
		}
		// An auto-approving session row.
		out = append(out, styleBusy.Render("✓")+" "+styleMuted.Render(truncate("auto · "+m.sessionLabel(it.autoID), 28)))
	}
	return out
}

func (m Model) permissionMain(L layout) []string {
	// An auto-approving session is selected → show it and how to turn it off.
	if it, ok := m.selectedPermItem(); ok && it.pending == nil && it.autoID != "" {
		live := ""
		for _, s := range m.sessions {
			if s.SessionID == it.autoID {
				live = s.Cwd
			}
		}
		b := []string{
			styleTitle.Render(m.sessionLabel(it.autoID)) + "  " + styleBusy.Render("● auto-approving safe"),
		}
		if live != "" {
			b = append(b, styleMuted.Render(live))
		}
		b = append(b,
			"",
			styleRow.Render("Safe actions from this session are approved automatically."),
			styleMuted.Render("Flagged (risky) actions still ask every time."),
			"",
			styleAccent.Render("[s] or [d] turn off auto-approve for this session"),
		)
		return b
	}

	r, ok := m.selectedPending()
	if !ok {
		s := []string{styleMuted.Render("No permission requests waiting.")}
		if m.policy.AllGlobal {
			s = append(s, styleAccent.Render("Global auto-approve is ON (risky actions still ask)."))
		} else if n := len(m.policy.AllSessions); n > 0 {
			s = append(s, styleAccent.Render("Auto-approving safe actions for "+itoa(n)+" session(s) (risky ones still ask)."))
		}
		return s
	}
	var b []string
	head := styleTitle.Render(r.Tool + " · " + r.Project)
	if r.Risk == riskWarn {
		head += "  " + lipgloss.NewStyle().Foreground(colBG).Background(colDanger).Bold(true).Render(" DANGER ")
	}
	b = append(b, head, styleMuted.Render(r.Cwd), "")
	b = append(b, styleMuted.Render("COMMAND"))
	b = append(b, wrapPlainStyled(r.Summary, L.mainW-2, styleRow)...)
	if len(r.Reasons) > 0 {
		b = append(b, "", styleDanger.Render("⚠ "+strings.Join(r.Reasons, ", ")))
	}
	safeHere := m.policy.AllSessions[r.SessionID]
	autoLine := styleAccent.Render("[s] auto-approve safe here")
	if safeHere {
		autoLine = styleBusy.Render("● auto-approving safe here") + styleMuted.Render("  [s] turn off")
	}
	b = append(b, "", styleAccent.Render("[a] approve    [d] deny")+styleMuted.Render("    [A] approve all safe"))
	b = append(b, autoLine)
	return b
}

// --- Skills sidebar + main ---

func (m Model) skillSidebar() []string {
	list := m.skillsList()
	if len(list) == 0 {
		return []string{styleMuted.Render(" none found")}
	}
	var out []string
	for _, s := range list {
		mark := styleBusy.Render("✓")
		if !s.Installed {
			mark = styleAccent.Render("+")
			if s.IsZip {
				mark = styleAccent.Render("⇩")
			}
		}
		out = append(out, mark+" "+truncate(s.Name, 28))
	}
	return out
}

func (m Model) skillMain(L layout) []string {
	if m.mode == modeCreateSkill {
		return m.createSkillLines(L)
	}
	s, ok := m.selectedSkill()
	if !ok {
		return []string{styleMuted.Render("Press [c] to create a prompt-only skill, or drop a skill folder or .zip into ~/Downloads.")}
	}
	status := styleBusy.Render("installed")
	if !s.Installed {
		status = styleAccent.Render("available in Downloads")
	}
	b := []string{styleTitle.Render(s.Name) + "  " + status, styleMuted.Render(s.Path), ""}
	if s.Description != "" {
		b = append(b, wrapPlainStyled(s.Description, L.mainW-2, styleRow)...)
	}
	b = append(b, "")
	if s.Installed {
		b = append(b, styleMuted.Render("[x] remove"))
	} else {
		b = append(b, styleAccent.Render("[i] install")+styleMuted.Render("  → ~/.claude/skills/"))
	}
	return b
}

// --- Agents sidebar + main ---

func (m Model) agentSidebar() []string {
	items := m.agentItems()
	if len(items) == 0 {
		return []string{styleMuted.Render(" none — press c to create")}
	}
	var out []string
	for _, it := range items {
		if it.live != nil {
			mark := styleBusy.Render("◆")
			if !it.live.sub.Running {
				mark = styleMuted.Render("✓")
			}
			out = append(out, mark+" "+truncate(it.live.sub.Name, 28))
			continue
		}
		mark := styleBusy.Render("✓")
		if !it.def.Installed {
			mark = styleAccent.Render("+")
		}
		out = append(out, mark+" "+truncate(it.def.Name, 28))
	}
	return out
}

func (m Model) agentMain(L layout) []string {
	if m.mode == modeCreateAgent {
		return m.createAgentLines(L)
	}
	// A running/recent subagent is selected → show its live detail.
	if it, ok := m.selectedAgentItem(); ok && it.live != nil {
		return m.liveAgentDetail(L, *it.live)
	}
	a, ok := m.selectedAgent()
	if !ok {
		return []string{styleMuted.Render("Press [c] to create an agent, or drop one into ~/Downloads.")}
	}
	status := styleBusy.Render("installed")
	if !a.Installed {
		status = styleAccent.Render("available in Downloads")
	}
	b := []string{styleTitle.Render(a.Name) + "  " + status}
	if a.Model != "" {
		b = append(b, styleMuted.Render("model: "+a.Model))
	}
	if a.Tools != "" {
		b = append(b, styleMuted.Render("tools: "+truncate(a.Tools, L.mainW-8)))
	}
	b = append(b, "")
	if a.Description != "" {
		b = append(b, wrapPlainStyled(a.Description, L.mainW-2, styleRow)...)
	}
	b = append(b, "")
	if a.Installed {
		b = append(b, styleMuted.Render("[x] remove"))
	} else {
		b = append(b, styleAccent.Render("[i] install")+styleMuted.Render("  → ~/.claude/agents/"))
	}
	return b
}

// liveAgentDetail shows a running/recent subagent: which session spawned it, its
// type, how long it ran, and its (approximate) token output.
func (m Model) liveAgentDetail(L layout, la liveAgent) []string {
	a := la.sub
	status := dot(colBusy) + " " + styleBusy.Render("running")
	if !a.Running {
		status = styleMuted.Render("✓ finished")
	}
	b := []string{styleTitle.Render(a.Name) + "  " + status}
	b = append(b, styleMuted.Render("session: ")+styleRow.Render(m.sessionLabel(la.SessionID)))
	if a.Type != "" {
		b = append(b, styleMuted.Render("type: ")+styleRow.Render(a.Type))
	}
	b = append(b, styleMuted.Render("elapsed: ")+styleRow.Render(formatDuration(a.Elapsed())))
	if a.Tokens > 0 {
		b = append(b, styleMuted.Render("tokens: ")+styleRow.Render("↓ "+formatTokens(a.Tokens)))
	}
	b = append(b, "", styleAccent.Render("[enter] open its session"))
	return b
}

func (m Model) createAgentLines(L layout) []string {
	f := m.agentForm
	field := func(label, val string, active bool) []string {
		l := styleMuted.Render(label)
		if active {
			l = styleAccent.Render(label)
		}
		return []string{l, "  " + val, ""}
	}
	var b []string
	b = append(b, styleTitle.Render("Create agent"), "")
	b = append(b, field("name", f.name.View(), f.focus == 0)...)
	b = append(b, field("description", f.desc.View(), f.focus == 1)...)
	b = append(b, field("tools (blank = all)", f.tools.View(), f.focus == 2)...)
	b = append(b, styleMuted.Render("prompt"))
	b = append(b, indent(f.prompt.View(), 2))
	b = append(b, "", styleMuted.Render("tab: next field   ctrl+s: create   esc: cancel"))
	return b
}

// createSkillLines renders the prompt-only "create skill" form. The result is a
// SKILL.md under ~/.claude/skills that runs on whatever model is installed and
// may use all of its capabilities.
func (m Model) createSkillLines(L layout) []string {
	f := m.skillForm
	field := func(label, val string, active bool) []string {
		l := styleMuted.Render(label)
		if active {
			l = styleAccent.Render(label)
		}
		return []string{l, "  " + val, ""}
	}
	var b []string
	b = append(b, styleTitle.Render("Create skill")+"  "+styleMuted.Render("prompt-only · uses the installed model's full capabilities"), "")
	b = append(b, field("name", f.name.View(), f.focus == 0)...)
	b = append(b, field("description (when to use it)", f.desc.View(), f.focus == 1)...)
	b = append(b, styleMuted.Render("prompt"))
	if f.focus == 2 {
		b[len(b)-1] = styleAccent.Render("prompt")
	}
	b = append(b, indent(f.prompt.View(), 2))
	b = append(b, "", styleMuted.Render("→ ~/.claude/skills/"))
	b = append(b, styleMuted.Render("tab: next field   ctrl+s: create   esc: cancel"))
	return b
}

// --- status + footer ---

func (m Model) renderStatus(L layout) string {
	// New-session prompt takes over the status line.
	if m.mode == modeNewSession {
		return "  " + styleAccent.Render("New session in: ") + m.newDir.View()
	}
	// While observing, keep a live, ticking status of the watched session here so
	// it stays visible at the bottom even as the conversation scrolls.
	if m.mode == modeObserve {
		if r, ok := m.selectedRow(); ok && r.live && m.activityOK {
			return "  " + m.liveStatusLine(r.s)
		}
	}
	if m.status == "" {
		return styleMuted.Render("  " + m.tabHint())
	}
	st := styleAccent
	if m.statusErr {
		st = styleDanger
	}
	return "  " + st.Render("› "+m.status)
}

func (m Model) tabHint() string {
	switch m.tab {
	case tabSessions:
		n := len(m.sessions)
		return itoa(n) + " live · " + itoa(len(m.ended)) + " saved · ↑↓ select · enter to prompt"
	case tabPermissions:
		return itoa(len(m.pending)) + " waiting · a approve · d deny · s auto-approve safe here"
	case tabSkills:
		return itoa(len(m.skillsInstalled)) + " installed · c create · i install from Downloads"
	case tabAgents:
		return itoa(len(m.agentsInstalled)) + " installed · c create · i install"
	}
	return ""
}

func (m Model) footerView(L layout) string {
	var keys []string
	switch m.mode {
	case modeNewSession:
		keys = []string{k("enter") + " start claude here", k("esc") + " cancel"}
	case modeObserve:
		keys = []string{k("↑↓/JK") + " scroll", k("p") + " prompt", k("esc") + " exit (read-only)"}
	case modeCompose:
		keys = []string{k("enter") + " send", k("shift+enter") + " newline", k("esc") + " cancel"}
	case modeRename:
		keys = []string{k("enter") + " save", k("esc") + " cancel"}
	case modeConfirmEnd:
		keys = []string{k("y") + " confirm", k("n") + " cancel"}
	case modeCreateAgent, modeCreateSkill:
		keys = []string{k("tab") + " field", k("ctrl+s") + " create", k("esc") + " cancel"}
	default:
		switch m.tab {
		case tabSessions:
			if len(m.choices) > 0 {
				keys = []string{k("↑↓") + " choose", k("enter") + " select", k("p") + " type", k("esc") + " dismiss"}
				break
			}
			endLabel := " end"
			if r, ok := m.selectedRow(); ok && !r.live {
				endLabel = " forget"
			}
			keys = []string{k("↑↓") + " move", k("enter") + " open", k("p") + " prompt", k("N") + " new", k("n") + " rename", k("x") + endLabel}
		case tabPermissions:
			keys = []string{k("a") + " approve", k("d") + " deny", k("A") + " all", k("s") + " safe-here", k("g") + " auto:" + onOff(m.policy.AllGlobal)}
		case tabSkills:
			keys = []string{k("c") + " create", k("i") + " install", k("x") + " remove"}
		case tabAgents:
			keys = []string{k("c") + " create", k("i") + " install", k("x") + " remove"}
		}
		keys = append(keys, k("tab")+" switch", k("q")+" quit")
	}
	return "  " + styleFooter.Render(strings.Join(keys, "   "))
}

func k(key string) string { return styleAccent.Render(key) }
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func todoLine(t TodoItem) string {
	switch t.Status {
	case "completed":
		return styleBusy.Render("✓") + " " + styleMuted.Render(strikethrough(t.Subject))
	case "in_progress":
		return styleAccent.Render("◐") + " " + styleRow.Render(t.Subject)
	default:
		return styleMuted.Render("○") + " " + styleRow.Render(t.Subject)
	}
}

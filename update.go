package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		L := m.layout()
		m.textarea.SetWidth(max(20, L.mainW-6)) // fit the detail panel, inside its border
		m.agentForm.prompt.SetWidth(min(msg.Width-6, 100))
		m.skillForm.prompt.SetWidth(min(msg.Width-6, 100))
		return m, nil

	case tickMsg:
		// Keep the heartbeat fresh so hooks know a dashboard is here, and refresh
		// the fast-moving data (sessions + pending permission requests).
		touchHeartbeat()
		cmds := []tea.Cmd{loadSessionsCmd(), loadPendingCmd(), tickCmd()}
		// Live view: while a session is open on the Sessions tab, re-read its
		// activity and conversation each tick so you watch it happen in real
		// time. No convoID reset here, so it updates in place without flashing.
		if m.tab == tabSessions {
			if r, ok := m.selectedRow(); ok && r.live {
				cmds = append(cmds, loadActivityCmd(r.s.SessionID), loadConvoCmd(r.s.SessionID))
			}
		}
		// On the Agents tab, refresh the cross-session live subagents each tick.
		if m.tab == tabAgents {
			cmds = append(cmds, loadLiveAgentsCmd(m.sessions))
		}
		// Start the spinner animation loop if something began working.
		if m.needsSpin() && !m.animOn {
			m.animOn = true
			cmds = append(cmds, animTickCmd())
		}
		return m, tea.Batch(cmds...)

	case animTickMsg:
		// Advance the spinner and keep ticking only while there's motion.
		if !m.needsSpin() {
			m.animOn = false
			return m, nil
		}
		m.frame++
		return m, animTickCmd()

	case sessionsLoadedMsg:
		prevID := ""
		if s, ok := m.selectedSession(); ok {
			prevID = s.SessionID
		}
		m.sessions = []Session(msg)
		if m.surfacesStale() {
			m.surfaces = DetectSurfaces(m.sessions)
		}
		// Fold live sessions into the durable store, then recompute which saved
		// sessions are no longer live so they show under "recently ended".
		upsertHistory(m.sessions,
			func(s Session) string { return m.cfg.DisplayName(s) },
			func(s Session) string { return m.surfaceOf(s) })
		m.ended = endedSessions(m.sessions)
		m.sessCursor = clampCursorRows(m.rows(), prevID, m.sessCursor)
		if s, ok := m.selectedSession(); ok && s.SessionID != m.activityID {
			m.activityID = s.SessionID
			m.activityOK = false
			m.convoID = ""
			return m, tea.Batch(loadActivityCmd(s.SessionID), loadConvoCmd(s.SessionID))
		}
		return m, nil

	case pendingLoadedMsg:
		m.pending = []PendingRequest(msg)
		if n := len(m.permItems()); m.permCursor >= n {
			m.permCursor = max(0, n-1)
		}
		return m, nil

	case inventoryLoadedMsg:
		m.skillsInstalled = msg.skillsInstalled
		m.skillsDownloads = msg.skillsDownloads
		m.agentsInstalled = msg.agentsInstalled
		m.agentsDownloads = msg.agentsDownloads
		return m, nil

	case activityLoadedMsg:
		if s, ok := m.selectedSession(); ok && s.SessionID == msg.sessionID {
			m.activity = msg.activity
			m.activityOK = true
			// Enrich the saved record so ended rows keep a title/last-prompt and
			// the step-by-step recap.
			enrichHistory(msg.sessionID, msg.activity.Title, msg.activity.LastPrompt, msg.activity.Steps)
		}
		return m, nil

	case liveAgentsLoadedMsg:
		m.liveAgents = []liveAgent(msg)
		if n := len(m.agentItems()); m.agentCursor >= n {
			m.agentCursor = max(0, n-1)
		}
		return m, nil

	case convoLoadedMsg:
		if s, ok := m.selectedSession(); ok && s.SessionID == msg.sessionID {
			m.convo = msg.turns
			m.convoID = msg.sessionID
			// Detect an offered choice list, but never override the user mid-type.
			if m.mode != modeCompose {
				m.choices = detectChoices(m.convo)
				if m.choiceCursor >= len(m.choices) {
					m.choiceCursor = max(0, len(m.choices)-1)
				}
			}
		}
		return m, nil

	case promptSentMsg:
		m.sending = false
		m.sendConn = ""
		if msg.err != nil {
			m.setStatus(msg.err.Error(), true)
			m.lastReply = ""
			// A failed send drops that session's queue so a bad prompt doesn't
			// wedge the whole line behind it.
			delete(m.queue, msg.sessionID)
		} else {
			m.lastReply = msg.reply
			if n := m.queueLen(msg.sessionID); n > 0 {
				m.setStatus("sent · "+itoa(n)+" queued", false)
			} else {
				m.setStatus("prompt sent", false)
			}
		}
		// Drain the next queued prompt (if any), then refresh.
		m2, next := m.startNextSend()
		return m2, tea.Batch(next, loadSessionsCmd(), loadActivityCmd(msg.sessionID))

	case actionDoneMsg:
		m.setStatus(msg.status, msg.err)
		if s, ok := m.selectedSession(); ok {
			m.refreshHookStatus(s.Cwd)
		}
		return m, tea.Batch(loadSessionsCmd(), loadPendingCmd(), loadInventoryCmd())

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m.routeToEditor(msg)
}

// handleMouse turns clicks and wheel events into selection and scrolling.
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	L := m.layout()

	switch msg.Button {
	case tea.MouseButtonWheelUp:
		if msg.X >= L.sidebarW && m.tab == tabSessions {
			return m.scrollMain(-3), nil
		}
		m.listOffset = max(0, m.listOffset-1)
		return m, nil
	case tea.MouseButtonWheelDown:
		if msg.X >= L.sidebarW && m.tab == tabSessions {
			return m.scrollMain(3), nil
		}
		m.listOffset++
		return m, nil
	}

	// Only act on a left press.
	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return m, nil
	}

	// Click on the tab bar (row 0) → switch tab.
	if msg.Y == 0 {
		for _, sp := range m.tabSpans() {
			if msg.X >= sp.lo && msg.X < sp.hi {
				return m.switchTab(sp.t)
			}
		}
		return m, nil
	}

	// Click in the sidebar list → select that row.
	if msg.X < L.sidebarW && msg.Y >= L.bodyTop && msg.Y < L.bodyTop+L.bodyH {
		idx := m.listOffset + (msg.Y - L.bodyTop)
		if idx >= 0 && idx < m.activeLen() {
			m.setActiveCursor(idx)
			if m.tab == tabSessions {
				return m.onSessionCursorMove()
			}
		}
	}
	return m, nil
}

func (m Model) switchTab(t tab) (tea.Model, tea.Cmd) {
	if m.tab == t {
		return m, nil
	}
	m.tab = t
	m.status = ""
	m.listOffset = 0
	m.mainScroll = 0
	m.follow = true
	if t == tabSessions {
		return m.onSessionCursorMove()
	}
	if t == tabAgents {
		return m, loadLiveAgentsCmd(m.sessions)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Editors/forms swallow keys until dismissed.
	switch m.mode {
	case modeCompose:
		return m.keyCompose(msg)
	case modeRename:
		return m.keyRename(msg)
	case modeConfirmEnd:
		return m.keyConfirmEnd(msg)
	case modeCreateAgent:
		return m.keyCreateAgent(msg)
	case modeCreateSkill:
		return m.keyCreateSkill(msg)
	case modeObserve:
		return m.keyObserve(msg)
	case modeNewSession:
		return m.keyNewSession(msg)
	}

	// Global keys (list mode, any tab).
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		removeHeartbeat()
		return m, tea.Quit
	case "tab", "right", "l":
		return m.switchTab((m.tab + 1) % tab(len(tabNames)))
	case "shift+tab", "left", "h":
		return m.switchTab((m.tab + tab(len(tabNames)) - 1) % tab(len(tabNames)))
	case "1":
		return m.switchTab(tabSessions)
	case "2":
		return m.switchTab(tabPermissions)
	case "3":
		return m.switchTab(tabSkills)
	case "4":
		return m.switchTab(tabAgents)
	case "r":
		return m, tea.Batch(loadSessionsCmd(), loadPendingCmd(), loadInventoryCmd())
	}

	switch m.tab {
	case tabSessions:
		return m.keySessions(msg)
	case tabPermissions:
		return m.keyPermissions(msg)
	case tabSkills:
		return m.keySkills(msg)
	case tabAgents:
		return m.keyAgents(msg)
	}
	return m, nil
}

// --- Sessions tab ---

func (m Model) keySessions(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Choice selector: when the model offered options, up/down/enter drive the
	// select and the prompt bar is hidden until dismissed.
	if len(m.choices) > 0 {
		switch msg.String() {
		case "up", "k":
			if m.choiceCursor > 0 {
				m.choiceCursor--
			}
			return m, nil
		case "down", "j":
			if m.choiceCursor < len(m.choices)-1 {
				m.choiceCursor++
			}
			return m, nil
		case "enter":
			if r, ok := m.selectedRow(); ok && r.live && m.choiceCursor < len(m.choices) {
				pick := m.choices[m.choiceCursor].marker
				m.choices = nil
				m.sending = true
				m.status = ""
				m.lastReply = ""
				return m, sendPromptCmd(pick, r.s)
			}
			return m, nil
		case "esc":
			m.choices = nil // dismiss and fall back to normal navigation
			return m, nil
		case "p":
			// Type a free answer instead of picking.
			m.choices = nil
			if r, ok := m.selectedRow(); ok && r.live {
				m.mode = modeCompose
				m.status = ""
				m.lastReply = ""
				m.textarea.Reset()
				return m, m.textarea.Focus()
			}
			return m, nil
		}
	}

	switch msg.String() {
	case "up", "k":
		if m.sessCursor > 0 {
			m.sessCursor--
			return m.onSessionCursorMove()
		}
	case "down", "j":
		if m.sessCursor < len(m.rows())-1 {
			m.sessCursor++
			return m.onSessionCursorMove()
		}
	case "J", "pgdown", " ":
		return m.scrollMain(8), nil
	case "K", "pgup":
		return m.scrollMain(-8), nil
	case "t":
		m.idleIdx = (m.idleIdx + 1) % len(idleThresholds)
	case "enter", "o":
		// Enter a session read-only — a full-width view that only watches; it
		// never sends anything, so a working session is never interrupted.
		if _, ok := m.selectedRow(); ok {
			m.mode = modeObserve
			m.status = ""
			m.follow = true
			return m, nil
		}
	case "p":
		if _, ok := m.selectedRow(); ok {
			// No busy-guard: a prompt sent to a working session is queued by Claude
			// on its side, so composing never interrupts the live turn.
			m.mode = modeCompose
			m.status = ""
			m.lastReply = ""
			m.textarea.Reset()
			return m, m.textarea.Focus()
		}
	case "N":
		// Start a new `claude` session — prompt for a directory, defaulting to the
		// selected session's project (or home).
		start := homeDir()
		if s, ok := m.selectedSession(); ok && s.Cwd != "" {
			start = s.Cwd
		}
		m.mode = modeNewSession
		m.status = ""
		m.newDir.SetValue(start)
		m.newDir.CursorEnd()
		return m, m.newDir.Focus()
	case "n":
		if s, ok := m.selectedSession(); ok {
			m.mode = modeRename
			m.rename.SetValue(m.cfg.DisplayName(s))
			m.rename.CursorEnd()
			return m, m.rename.Focus()
		}
	case "x", "delete":
		if r, ok := m.selectedRow(); ok {
			if !r.live {
				// Ended session: drop it from the saved store.
				id := r.s.SessionID
				return m, func() tea.Msg {
					removeSaved(id)
					return actionDoneMsg{"removed " + r.saved.Name + " from history", false}
				}
			}
			if r.s.IsIdle(m.idleThreshold()) {
				m.mode = modeConfirmEnd
			}
		}
	case "H":
		// Toggle the permission hook for THIS session's project only.
		if s, ok := m.selectedSession(); ok && s.Cwd != "" {
			dir := s.Cwd
			install := !m.hookProject
			cmd := func() tea.Msg {
				var changed bool
				var err error
				if install {
					changed, err = EnsureHookInstalledIn(dir)
				} else {
					changed, err = UninstallHookIn(dir)
				}
				verb := "installed for"
				if !install {
					verb = "removed from"
				}
				if err != nil {
					return actionDoneMsg{"hook change failed: " + err.Error(), true}
				}
				_ = changed
				return actionDoneMsg{"hook " + verb + " " + filepathBase(dir), false}
			}
			m.refreshHookStatus(dir) // optimistic; corrected on next selection
			m.hookProject = install
			return m, cmd
		}
	case "G":
		// Toggle the GLOBAL hook (all projects).
		install := !m.hookGlobal
		sel := ""
		if s, ok := m.selectedSession(); ok {
			sel = s.Cwd
		}
		cmd := func() tea.Msg {
			var err error
			if install {
				_, err = EnsureHookInstalled()
			} else {
				_, err = UninstallHook()
			}
			if err != nil {
				return actionDoneMsg{"global hook change failed: " + err.Error(), true}
			}
			if install {
				return actionDoneMsg{"permission hook installed globally", false}
			}
			return actionDoneMsg{"global permission hook removed", false}
		}
		m.hookGlobal = install
		_ = sel
		return m, cmd
	}
	return m, nil
}

func filepathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// scrollMain moves the session detail by delta lines. Scrolling up breaks
// live-follow so you can read back; scrolling to the bottom re-arms it.
func (m Model) scrollMain(delta int) Model {
	L := m.layout()
	_, convo, _, avail := m.sessionParts(L)
	bottom := max(0, len(convo)-avail)
	// If we were following, start from the bottom so "up" reveals history.
	if m.follow {
		m.mainScroll = bottom
	}
	m.mainScroll += delta
	if m.mainScroll < 0 {
		m.mainScroll = 0
	}
	if m.mainScroll >= bottom {
		m.mainScroll = bottom
		m.follow = true // back at the newest — resume live-follow
	} else {
		m.follow = false
	}
	return m
}

func (m Model) onSessionCursorMove() (Model, tea.Cmd) {
	m.lastReply = ""
	m.mainScroll = 0
	m.follow = true // newly opened session starts pinned to the newest activity
	m.choices = nil // a fresh session re-detects its own offered choices
	m.choiceCursor = 0
	if s, ok := m.selectedSession(); ok {
		m.activityID = s.SessionID
		m.activityOK = false
		m.convoID = "" // triggers "loading…" until the convo arrives
		m.refreshHookStatus(s.Cwd)
		return m, tea.Batch(loadActivityCmd(s.SessionID), loadConvoCmd(s.SessionID))
	}
	return m, nil
}

// refreshHookStatus reads whether the permission hook is installed globally and
// for the given project, plus the project's own skills/agents, so the detail
// panel can show them.
func (m *Model) refreshHookStatus(projectDir string) {
	m.hookGlobal = GlobalHookInstalled()
	m.hookProject = projectDir != "" && ProjectHookInstalled(projectDir)
	m.sessSkills = ProjectSkillNames(projectDir)
	m.sessAgents = ProjectAgentNames(projectDir)
}

func (m Model) keyCompose(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.textarea.Blur()
		return m, nil
	case "enter", "ctrl+s":
		s, ok := m.selectedSession()
		if !ok {
			return m, nil
		}
		text := trimSpace(m.textarea.Value())
		if text == "" {
			return m, nil
		}
		// Clear the prompt bar immediately and remember it, so the user can queue
		// the next message right away — the box stays open, Claude-Code style.
		m.promptHistory = append(m.promptHistory, text)
		m.histIdx = len(m.promptHistory)
		m.textarea.Reset()
		m.status = ""
		m.lastReply = ""
		// Queue it. If a send is already running it waits its turn; otherwise it
		// starts now. Either way, sends stay serial.
		m.enqueue(s.SessionID, text)
		if m.sendConn != "" {
			m.setStatus("queued ("+itoa(m.queueLen(s.SessionID))+" waiting)", false)
			return m, nil
		}
		return m.startNextSend()
	case "shift+enter", "alt+enter", "ctrl+j":
		// Explicit newline — Enter is reserved for send.
		m.textarea.InsertString("\n")
		return m, nil
	case "up":
		// Recall the previous sent prompt when the cursor is on the first line.
		if len(m.promptHistory) > 0 && m.textarea.Line() == 0 && m.histIdx > 0 {
			m.histIdx--
			m.textarea.SetValue(m.promptHistory[m.histIdx])
			m.textarea.CursorEnd()
			return m, nil
		}
	case "down":
		// Walk back toward the newest, then to an empty box.
		if m.histIdx < len(m.promptHistory) {
			m.histIdx++
			if m.histIdx == len(m.promptHistory) {
				m.textarea.Reset()
			} else {
				m.textarea.SetValue(m.promptHistory[m.histIdx])
				m.textarea.CursorEnd()
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.textarea, cmd = m.textarea.Update(msg)
	return m, cmd
}

func (m Model) keyRename(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.rename.Blur()
		return m, nil
	case "enter":
		s, ok := m.selectedSession()
		if !ok {
			m.mode = modeList
			return m, nil
		}
		name := trimSpace(m.rename.Value())
		if name != "" && name != s.Name {
			m.cfg.Nicknames[s.SessionID] = name
			m.cfg.Save()
			writeRegistryName(s, name)
		} else if name == "" {
			delete(m.cfg.Nicknames, s.SessionID)
			m.cfg.Save()
			writeRegistryName(s, s.Name)
		}
		m.mode = modeList
		m.rename.Blur()
		return m, loadSessionsCmd()
	}
	var cmd tea.Cmd
	m.rename, cmd = m.rename.Update(msg)
	return m, cmd
}

// keyObserve drives the read-only, full-width session view. It only scrolls and
// exits — it never sends, approves, or otherwise touches the live session.
func (m Model) keyObserve(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "enter", "o", "q":
		m.mode = modeList
		return m, nil
	case "ctrl+c":
		m.quitting = true
		removeHeartbeat()
		return m, tea.Quit
	case "p":
		// Jump straight from observing to composing a prompt.
		if _, ok := m.selectedRow(); ok {
			m.mode = modeCompose
			m.status = ""
			m.lastReply = ""
			m.textarea.Reset()
			return m, m.textarea.Focus()
		}
	case "up", "k":
		return m.scrollMain(-1), nil
	case "down", "j":
		return m.scrollMain(1), nil
	case "K", "pgup":
		return m.scrollMain(-8), nil
	case "J", "pgdown", " ":
		return m.scrollMain(8), nil
	}
	return m, nil
}

// keyNewSession takes a directory and opens a new terminal running `claude`
// there. The new session appears in the list on the next refresh.
func (m Model) keyNewSession(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.newDir.Blur()
		return m, nil
	case "enter":
		dir := expandHome(trimSpace(m.newDir.Value()))
		m.mode = modeList
		m.newDir.Blur()
		return m, func() tea.Msg {
			if err := newSessionInDir(dir); err != nil {
				return actionDoneMsg{"couldn't start session: " + err.Error(), true}
			}
			return actionDoneMsg{"started a new session in " + filepathBase(dir), false}
		}
	}
	var cmd tea.Cmd
	m.newDir, cmd = m.newDir.Update(msg)
	return m, cmd
}

func (m Model) keyConfirmEnd(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "enter":
		m.mode = modeList
		if s, ok := m.selectedSession(); ok {
			return m, func() tea.Msg {
				if err := endProcess(s.PID); err != nil {
					return actionDoneMsg{"could not end session: " + err.Error(), true}
				}
				return actionDoneMsg{"ended " + s.Name, false}
			}
		}
	case "n", "esc":
		m.mode = modeList
	}
	return m, nil
}

// --- Permissions tab ---

func (m Model) keyPermissions(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// When an auto-approving session row is selected, d/s/enter turn it off.
	if it, ok := m.selectedPermItem(); ok && it.pending == nil && it.autoID != "" {
		switch msg.String() {
		case "up", "k":
			if m.permCursor > 0 {
				m.permCursor--
			}
			return m, nil
		case "down", "j":
			if m.permCursor < len(m.permItems())-1 {
				m.permCursor++
			}
			return m, nil
		case "d", "s", "enter":
			delete(m.policy.AllSessions, it.autoID)
			_ = savePolicy(m.policy)
			m.setStatus("turned off auto-approve for "+m.sessionLabel(it.autoID), false)
			return m, nil
		}
	}

	switch msg.String() {
	case "up", "k":
		if m.permCursor > 0 {
			m.permCursor--
		}
	case "down", "j":
		if m.permCursor < len(m.permItems())-1 {
			m.permCursor++
		}
	case "a", "enter":
		if r, ok := m.selectedPending(); ok {
			return m, decideCmd(r, true, "approved in Omni")
		}
	case "d":
		if r, ok := m.selectedPending(); ok {
			return m, decideCmd(r, false, "denied in Omni")
		}
	case "A":
		// Approve every non-flagged request at once; flagged ones stay.
		return m, approveAllCmd(m.pending)
	case "g":
		m.policy.AllGlobal = !m.policy.AllGlobal
		_ = savePolicy(m.policy)
		state := "off"
		if m.policy.AllGlobal {
			state = "on"
		}
		m.setStatus("global auto-approve "+state+" (flagged actions still ask)", false)
	case "s":
		// Auto-approve safe actions for just THIS session, so routine work stops
		// asking. Flagged (warn) actions still always need an explicit decision.
		if r, ok := m.selectedPending(); ok {
			if m.policy.AllSessions == nil {
				m.policy.AllSessions = map[string]bool{}
			}
			now := !m.policy.AllSessions[r.SessionID]
			m.policy.AllSessions[r.SessionID] = now
			if !now {
				delete(m.policy.AllSessions, r.SessionID)
			}
			_ = savePolicy(m.policy)
			if now {
				// Clear what's already waiting for this session in one go.
				m.setStatus("auto-approving safe actions for "+r.Project+" (flagged still ask)", false)
				return m, approveSessionSafeCmd(m.pending, r.SessionID)
			}
			m.setStatus("stopped auto-approving "+r.Project, false)
		}
	}
	return m, nil
}

func decideCmd(r PendingRequest, allow bool, reason string) tea.Cmd {
	return func() tea.Msg {
		if err := writeDecision(Decision{ID: r.ID, Allow: allow, Reason: reason}); err != nil {
			return actionDoneMsg{"could not write decision: " + err.Error(), true}
		}
		verb := "approved"
		if !allow {
			verb = "denied"
		}
		return actionDoneMsg{verb + " " + r.Tool + " · " + r.Project, false}
	}
}

// approveSessionSafeCmd clears every non-flagged request already waiting for one
// session — the immediate half of turning on per-session auto-approve, so the
// user doesn't have to click through the queue that's on screen right now.
func approveSessionSafeCmd(pending []PendingRequest, sessionID string) tea.Cmd {
	return func() tea.Msg {
		n := 0
		for _, r := range pending {
			if r.SessionID != sessionID || r.Risk == riskWarn {
				continue
			}
			if err := writeDecision(Decision{ID: r.ID, Allow: true, Reason: "auto-approved (session safe)"}); err == nil {
				n++
			}
		}
		return actionDoneMsg{"auto-approved " + itoa(n) + " safe request(s); flagged ones still ask", false}
	}
}

func approveAllCmd(pending []PendingRequest) tea.Cmd {
	return func() tea.Msg {
		n := 0
		for _, r := range pending {
			if r.Risk == riskWarn {
				continue // flagged actions are never bulk-approved
			}
			if err := writeDecision(Decision{ID: r.ID, Allow: true, Reason: "approve-all"}); err == nil {
				n++
			}
		}
		return actionDoneMsg{"approved " + itoa(n) + " request(s); flagged ones still need review", false}
	}
}

// --- Skills tab ---

func (m Model) keySkills(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	list := m.skillsList()
	switch msg.String() {
	case "up", "k":
		if m.skillCursor > 0 {
			m.skillCursor--
		}
	case "down", "j":
		if m.skillCursor < len(list)-1 {
			m.skillCursor++
		}
	case "c":
		// Author a prompt-only custom skill from a small form.
		m.mode = modeCreateSkill
		m.skillForm = newSkillForm()
		m.skillForm.focus = 0
		m.status = ""
		return m, m.skillForm.name.Focus()
	case "i", "enter":
		if s, ok := m.selectedSkill(); ok && !s.Installed {
			return m, func() tea.Msg {
				name, err := InstallSkill(s)
				if err != nil {
					return actionDoneMsg{"install failed: " + err.Error(), true}
				}
				return actionDoneMsg{"installed skill " + name, false}
			}
		}
	case "x", "delete":
		if s, ok := m.selectedSkill(); ok && s.Installed {
			return m, func() tea.Msg {
				if err := RemoveSkill(s); err != nil {
					return actionDoneMsg{"remove failed: " + err.Error(), true}
				}
				return actionDoneMsg{"removed skill " + s.Name, false}
			}
		}
	}
	return m, nil
}

func (m Model) keyCreateSkill(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		return m, nil
	case "ctrl+s":
		f := m.skillForm
		name, err := CreateSkill(f.name.Value(), f.desc.Value(), f.prompt.Value())
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		m.mode = modeList
		m.setStatus("created skill "+name, false)
		return m, loadInventoryCmd()
	case "tab", "down":
		m.skillForm.focus = (m.skillForm.focus + 1) % m.skillForm.fields()
		return m, m.focusSkillField()
	case "shift+tab", "up":
		m.skillForm.focus = (m.skillForm.focus + m.skillForm.fields() - 1) % m.skillForm.fields()
		return m, m.focusSkillField()
	}
	return m.routeToSkillForm(msg)
}

func (m *Model) focusSkillField() tea.Cmd {
	m.skillForm.name.Blur()
	m.skillForm.desc.Blur()
	m.skillForm.prompt.Blur()
	switch m.skillForm.focus {
	case 0:
		return m.skillForm.name.Focus()
	case 1:
		return m.skillForm.desc.Focus()
	default:
		return m.skillForm.prompt.Focus()
	}
}

func (m Model) routeToSkillForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.skillForm.focus {
	case 0:
		m.skillForm.name, cmd = m.skillForm.name.Update(msg)
	case 1:
		m.skillForm.desc, cmd = m.skillForm.desc.Update(msg)
	default:
		m.skillForm.prompt, cmd = m.skillForm.prompt.Update(msg)
	}
	return m, cmd
}

// --- Agents tab ---

func (m Model) keyAgents(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "up", "k":
		if m.agentCursor > 0 {
			m.agentCursor--
		}
		return m, nil
	case "down", "j":
		if m.agentCursor < len(m.agentItems())-1 {
			m.agentCursor++
		}
		return m, nil
	case "c":
		m.mode = modeCreateAgent
		m.agentForm = newAgentForm()
		m.agentForm.focus = 0
		return m, m.agentForm.name.Focus()
	case "enter":
		// A live-agent row jumps to the session that spawned it.
		if it, ok := m.selectedAgentItem(); ok && it.live != nil {
			for i, r := range m.rows() {
				if r.live && r.s.SessionID == it.live.SessionID {
					m.tab = tabSessions
					m.sessCursor = i
					return m.onSessionCursorMove()
				}
			}
			return m, nil
		}
		fallthrough
	case "i":
		if a, ok := m.selectedAgent(); ok && !a.Installed {
			return m, func() tea.Msg {
				if err := InstallAgent(a); err != nil {
					return actionDoneMsg{"install failed: " + err.Error(), true}
				}
				return actionDoneMsg{"installed agent " + a.Name, false}
			}
		}
	case "x", "delete":
		if a, ok := m.selectedAgent(); ok && a.Installed {
			return m, func() tea.Msg {
				if err := RemoveAgent(a); err != nil {
					return actionDoneMsg{"remove failed: " + err.Error(), true}
				}
				return actionDoneMsg{"removed agent " + a.Name, false}
			}
		}
	}
	return m, nil
}

func (m Model) keyCreateAgent(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		return m, nil
	case "ctrl+s":
		f := m.agentForm
		err := CreateAgent(f.name.Value(), f.desc.Value(), f.tools.Value(), "", f.prompt.Value())
		if err != nil {
			m.setStatus(err.Error(), true)
			return m, nil
		}
		m.mode = modeList
		m.setStatus("created agent "+trimSpace(f.name.Value()), false)
		return m, loadInventoryCmd()
	case "tab", "down":
		m.agentForm.focus = (m.agentForm.focus + 1) % m.agentForm.fields()
		return m, m.focusAgentField()
	case "shift+tab", "up":
		m.agentForm.focus = (m.agentForm.focus + m.agentForm.fields() - 1) % m.agentForm.fields()
		return m, m.focusAgentField()
	}
	return m.routeToAgentForm(msg)
}

func (m *Model) focusAgentField() tea.Cmd {
	m.agentForm.name.Blur()
	m.agentForm.desc.Blur()
	m.agentForm.tools.Blur()
	m.agentForm.prompt.Blur()
	switch m.agentForm.focus {
	case 0:
		return m.agentForm.name.Focus()
	case 1:
		return m.agentForm.desc.Focus()
	case 2:
		return m.agentForm.tools.Focus()
	default:
		return m.agentForm.prompt.Focus()
	}
}

func (m Model) routeToAgentForm(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m.agentForm.focus {
	case 0:
		m.agentForm.name, cmd = m.agentForm.name.Update(msg)
	case 1:
		m.agentForm.desc, cmd = m.agentForm.desc.Update(msg)
	case 2:
		m.agentForm.tools, cmd = m.agentForm.tools.Update(msg)
	default:
		m.agentForm.prompt, cmd = m.agentForm.prompt.Update(msg)
	}
	return m, cmd
}

func (m Model) routeToEditor(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeCompose:
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	case modeRename:
		var cmd tea.Cmd
		m.rename, cmd = m.rename.Update(msg)
		return m, cmd
	case modeNewSession:
		var cmd tea.Cmd
		m.newDir, cmd = m.newDir.Update(msg)
		return m, cmd
	case modeCreateAgent:
		return m.routeToAgentForm(msg)
	case modeCreateSkill:
		return m.routeToSkillForm(msg)
	}
	return m, nil
}

func clampCursorRows(rows []sessRow, prevID string, prev int) int {
	if prevID != "" {
		for i, r := range rows {
			if r.s.SessionID == prevID {
				return i
			}
		}
	}
	if prev >= len(rows) {
		return max(0, len(rows)-1)
	}
	if prev < 0 {
		return 0
	}
	return prev
}

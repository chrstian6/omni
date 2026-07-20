package main

import (
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type tab int

const (
	tabSessions tab = iota
	tabPermissions
	tabSkills
	tabAgents
)

var tabNames = []string{"Sessions", "Permissions", "Skills", "Agents"}

type mode int

const (
	modeList mode = iota
	modeCompose
	modeRename
	modeConfirmEnd
	modeCreateAgent
	modeCreateSkill // authoring a prompt-only skill from a small form
	modeObserve     // full-width, read-only view of a session — never interrupts it
	modeNewSession  // prompting for a directory to start a new `claude` session in
)

var idleThresholds = []time.Duration{
	60 * time.Second, 5 * time.Minute, 15 * time.Minute, 60 * time.Minute,
}
var idleThresholdLabels = []string{"1m", "5m", "15m", "1h"}

type Model struct {
	cfg    Config
	policy Policy
	tab    tab
	mode   mode

	// sessions
	sessions []Session
	ended    []SavedSession // saved sessions no longer live
	surfaces map[string]string
	// outboxQueue is what's waiting to be delivered, per session, refreshed on
	// the sessions tick. The view reads it on every frame — including spinner
	// frames — so it must not hit the filesystem.
	outboxQueue map[string][]OutboxMsg
	sessCursor  int
	activity    Activity
	activityID  string
	activityOK  bool
	convo       []convoTurn // full conversation of the selected session
	convoID     string

	// layout / scrolling
	listOffset int  // sidebar scroll
	mainScroll int  // main-panel scroll
	follow     bool // live-follow: pin the conversation to the newest turn

	// hook status for the selected session's project (recomputed on selection)
	hookGlobal  bool
	hookProject bool

	// project skills/agents available to the selected session
	sessSkills []string
	sessAgents []string

	// permissions
	pending    []PendingRequest
	permCursor int

	// skills
	skillsInstalled []Skill
	skillsDownloads []Skill
	skillCursor     int

	// agents
	agentsInstalled []Agent
	agentsDownloads []Agent
	agentCursor     int
	liveAgents      []liveAgent // running/recent subagents across all live sessions

	idleIdx int

	// editors / forms
	textarea  textarea.Model
	rename    textinput.Model
	newDir    textinput.Model // directory for a new `claude` session
	agentForm agentForm
	skillForm skillForm

	sending   bool
	lastReply string
	status    string // transient one-line status (actions, errors)
	statusErr bool

	// thinking-spinner animation
	frame  int  // advances while a session is busy, drives the spinner
	animOn bool // whether the fast animation ticker is currently running

	// sent-prompt history for up/down recall in the compose box
	promptHistory []string
	histIdx       int
	draft         string // text kept when compose is cancelled, restored on reopen

	// choice selector: options the model offered, navigable with up/down
	choices      []choiceOpt
	choiceCursor int
	// answeredChoices fingerprints the menu the user just answered. Delivery is
	// asynchronous, so the transcript still shows that menu for a while after the
	// pick — without this the selector reappears immediately and a second press
	// queues a duplicate answer.
	answeredChoices string

	// per-session message queue. Sends are serialized (one --resume at a time)
	// so two headless prompts never collide on the same transcript. Claude itself
	// queues the resumed prompt into the live session, so sending never interrupts.
	queue    map[string][]string
	sendConn string // sessionID currently being sent, "" when idle

	width, height int
	quitting      bool
}

// agentForm is the small "create agent" form: a set of stacked inputs.
type agentForm struct {
	name   textinput.Model
	desc   textinput.Model
	tools  textinput.Model
	prompt textarea.Model
	focus  int // which field is active
}

func newAgentForm() agentForm {
	mk := func(ph string) textinput.Model {
		t := textinput.New()
		t.Placeholder = ph
		return t
	}
	pa := textarea.New()
	pa.Placeholder = "System prompt — what this agent is and how it behaves…"
	pa.ShowLineNumbers = false
	pa.SetHeight(4)
	return agentForm{
		name:   mk("agent-name (kebab-case)"),
		desc:   mk("When should this agent be used?"),
		tools:  mk("tools (blank = all)"),
		prompt: pa,
	}
}

func (f *agentForm) fields() int { return 4 }

// skillForm is the "create skill" form: a prompt-only skill authored from a
// name, a description (when to use it), and the prompt/instructions body.
type skillForm struct {
	name   textinput.Model
	desc   textinput.Model
	prompt textarea.Model
	focus  int // which field is active
}

func newSkillForm() skillForm {
	mk := func(ph string) textinput.Model {
		t := textinput.New()
		t.Placeholder = ph
		return t
	}
	pa := textarea.New()
	pa.Placeholder = "Prompt — the instructions this skill loads. It runs on the installed model and may use all its capabilities…"
	pa.ShowLineNumbers = false
	pa.SetHeight(5)
	return skillForm{
		name:   mk("skill-name (kebab-case)"),
		desc:   mk("When should this skill be used?"),
		prompt: pa,
	}
}

func (f *skillForm) fields() int { return 3 }

func NewModel() Model {
	ta := textarea.New()
	ta.Placeholder = "Type a prompt for this session…"
	ta.ShowLineNumbers = false
	ta.SetHeight(4)

	ta.Prompt = "> " // Claude-Code-style prompt marker on each input line
	// Paint the input on the panel background and drop the default cursor-line
	// highlight, so no white block shows through the cream/near-black panel.
	ta.FocusedStyle.Base = lipgloss.NewStyle().Background(colBG)
	ta.BlurredStyle.Base = lipgloss.NewStyle().Background(colBG)
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle().Background(colBG).Foreground(colText)
	ta.FocusedStyle.Text = lipgloss.NewStyle().Background(colBG).Foreground(colText)
	ta.BlurredStyle.Text = lipgloss.NewStyle().Background(colBG).Foreground(colText)
	ta.FocusedStyle.Prompt = lipgloss.NewStyle().Background(colBG).Foreground(colAccent)
	ta.BlurredStyle.Prompt = lipgloss.NewStyle().Background(colBG).Foreground(colAccent)
	ta.FocusedStyle.Placeholder = lipgloss.NewStyle().Background(colBG).Foreground(colMuted)

	ti := textinput.New()
	ti.Placeholder = "New name"

	nd := textinput.New()
	nd.Placeholder = "~/path/to/project"

	return Model{
		cfg:         LoadConfig(),
		policy:      loadPolicy(),
		surfaces:    map[string]string{},
		outboxQueue: map[string][]OutboxMsg{},
		idleIdx:     1,
		textarea:    ta,
		rename:      ti,
		newDir:      nd,
		agentForm:   newAgentForm(),
		skillForm:   newSkillForm(),
		queue:       map[string][]string{},
	}
}

func (m Model) Init() tea.Cmd {
	touchHeartbeat() // announce the dashboard so hooks start routing here
	return tea.Batch(loadSessionsCmd(), loadPendingCmd(), loadInventoryCmd(), tickCmd())
}

// --- messages ---

type tickMsg time.Time
type animTickMsg time.Time
type sessionsLoadedMsg []Session
type pendingLoadedMsg []PendingRequest
type inventoryLoadedMsg struct {
	skillsInstalled []Skill
	skillsDownloads []Skill
	agentsInstalled []Agent
	agentsDownloads []Agent
}
type activityLoadedMsg struct {
	sessionID string
	activity  Activity
}
type convoLoadedMsg struct {
	sessionID string
	turns     []convoTurn
}
type liveAgentsLoadedMsg []liveAgent
type promptSentMsg struct {
	sessionID string
	reply     string
	err       error
}
type actionDoneMsg struct {
	status string
	err    bool
}

// --- commands ---

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// animTickCmd drives the thinking spinner. It runs faster than the 1s data tick
// (so the spinner reads as motion) and only reschedules itself while something is
// actually working — idle terminals don't redraw.
const animInterval = 110 * time.Millisecond

func animTickCmd() tea.Cmd {
	return tea.Tick(animInterval, func(t time.Time) tea.Msg { return animTickMsg(t) })
}
func loadSessionsCmd() tea.Cmd {
	return func() tea.Msg { return sessionsLoadedMsg(LoadSessions()) }
}
func loadPendingCmd() tea.Cmd {
	return func() tea.Msg { return pendingLoadedMsg(loadPending()) }
}
func loadInventoryCmd() tea.Cmd {
	return func() tea.Msg {
		return inventoryLoadedMsg{
			skillsInstalled: InstalledSkills(),
			skillsDownloads: DownloadsSkills(),
			agentsInstalled: InstalledAgents(),
			agentsDownloads: DownloadsAgents(),
		}
	}
}
func loadActivityCmd(sessionID string) tea.Cmd {
	return func() tea.Msg {
		return activityLoadedMsg{sessionID: sessionID, activity: LoadActivity(sessionID)}
	}
}
func loadConvoCmd(sessionID string) tea.Cmd {
	return func() tea.Msg {
		return convoLoadedMsg{sessionID: sessionID, turns: loadConvo(sessionID)}
	}
}

// loadLiveAgentsCmd reads the subagents of every live session so the Agents tab
// can show all running/recent background agents in one place.
func loadLiveAgentsCmd(sessions []Session) tea.Cmd {
	return func() tea.Msg {
		var out []liveAgent
		for _, s := range sessions {
			for _, a := range loadSubagents(s.SessionID) {
				out = append(out, liveAgent{SessionID: s.SessionID, sub: a})
			}
		}
		// Running first, then most-recently-started.
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].sub.Running != out[j].sub.Running {
				return out[i].sub.Running
			}
			return out[i].sub.Started.After(out[j].sub.Started)
		})
		return liveAgentsLoadedMsg(out)
	}
}

func sendPromptCmd(prompt string, s Session) tea.Cmd {
	return func() tea.Msg {
		reply, err := SendPrompt(prompt, s)
		return promptSentMsg{sessionID: s.SessionID, reply: reply, err: err}
	}
}

// --- selection helpers ---

// sessRow is one entry in the Sessions tab: either a live session or a saved
// (ended) one. The cursor walks live rows first, then ended rows.
type sessRow struct {
	s     Session
	live  bool
	saved SavedSession
}

// rows lists live sessions first, then ended ones. Live sessions are clustered
// by the surface that owns them (all the VS Code ones together, then Cursor,
// then a plain shell) because ownership is the organizing idea of the dashboard:
// a session belongs to the terminal it was started in, and that's where its
// prompts get delivered. Ordering within a cluster is by start time so rows stay
// put as sessions flip between busy and waiting.
func (m Model) rows() []sessRow {
	rows := make([]sessRow, 0, len(m.sessions)+len(m.ended))
	live := make([]Session, len(m.sessions))
	copy(live, m.sessions)
	sort.SliceStable(live, func(i, j int) bool {
		si, sj := m.surfaceOf(live[i]), m.surfaceOf(live[j])
		if si != sj {
			return si < sj
		}
		return live[i].StartedAt < live[j].StartedAt
	})
	for _, s := range live {
		rows = append(rows, sessRow{s: s, live: true})
	}
	for _, sv := range m.ended {
		rows = append(rows, sessRow{s: sv.asSession(), live: false, saved: sv})
	}
	return rows
}

func (m Model) selectedRow() (sessRow, bool) {
	rows := m.rows()
	if m.sessCursor < 0 || m.sessCursor >= len(rows) {
		return sessRow{}, false
	}
	return rows[m.sessCursor], true
}

// selectedSession returns the selected session (live or synthesized-from-saved)
// so prompt/rename/activity paths work uniformly across both.
func (m Model) selectedSession() (Session, bool) {
	if r, ok := m.selectedRow(); ok {
		return r.s, true
	}
	return Session{}, false
}

// permItem is one row in the Permissions tab: either a waiting request, or a
// session that currently has auto-approve-safe turned on (so it can be turned
// off even when nothing is waiting for it).
type permItem struct {
	pending *PendingRequest
	autoID  string
}

// permItems is the Permissions list: pending requests first, then the sessions
// that are auto-approving safe actions.
func (m Model) permItems() []permItem {
	out := make([]permItem, 0, len(m.pending)+len(m.policy.AllSessions))
	for i := range m.pending {
		out = append(out, permItem{pending: &m.pending[i]})
	}
	for _, id := range m.autoSessionIDs() {
		out = append(out, permItem{autoID: id})
	}
	return out
}

// autoSessionIDs are the sessions with per-session auto-approve on, sorted for a
// stable order.
func (m Model) autoSessionIDs() []string {
	var ids []string
	for id, on := range m.policy.AllSessions {
		if on {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// sessionLabel is a friendly name for a session id — its nickname/project if we
// still know it, else a short id.
func (m Model) sessionLabel(id string) string {
	for _, s := range m.sessions {
		if s.SessionID == id {
			return m.cfg.DisplayName(s)
		}
	}
	for _, sv := range m.ended {
		if sv.SessionID == id {
			return sv.Name
		}
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (m Model) selectedPermItem() (permItem, bool) {
	items := m.permItems()
	if m.permCursor < 0 || m.permCursor >= len(items) {
		return permItem{}, false
	}
	return items[m.permCursor], true
}

func (m Model) selectedPending() (PendingRequest, bool) {
	if it, ok := m.selectedPermItem(); ok && it.pending != nil {
		return *it.pending, true
	}
	return PendingRequest{}, false
}

// skillsList is the installed skills followed by the Downloads ones, so a single
// cursor can walk both sections.
func (m Model) skillsList() []Skill {
	return append(append([]Skill{}, m.skillsInstalled...), m.skillsDownloads...)
}

func (m Model) selectedSkill() (Skill, bool) {
	list := m.skillsList()
	if m.skillCursor < 0 || m.skillCursor >= len(list) {
		return Skill{}, false
	}
	return list[m.skillCursor], true
}

func (m Model) agentsList() []Agent {
	return append(append([]Agent{}, m.agentsInstalled...), m.agentsDownloads...)
}

// liveAgent is one running/recent subagent, tagged with the session that spawned
// it so the Agents tab can show cross-session activity.
type liveAgent struct {
	SessionID string
	sub       Subagent
}

// agentItem is one row in the Agents tab: a live subagent, or an installed/
// downloadable agent definition. Live agents list first.
type agentItem struct {
	live *liveAgent
	def  *Agent
}

func (m Model) agentItems() []agentItem {
	out := make([]agentItem, 0, len(m.liveAgents)+len(m.agentsInstalled)+len(m.agentsDownloads))
	for i := range m.liveAgents {
		out = append(out, agentItem{live: &m.liveAgents[i]})
	}
	defs := m.agentsList()
	for i := range defs {
		out = append(out, agentItem{def: &defs[i]})
	}
	return out
}

func (m Model) selectedAgentItem() (agentItem, bool) {
	items := m.agentItems()
	if m.agentCursor < 0 || m.agentCursor >= len(items) {
		return agentItem{}, false
	}
	return items[m.agentCursor], true
}

// selectedAgent returns the selected agent *definition* (nil for a live-agent
// row), so install/remove keys only ever act on real files.
func (m Model) selectedAgent() (Agent, bool) {
	if it, ok := m.selectedAgentItem(); ok && it.def != nil {
		return *it.def, true
	}
	return Agent{}, false
}

func (m Model) idleThreshold() time.Duration { return idleThresholds[m.idleIdx] }

// liveSession finds a live session by id.
func (m Model) liveSession(id string) (Session, bool) {
	for _, s := range m.sessions {
		if s.SessionID == id {
			return s, true
		}
	}
	return Session{}, false
}

// deliverPrompt routes a prompt to the right mechanism for its target, and is
// the one place that decides who owns a session.
//
// A LIVE session belongs to the terminal that started it. We never spawn a
// second `claude --resume` against it — that would put two writers on one
// transcript and leave the user's own VS Code window showing a conversation
// that no longer matches disk. Instead the message goes to the outbox and the
// Stop hook running inside that session hands it over on its next turn. The
// reply appears where the user is actually looking.
//
// An ENDED session has no owning process, so there's nothing to diverge from
// and `--resume` is the correct — and only — way to continue it.
func (m *Model) deliverPrompt(r sessRow, text string) tea.Cmd {
	if !r.live {
		m.enqueue(r.s.SessionID, text)
		if m.sendConn != "" {
			m.setStatus("queued ("+itoa(m.queueLen(r.s.SessionID))+" waiting)", false)
			return nil
		}
		m2, cmd := m.startNextSend()
		*m = m2
		return cmd
	}

	// If the session lives in a tmux pane we can type into it directly, which
	// works whether it's busy or sitting idle at its prompt — the outbox can
	// only ever be drained at the end of a turn, so an idle session would
	// otherwise never receive this.
	if pane, ok := PaneForSession(r.s); ok {
		if err := TmuxSendLine(pane, text); err != nil {
			m.setStatus("tmux send failed: "+err.Error(), true)
			return nil
		}
		m.setStatus("typed into "+r.s.Project()+" ("+pane+")", false)
		return nil
	}

	// Without the Stop hook in this project's settings nothing will ever drain
	// the outbox, so say so rather than queue into a void.
	if !m.deliveryInstalledFor(r.s.Cwd) {
		m.setStatus("can't deliver: Omni's Stop hook isn't installed for "+r.s.Project()+" — press h to install", true)
		return nil
	}

	if _, err := enqueueOutbox(r.s.SessionID, text); err != nil {
		m.setStatus("could not queue: "+err.Error(), true)
		return nil
	}
	m.refreshOutboxQueue() // show the badge now, not on the next tick

	surface := m.surfaceOf(r.s)
	n := len(m.outboxQueue[r.s.SessionID])
	if r.s.IsBusy() {
		m.setStatus("queued for "+surface+" — delivers when this turn ends ("+itoa(n)+" waiting)", false)
	} else {
		// Honest about the one real limitation: Stop only fires at the end of a
		// turn, so an idle session picks this up when it next runs.
		m.setStatus("queued for "+surface+" — delivers on its next turn ("+itoa(n)+" waiting) · f to focus", false)
	}
	return nil
}

// deliveryInstalledFor reports whether the hooks are registered somewhere that
// covers this session — its own project settings, or the global file.
func (m Model) deliveryInstalledFor(cwd string) bool {
	return GlobalHookInstalled() || (cwd != "" && ProjectHookInstalled(cwd))
}

// refreshOutboxQueue re-reads what's queued for every live session. Called on
// the sessions tick and right after queueing, so the badge and the pending list
// update immediately rather than waiting for the next poll.
func (m *Model) refreshOutboxQueue() {
	q := make(map[string][]OutboxMsg, len(m.sessions))
	for _, s := range m.sessions {
		if msgs := loadOutbox(s.SessionID); len(msgs) > 0 {
			q[s.SessionID] = msgs
		}
	}
	m.outboxQueue = q
}

// enqueue adds a prompt to a session's queue.
func (m *Model) enqueue(sessionID, text string) {
	if m.queue == nil {
		m.queue = map[string][]string{}
	}
	m.queue[sessionID] = append(m.queue[sessionID], text)
}

// queueLen is how many prompts are waiting for a session.
func (m Model) queueLen(sessionID string) int { return len(m.queue[sessionID]) }

// resumableSession finds the session a queued `--resume` prompt targets. It
// looks at ended sessions too: those are exactly the ones this path serves now
// that live sessions are delivered through their owning terminal instead.
// Without the ended lookup a queued prompt would find no session and be dropped.
func (m Model) resumableSession(id string) (Session, bool) {
	if s, ok := m.liveSession(id); ok {
		return s, true
	}
	for _, sv := range m.ended {
		if sv.SessionID == id {
			return sv.asSession(), true
		}
	}
	return Session{}, false
}

// startNextSend pops the next queued prompt for a session and sends it, but only
// when nothing is already in flight — sends stay strictly serial so two headless
// `--resume` runs never write the same transcript at once.
func (m Model) startNextSend() (Model, tea.Cmd) {
	if m.sendConn != "" {
		return m, nil // a send is already running
	}
	// Prefer the selected session's queue, then any other with work waiting.
	order := []string{}
	if s, ok := m.selectedSession(); ok {
		order = append(order, s.SessionID)
	}
	for id := range m.queue {
		order = append(order, id)
	}
	for _, id := range order {
		q := m.queue[id]
		if len(q) == 0 {
			continue
		}
		s, ok := m.resumableSession(id)
		if !ok {
			delete(m.queue, id) // session gone entirely — drop its queue
			continue
		}
		next := q[0]
		m.queue[id] = q[1:]
		m.sendConn = id
		m.sending = true
		return m, sendPromptCmd(next, s)
	}
	return m, nil
}

// needsSpin is true when there's live motion worth animating: a prompt is being
// sent, the selected session is busy, or it has agents still running.
func (m Model) needsSpin() bool {
	if m.sending {
		return true
	}
	if r, ok := m.selectedRow(); ok && r.live {
		if r.s.IsBusy() {
			return true
		}
		if runningCount(m.activity.Agents) > 0 {
			return true
		}
	}
	return false
}

// --- layout geometry (shared by view and mouse hit-testing) ---

type layout struct {
	w, h     int
	bodyTop  int // first body row (0-indexed)
	bodyH    int // body rows
	sidebarW int
	mainW    int
}

func (m Model) layout() layout {
	w, h := m.width, m.height
	if w < 20 {
		w = 100
	}
	if h < 8 {
		h = 30
	}
	sidebarW := 36
	if w < 100 {
		sidebarW = w / 3
	}
	if sidebarW < 22 {
		sidebarW = 22
	}
	// Observe mode hides the sidebar for a full-width, read-only session view.
	if m.mode == modeObserve {
		sidebarW = 0
	}
	// rows: 0 tabbar · 1 divider · [bodyTop..] body · h-2 status · h-1 footer
	return layout{
		w: w, h: h,
		bodyTop:  2,
		bodyH:    max(1, h-4),
		sidebarW: sidebarW,
		mainW:    max(10, w-sidebarW-1),
	}
}

type tabSpan struct {
	t      tab
	lo, hi int
}

// tabLabel is the text inside a tab. The Permissions tab carries a live counter
// of pending approvals so you can see at a glance that something needs a
// decision — rendering (renderTabBar) and hit-testing (tabSpans) both derive
// their width from this one place so they never drift.
func (m Model) tabLabel(i int) string {
	name := tabNames[i]
	if tab(i) == tabPermissions {
		if n := len(m.pending); n > 0 {
			name += " ●" + itoa(n)
		}
	}
	return name
}

// tabSpans is where each tab sits on the tab-bar row, derived from tabLabel so
// mouse hit-testing and rendering stay in sync as the pending counter changes.
func (m Model) tabSpans() []tabSpan {
	x := 2 + lipgloss.Width("◆ OMNI") + 3
	var spans []tabSpan
	for i := range tabNames {
		w := lipgloss.Width(m.tabLabel(i)) + 2 // " label "
		spans = append(spans, tabSpan{tab(i), x, x + w})
		x += w + 1
	}
	return spans
}

// activeLen / activeCursor / setActiveCursor abstract the current tab's list so
// navigation and clicks share one path.
func (m Model) activeLen() int {
	switch m.tab {
	case tabSessions:
		return len(m.rows())
	case tabPermissions:
		return len(m.permItems())
	case tabSkills:
		return len(m.skillsList())
	case tabAgents:
		return len(m.agentItems())
	}
	return 0
}

func (m Model) activeCursor() int {
	switch m.tab {
	case tabSessions:
		return m.sessCursor
	case tabPermissions:
		return m.permCursor
	case tabSkills:
		return m.skillCursor
	case tabAgents:
		return m.agentCursor
	}
	return 0
}

func (m *Model) setActiveCursor(i int) {
	switch m.tab {
	case tabSessions:
		m.sessCursor = i
	case tabPermissions:
		m.permCursor = i
	case tabSkills:
		m.skillCursor = i
	case tabAgents:
		m.agentCursor = i
	}
}

func (m Model) surfacesStale() bool {
	if len(m.surfaces) != len(m.sessions) {
		return true
	}
	for _, s := range m.sessions {
		if _, ok := m.surfaces[s.SessionID]; !ok {
			return true
		}
	}
	return false
}

func (m Model) surfaceOf(s Session) string {
	if label, ok := m.surfaces[s.SessionID]; ok {
		return label
	}
	return s.Surface()
}

func (m *Model) setStatus(s string, isErr bool) {
	m.status = s
	m.statusErr = isErr
}

// quitCleanup restores every settings.json Omni touched and drops the heartbeat,
// so terminating the dashboard leaves the machine's session settings exactly as
// they were before Omni started routing permissions.
func (m *Model) quitCleanup() {
	var dirs []string
	for _, s := range m.sessions {
		dirs = append(dirs, s.Cwd)
	}
	RestoreAllOnQuit(dirs)
	removeHeartbeat()
}

// beginCompose opens the prompt bar, restoring whatever draft was left behind by
// a previous cancel so an interrupted message isn't lost. Returns the focus cmd.
func (m *Model) beginCompose() tea.Cmd {
	m.mode = modeCompose
	m.status = ""
	m.lastReply = ""
	if m.draft != "" {
		m.textarea.SetValue(m.draft)
		m.textarea.CursorEnd()
	} else {
		m.textarea.Reset()
	}
	return m.textarea.Focus()
}

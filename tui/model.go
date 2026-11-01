package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/daemon"
	"runlite/internal/logs"
	"runlite/internal/proc"
	"runlite/internal/service"
)

// Constants define the algorithm's parameter space - Chapter 2: Polynomial bounds
const (
	// Polling intervals bounded for responsiveness vs overhead trade-off
	pollInterval    = 700 * time.Millisecond // O(1) refresh, bounded by human perception threshold
	projectPoll     = 1500 * time.Millisecond
	logTailLines    = 500        // Fixed window size O(1) memory
	maxLogBytes     = 512 * 1024 // Hard cap on log buffer memory
	hardLineCap     = 4000       // Max lines to prevent unbounded growth
	inputFieldCount = 3          // Number of input fields in add form
)

// ProjectItem represents a project in the list with O(1) title/description access
type ProjectItem struct {
	api.ProjectView
	Index int
}

// Title implements list.Item interface - O(1) computation, cached representation
func (p ProjectItem) Title() string {
	return summarizeProject(p.ProjectView)
}

func (p ProjectItem) Description() string {
	return p.ID
}

func (p ProjectItem) FilterValue() string {
	return p.Name
}

// Model maintains application state using CQRS pattern from bubbletea
type Model struct {
	// View dimensions - updated on WindowSizeMsg
	width  int
	height int

	// List widget for project navigation - greedy selection strategy
	list         list.Model
	statusBar    string
	actionMsg    string
	client       *api.Client
	projMap      map[string]api.ProjectView // ID -> ProjectView for O(1) lookup
	selID        string
	followLog    bool
	spawn        func(string, *config.Paths) error
	disconnected bool
	// pendingRemove 非空表示正在等待用户确认移除该项目 ID（UI 内确认态）
	pendingRemove string

	// Log state - only touched on bubbletea's Update/View thread
	logLines  []logs.Line
	logCursor int
	logRunID  string

	// Input forms - structured data with validation
	inputs []InputField
	// Current focused field index - greedy focus cycling
	inputIndex  int
	addMode     bool
	helpVisible bool
	// rightTab: 0 日志 1 配置 2 高级
	rightTab int
	keys     keyMap

	// Tunables loaded from ~/.runlite/tui_config.json
	logPoll     time.Duration
	projPoll    time.Duration
	defaultPort int
}

// State-driven color palette
var (
	styleRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	styleStopped = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styleFailed  = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styleIdle    = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleTS      = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
)

// statusColorized renders a run summary tinted by lifecycle state.
func statusColorized(p api.ProjectView) string {
	if p.Run == nil {
		return styleIdle.Render("未启动")
	}
	s := runSummary(p.Run)
	switch {
	case p.Run.State.Active():
		return styleRunning.Render(s)
	case p.Run.Stopped:
		return styleStopped.Render(s)
	default:
		return styleFailed.Render(s)
	}
}

// InputField encapsulates label + text input with state management
type InputField struct {
	textInput textinput.Model
	Label     string
}

// newModel constructs the initial state - O(1) initialization
func NewModel() *Model {
	cfg := loadTUIConfig(config.DefaultHome())
	applyTheme(cfg.Theme)
	m := &Model{
		projMap:     make(map[string]api.ProjectView),
		followLog:   true,
		spawn:       daemon.Spawn,
		keys:        loadKeys(),
		logPoll:     clampMS(cfg.LogPollMS, 200*time.Millisecond, 10*time.Second, pollInterval),
		projPoll:    clampMS(cfg.ProjectPollMS, 500*time.Millisecond, time.Minute, projectPoll),
		defaultPort: cfg.DefaultPort,
	}
	m.initInputs()
	m.setupList()
	m.statusBar = helpHint()
	return m
}

// tuiConfig mirrors ~/.runlite/tui_config.json:
//
//	{ "log_poll_ms": 700, "project_poll_ms": 1500, "default_port": 8080,
//	  "theme": { "running": "42", "stopped": "214" } }
type tuiConfig struct {
	LogPollMS     int         `json:"log_poll_ms"`
	ProjectPollMS int         `json:"project_poll_ms"`
	DefaultPort   int         `json:"default_port"`
	Theme         themeConfig `json:"theme"`
}

// themeConfig overrides the status color palette (256-color or #rrggbb).
type themeConfig struct {
	Running string `json:"running"`
	Stopped string `json:"stopped"`
	Failed  string `json:"failed"`
	Idle    string `json:"idle"`
	TS      string `json:"ts"`
	Err     string `json:"err"`
}

// loadTUIConfig reads the config file; missing or invalid values fall back to
// the compiled-in defaults.
func loadTUIConfig(home string) tuiConfig {
	cfg := tuiConfig{
		LogPollMS:     int(pollInterval / time.Millisecond),
		ProjectPollMS: int(projectPoll / time.Millisecond),
	}
	if home == "" {
		return cfg
	}
	data, err := os.ReadFile(filepath.Join(home, "tui_config.json"))
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(data, &cfg)
	return cfg
}

func clampMS(ms int, min, max, def time.Duration) time.Duration {
	if ms <= 0 {
		return def
	}
	d := time.Duration(ms) * time.Millisecond
	return minmax(d, min, max)
}

func minmax(d, min, max time.Duration) time.Duration {
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}

// applyTheme rebuilds the package-level status styles from theme overrides.
func applyTheme(t themeConfig) {
	set := func(s *lipgloss.Style, c string) {
		if c != "" {
			*s = lipgloss.NewStyle().Foreground(lipgloss.Color(c))
		}
	}
	set(&styleRunning, t.Running)
	set(&styleStopped, t.Stopped)
	set(&styleFailed, t.Failed)
	set(&styleIdle, t.Idle)
	set(&styleTS, t.TS)
	set(&styleErr, t.Err)
}

// initInputs establishes the add-form state machine - Chapter 6: DP state definition
func (m *Model) initInputs() {
	m.inputs = make([]InputField, inputFieldCount)

	for i := 0; i < inputFieldCount; i++ {
		var ti textinput.Model
		switch i {
		case 0:
			ti = newTextInput("选择项目目录", 200, 40)
			ti.Focus() // Initial focus - greedy choice
		case 1:
			ti = newTextInput("留空则用目录名", 100, 40)
		case 2:
			ti = newTextInput("留空则自动检测", 200, 40)
		}
		m.inputs[i] = InputField{textInput: ti, Label: getFieldLabel(i)}
	}
	m.inputIndex = 0
}

// setupList configures the list widget with visual hierarchy - O(1) setup
func (m *Model) setupList() {
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	itemStyle := lipgloss.NewStyle().PaddingLeft(2)
	selectedStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("170")).PaddingLeft(2)

	del := list.NewDefaultDelegate()
	del.Styles.NormalTitle = itemStyle
	del.Styles.SelectedTitle = selectedStyle

	l := list.New([]list.Item{}, del, m.width/3, m.height*2)
	l.Title = "项目列表"
	l.SetFilteringEnabled(true)
	// list.New focuses the filter input by default; without this Blur the
	// filter-state guard in handleKeyMsg would swallow every action key.
	l.FilterInput.Blur()
	l.FilterInput.Reset()
	l.SetShowHelp(false)
	l.SetShowStatusBar(false)
	l.Styles.Title = titleStyle
	l.Styles.PaginationStyle = lipgloss.NewStyle().MarginLeft(1)
	l.Styles.HelpStyle = lipgloss.NewStyle().MarginLeft(3)

	m.list = l
}

// Init returns initialization commands - Chapter 5: Divide & conquer base case
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.fetchProjectsCmd(), m.tickProjects(), m.tickLogs())
}

// Tick messages drive polling through the event loop instead of goroutines,
// so every model mutation stays on bubbletea's single Update thread.
type projectsTickMsg struct{}
type logsTickMsg struct{}

func (m *Model) tickProjects() tea.Cmd {
	return tea.Tick(m.projPoll, func(time.Time) tea.Msg { return projectsTickMsg{} })
}

func (m *Model) tickLogs() tea.Cmd {
	return tea.Tick(m.logPoll, func(time.Time) tea.Msg { return logsTickMsg{} })
}

// resolveClient reuses a healthy client or dials the daemon; it never touches
// the model, so it is safe to call from inside a tea.Cmd closure.
//
// The liveness probe is an authenticated call (Projects), not the token-free
// /api/health: after a daemon restart the old client's token is stale, but
// health still answers — the client was then reused and every subsequent
// token-gated call 401'd, leaving the TUI stuck on "未连接" forever. A stale
// token fails Projects, so this path reloads daemon.json and picks up the new
// token.
func resolveClient(c *api.Client, spawn func(string, *config.Paths) error) (*api.Client, error) {
	if c != nil {
		if _, perr := c.Projects(); perr == nil {
			return c, nil
		}
	}
	return daemon.Client(runliteCLI(), true, spawn)
}

// fetchProjectsCmd reads state I/O off the Update thread and reports it back
// as a Msg; the client is carried in the Msg and assigned by handleMsg.
func (m *Model) fetchProjectsCmd() tea.Cmd {
	c := m.client
	spawn := m.spawn
	return func() tea.Msg {
		nc, err := resolveClient(c, spawn)
		if err != nil {
			return Msg{err: err}
		}
		projs, err := nc.Projects()
		return Msg{projs: projs, client: nc, err: err}
	}
}

// fetchLogsCmd tails the selected project's log since the cursor.
func (m *Model) fetchLogsCmd() tea.Cmd {
	c := m.client
	selID := m.selID
	since := 0
	if m.followLog {
		since = m.logCursor
	}
	if c == nil || selID == "" {
		return nil
	}
	return func() tea.Msg {
		resp, err := c.Logs(selID, logTailLines, since, "")
		if err != nil || resp == nil || len(resp.Lines) == 0 {
			return nil
		}
		return Msg{lines: resp.Lines}
	}
}

// View renders the UI - composition of independent components
func (m *Model) View() string {
	var b strings.Builder

	// Render header
	b.WriteString(m.headerView())
	b.WriteString("\n\n")

	// Conditional rendering based on state - early exit for performance
	if m.helpVisible {
		b.WriteString(helpView())
		return b.String()
	}
	if m.addMode {
		b.WriteString(m.addFormView())
		return b.String()
	}

	// Main layout: list + overview strip + tabbed panel
	b.WriteString(m.projectListView())
	b.WriteString(m.summaryView())
	b.WriteString("\n" + separator() + "\n")
	b.WriteString(m.tabBar() + "\n")
	b.WriteString(m.tabContent())
	b.WriteString("\n" + m.statusBarView())

	if m.pendingRemove != "" {
		confirmStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("202")).Bold(true)
		b.WriteString("\n" + confirmStyle.Render(fmt.Sprintf("确认移除 %q？(y 确认 / n 或 esc 取消)", m.pendingRemove)))
	}

	if m.actionMsg != "" {
		msgStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("202")).Bold(true)
		b.WriteString("\n" + msgStyle.Render("["+m.actionMsg+"]"))
		m.actionMsg = ""
	}

	return b.String()
}

// Update processes messages using pattern matching - Chapter 2: State transitions
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.handleKeyMsg(msg)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.list.SetWidth(max(1, m.width/3))
		m.list.SetHeight(max(1, m.height*2))
		return m, nil

	case Msg:
		return m.handleMsg(msg)

	case dirPickedMsg:
		if msg.err != nil {
			m.actionMsg = errorMsg(msg.err)
			return m, nil
		}
		if msg.path != "" && m.addMode {
			m.inputs[0].textInput.SetValue(msg.path)
		}
		return m, nil

	case projectsTickMsg:
		return m, tea.Batch(m.fetchProjectsCmd(), m.tickProjects())

	case logsTickMsg:
		return m, tea.Batch(m.fetchLogsCmd(), m.tickLogs())
	}
	return m, nil
}

// handleKeyMsg implements keyboard command dispatch - O(1) routing.
// Action keys come from the customizable keyMap; everything else (navigation,
// filtering) is forwarded to the list widget.
func (m *Model) handleKeyMsg(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Short-circuit for active modes
	if m.addMode {
		return m.handleAddMode(msg)
	}
	if m.helpVisible {
		if isEscape(msg) {
			m.helpVisible = false
		}
		return m, nil
	}
	if m.pendingRemove != "" {
		return m.handleConfirmRemove(msg)
	}

	kp := msg.String()

	// While typing a search filter, all keys belong to the list widget.
	// Use SettingFilter() (filterState == Filtering), not FilterInput.Focused():
	// bubbles keeps the input focused after esc, which would swallow keys.
	if m.list.SettingFilter() {
		return m.forwardToList(msg)
	}

	switch {
	case m.keys.is("quit", kp):
		return m, tea.Quit
	case m.keys.is("start", kp):
		return m, m.runProject()
	case m.keys.is("stop", kp):
		return m, m.stopProject()
	case m.keys.is("restart", kp):
		return m, m.restartProject()
	case m.keys.is("remove", kp):
		if _, ok := m.projMap[m.selID]; ok {
			m.pendingRemove = m.selID
		}
		return m, nil
	case m.keys.is("add", kp):
		return m, m.showAddForm()
	case m.keys.is("help", kp):
		m.helpVisible = true
		return m, nil
	case m.keys.is("cycleTabs", kp):
		m.rightTab = (m.rightTab + 1) % 3
		return m, nil
	case m.keys.is("batchStart", kp):
		return m, m.batchOp(true)
	case m.keys.is("batchStop", kp):
		return m, m.batchOp(false)
	}

	return m.forwardToList(msg)
}

// forwardToList routes keys to the list and keeps selID in sync with the
// cursor; switching projects resets the log window.
func (m *Model) forwardToList(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	if it, ok := m.list.SelectedItem().(ProjectItem); ok && it.ID != m.selID {
		m.selID = it.ID
		m.logLines = nil
		m.logCursor = 0
	}
	return m, cmd
}

// handleAddMode processes form input - Chapter 6: Finite state machine
func (m *Model) handleAddMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.addMode = false
		return m, nil
	case "enter":
		if m.validateForm() {
			return m, m.submitAddProject()
		}
		return m, m.cycleFocus()
	case "tab":
		return m, m.cycleFocus()
	}
	if m.keys.is("pickDir", msg.String()) && m.inputIndex == 0 {
		return m, pickDirCmd()
	}
	// Forward typing to the focused text input
	var cmd tea.Cmd
	m.inputs[m.inputIndex].textInput, cmd = m.inputs[m.inputIndex].textInput.Update(msg)
	return m, cmd
}

// handleMsg processes background messages - producer/consumer pattern
func (m *Model) handleMsg(msg Msg) (tea.Model, tea.Cmd) {
	if msg.client != nil {
		m.client = msg.client
	}
	if msg.addOK {
		m.addMode = false
	}
	if msg.note != "" {
		m.actionMsg = msg.note
	}
	if msg.err != nil && contains(msg.err.Error(), "unavailable") {
		m.disconnected = true
		m.statusBar = disconnectMsg()
		return m, nil
	}
	if msg.err != nil {
		m.actionMsg = errorMsg(msg.err)
		m.disconnected = true
		return m, tea.Batch(m.fetchProjectsCmd(), m.scrollToBottom())
	}

	if len(msg.projs) > 0 {
		items := make([]list.Item, len(msg.projs))
		m.projMap = make(map[string]api.ProjectView, len(msg.projs))
		for i, p := range msg.projs {
			m.projMap[p.ID] = p
			items[i] = ProjectItem{ProjectView: p, Index: i}
		}
		m.list.SetItems(items)

		// Greedy selection: preserve or default
		if len(items) > 0 {
			idx := -1
			for i, p := range msg.projs {
				if p.ID == m.selID {
					idx = i
					break
				}
			}
			if idx >= 0 {
				m.list.Select(idx)
			} else {
				m.list.Select(0)
				m.selID = items[0].(ProjectItem).ID
			}
		}

		m.statusBar = connectedStatus(len(msg.projs))
		m.disconnected = false
	}

	if msg.lines != nil && len(msg.lines) > 0 {
		m.updateLogs(msg.lines)
	}

	return m, nil
}

// projectListView renders the left panel - greedy display optimization
func (m *Model) projectListView() string {
	if len(m.projMap) == 0 {
		return "(按 a 添加新项目)\n"
	}
	return m.list.View()
}

// summaryView renders the compact overview strip above the tabbed panel.
func (m *Model) summaryView() string {
	p, ok := m.projMap[m.selID]
	if !ok {
		return "(还没有项目，按 a 添加)\n"
	}
	return fmt.Sprintf("%s  %s  %s\n目录: %s\n", p.Name, p.Kind, statusColorized(p), p.Dir)
}

var tabNames = []string{"日志", "配置", "高级"}

// tabBar renders the switchable right-panel tabs.
func (m *Model) tabBar() string {
	active := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	inactive := lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	parts := make([]string, len(tabNames))
	for i, n := range tabNames {
		if i == m.rightTab {
			parts[i] = active.Render("[" + n + "]")
		} else {
			parts[i] = inactive.Render(n)
		}
	}
	return strings.Join(parts, " │ ") + inactive.Render("  (t 切换)")
}

// tabContent renders the panel body for the active tab.
func (m *Model) tabContent() string {
	switch m.rightTab {
	case 1:
		return m.detailView()
	case 2:
		return m.advancedView()
	default:
		return m.logView()
	}
}

// advancedView dumps raw run fields and internal state for troubleshooting.
func (m *Model) advancedView() string {
	p, ok := m.projMap[m.selID]
	if !ok {
		return "(请选择一个项目)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ID: %s\n目录: %s\n类型: %s  当前配置: %s\n", p.ID, p.Dir, p.Kind, orDash(p.Active))
	if p.Run != nil {
		r := p.Run
		writeRunFields(&b, r)
		fmt.Fprintf(&b, "状态: %s  PID: %d  端口: %d  退出码: %d\n", r.State, r.PID, r.Port, r.ExitCode)
	} else {
		b.WriteString("运行记录: 无\n")
	}
	for _, n := range p.Notes {
		b.WriteString(styleErr.Render("! " + n + "\n"))
	}
	fmt.Fprintf(&b, "日志游标: %d  缓冲行数: %d  轮询: %v\n", m.logCursor, len(m.logLines), m.logPoll)
	return b.String()
}

// detailView computes and formats project details - O(n) for n configs
func (m *Model) detailView() string {
	if len(m.projMap) == 0 {
		return "(还没有项目)"
	}
	p, ok := m.projMap[m.selID]
	if !ok {
		return "(请选择一个项目)"
	}

	return formatDetails(p)
}

// logView renders the bottom-right panel with bounded scroll
func (m *Model) logView() string {
	if len(m.logLines) == 0 {
		return "(无日志数据)"
	}

	var b strings.Builder
	for _, ln := range m.logLines {
		ts := styleTS.Render(normalizeTimestamp(ln.TS))
		text := ln.Text
		if ln.Err {
			text = styleErr.Render("! " + text)
		}
		fmt.Fprintf(&b, "%s %s\n", ts, text)
	}
	return b.String()
}

// updateLogs implements circular buffer logic - Chapter 6: Sliding window DP
func (m *Model) updateLogs(newLines []logs.Line) {
	if len(newLines) > 0 {
		seq := newLines[len(newLines)-1].Seq
		if seq > m.logCursor {
			m.logCursor = seq
		}
	}

	// Append and trim to hard bounds
	m.logLines = append(m.logLines, newLines...)
	m.trimLogWindow()
}

// runProject executes start command - client and selection are captured here
// (on the Update thread); the closure only touches the captured values.
func (m *Model) runProject() tea.Cmd {
	c, p := m.getClientAndSelected()
	if c == nil {
		return func() tea.Msg { return Msg{err: errors.New("未连接到 daemon")} }
	}
	if p.ID != m.selID {
		return func() tea.Msg { return Msg{err: errors.New("请先选择一个项目")} }
	}
	return func() tea.Msg {
		req := service.StartRequest{Key: p.ID}
		run, err := c.Start(p.ID, req, false)
		var id string
		if run != nil {
			id = run.ID
		}
		return Msg{runID: id, err: err}
	}
}

// stopProject stops only the selected project; mirrors runProject's capture
// pattern (client and selection resolved on the Update thread).
func (m *Model) stopProject() tea.Cmd {
	c, p := m.getClientAndSelected()
	if c == nil {
		return func() tea.Msg { return Msg{err: errors.New("未连接到 daemon")} }
	}
	if p.ID != m.selID {
		return func() tea.Msg { return Msg{err: errors.New("请先选择一个项目")} }
	}
	return func() tea.Msg {
		if _, err := c.Stop(p.ID, false); err != nil {
			return Msg{err: err}
		}
		return Msg{note: fmt.Sprintf("已停止 %s", p.Name)}
	}
}

// restartProject implements stop+start sequence - atomic operation
func (m *Model) restartProject() tea.Cmd {
	c, p := m.getClientAndSelected()
	if c == nil || p.ID != m.selID {
		return func() tea.Msg { return Msg{err: m.selErr()} }
	}
	return func() tea.Msg {
		// Stop phase
		if _, err := c.Stop(p.ID, false); err != nil && !contains(err.Error(), "没有在运行") {
			return Msg{err: fmt.Errorf("停止失败：%w", err)}
		}

		// Start phase
		req := service.StartRequest{Key: p.ID}
		run, err := c.Start(p.ID, req, false)
		var id string
		if run != nil {
			id = run.ID
		}
		return Msg{runID: id, err: err}
	}
}

// handleConfirmRemove processes keys while a removal is pending confirmation.
// It replaces the old Printf/Scanln flow, which fought bubbletea's raw-mode
// console: the prompt never reached the user and Scanln never got stdin.
func (m *Model) handleConfirmRemove(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		id := m.pendingRemove
		m.pendingRemove = ""
		return m, m.removeProject(id)
	case "n", "N", "esc":
		m.pendingRemove = ""
	}
	return m, nil
}

// removeProject issues the daemon removal for the confirmed project ID.
// The client is captured here, on the Update thread.
func (m *Model) removeProject(id string) tea.Cmd {
	c := m.client
	if c == nil {
		return func() tea.Msg { return Msg{err: errors.New("未连接到 daemon")} }
	}
	return func() tea.Msg { return Msg{err: c.Remove(id)} }
}

// showAddForm initiates the add workflow - O(1) state transition
func (m *Model) showAddForm() tea.Cmd {
	m.initInputs()
	m.addMode = true
	m.inputIndex = 0
	return m.inputs[0].textInput.Focus()
}

// submitAddProject validates and submits - inputs and client are captured on
// the Update thread; addMode is cleared by handleMsg when the result arrives.
func (m *Model) submitAddProject() tea.Cmd {
	dir, name, cmd := extractInputs(m.inputs)
	c := m.client
	spawn := m.spawn
	port := m.defaultPort
	if dir == "" {
		return func() tea.Msg { return Msg{err: errors.New("目录必填")} }
	}
	return func() tea.Msg {
		nc, err := resolveClient(c, spawn)
		if err != nil {
			return Msg{err: err}
		}

		req := service.AddRequest{Dir: dir, Name: name, Cmd: cmd, Port: port}
		pv, err := nc.Add(req)
		if err != nil {
			return Msg{err: err}
		}

		return Msg{projs: []api.ProjectView{pv}, client: nc, addOK: true}
	}
}

// validateForm checks all fields - Chapter 4: Local predicates imply global correctness
func (m *Model) validateForm() bool {
	for i := range m.inputs {
		if m.inputs[i].textInput.Value() == "" && getFieldLabel(i) == "目录" {
			m.inputIndex = i
			m.inputs[i].textInput.Focus()
			return false
		}
	}
	return true
}

// getClientAndSelected extracts shared client and selected project - defensive copy
func (m *Model) getClientAndSelected() (*api.Client, api.ProjectView) {
	c := m.client
	p, _ := m.projMap[m.selID]
	return c, p
}

// selErr returns selection error message - constant-time string generation
func (m *Model) selErr() error {
	return errors.New("请先选择项目")
}

// Helper functions - pure functions for separation of concerns

func newTextInput(placeholder string, charLimit, width int) textinput.Model {
	ti := textinput.New()
	ti.Placeholder = placeholder
	ti.CharLimit = charLimit
	ti.Width = width
	return ti
}

func getFieldLabel(i int) string {
	labels := []string{"目录", "名字", "自定义命令"}
	if i >= 0 && i < len(labels) {
		return labels[i]
	}
	return "未知"
}

func isEscape(msg tea.KeyMsg) bool {
	return msg.String() == "esc"
}

// cycleFocus moves form focus to the next input field.
func (m *Model) cycleFocus() tea.Cmd {
	m.inputs[m.inputIndex].textInput.Blur()
	m.inputIndex = (m.inputIndex + 1) % len(m.inputs)
	return m.inputs[m.inputIndex].textInput.Focus()
}

// batchOp starts every inactive project (start=true) or stops every active
// one. The client and target list are snapshotted on the Update thread.
func (m *Model) batchOp(start bool) tea.Cmd {
	c := m.client
	if c == nil {
		return func() tea.Msg { return Msg{err: errors.New("未连接到 daemon")} }
	}
	targets := make([]string, 0, len(m.projMap))
	for id, p := range m.projMap {
		active := p.Run != nil && p.Run.State.Active()
		if start != active {
			targets = append(targets, id)
		}
	}
	if len(targets) == 0 {
		return func() tea.Msg { return Msg{note: "没有需要批量操作的项目"} }
	}
	sort.Strings(targets)
	return func() tea.Msg {
		var fails []string
		for _, id := range targets {
			var err error
			if start {
				_, err = c.Start(id, service.StartRequest{Key: id}, false)
			} else {
				_, err = c.Stop(id, false)
			}
			if err != nil && !contains(err.Error(), "没有在运行") {
				fails = append(fails, id+": "+err.Error())
			}
		}
		if len(fails) > 0 {
			return Msg{err: errors.New(strings.Join(fails, "; "))}
		}
		verb := "停止"
		if start {
			verb = "启动"
		}
		return Msg{note: fmt.Sprintf("已批量%s %d 个项目", verb, len(targets))}
	}
}

// dirPickedMsg carries the result of the native folder picker dialog.
type dirPickedMsg struct {
	path string
	err  error
}

// pickDirCmd opens the Windows folder browser off the Update thread.
// Cancelling the dialog yields an empty path and is treated as a no-op.
func pickDirCmd() tea.Cmd {
	return func() tea.Msg {
		script := "$f = (New-Object -ComObject Shell.Application).BrowseForFolder(0, '选择项目目录', 0, 0); if ($f) { $f.Self.Path }"
		out, err := exec.Command("powershell", "-NoProfile", "-Command", script).Output()
		if err != nil {
			return dirPickedMsg{err: err}
		}
		return dirPickedMsg{path: strings.TrimSpace(string(out))}
	}
}

// keyMap maps actions to keys; override any action via
// ~/.runlite/tui_keys.json, e.g. {"start": ["s"], "quit": ["ctrl+q"]}
type keyMap map[string][]string

func defaultKeys() keyMap {
	return keyMap{
		"quit":       {"q", "ctrl+c"},
		"start":      {" "},
		"stop":       {"s"},
		"restart":    {"r"},
		"remove":     {"delete", "backspace"},
		"add":        {"a"},
		"help":       {"?"},
		"cycleTabs":  {"t"},
		"batchStart": {"A"},
		"batchStop":  {"S"},
		"pickDir":    {"ctrl+d"},
	}
}

func loadKeys() keyMap {
	k := defaultKeys()
	home := config.DefaultHome()
	if home == "" {
		return k
	}
	data, err := os.ReadFile(filepath.Join(home, "tui_keys.json"))
	if err != nil {
		return k
	}
	var overrides map[string][]string
	if err := json.Unmarshal(data, &overrides); err != nil {
		return k
	}
	for action, keys := range overrides {
		if len(keys) > 0 {
			k[action] = keys
		}
	}
	return k
}

func (k keyMap) is(action, key string) bool {
	for _, x := range k[action] {
		if x == key {
			return true
		}
	}
	return false
}

func (m *Model) scrollToBottom() tea.Cmd {
	// Scroll log view to bottom
	return nil
}

func (m *Model) statusBarView() string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(m.statusBar)
}

// addFormView renders the add-project form - one labeled field per row
func (m *Model) addFormView() string {
	var b strings.Builder
	b.WriteString("添加新项目（tab 切换 | enter 提交 | esc 取消）\n\n")
	for i := range m.inputs {
		labelStyle := lipgloss.NewStyle()
		if i == m.inputIndex {
			labelStyle = labelStyle.Bold(true).Foreground(lipgloss.Color("205"))
		}
		fmt.Fprintf(&b, "%s: %s\n", labelStyle.Render(m.inputs[i].Label), m.inputs[i].textInput.View())
	}
	return b.String()
}

func (m *Model) headerView() string {
	status := styleRunning.Render("● 已连接")
	if m.disconnected {
		status = styleFailed.Render("● 未连接")
	}
	return lipgloss.NewStyle().Bold(true).Render(" Runlite TUI ") + "  " + status
}

func separator() string {
	return strings.Repeat("─", 40)
}

func helpHint() string {
	return "按 ? 查看帮助 | a 添加 | q 退出"
}

func disconnectMsg() string {
	return "错误：无法连接 daemon"
}

func connectedStatus(count int) string {
	return fmt.Sprintf("已连接：%d 个项目 | 每 1.5s 刷新", count)
}

func errorMsg(err error) string {
	return fmt.Sprintf("错误：%v", err)
}

func formatDetails(p api.ProjectView) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s\n", p.Name))
	b.WriteString(fmt.Sprintf("目录：%s\n", p.Dir))
	b.WriteString(fmt.Sprintf("类型：%s  当前配置：%s\n\n", p.Kind, orDash(p.Active)))

	for _, cfg := range p.Configs {
		mark := " "
		if cfg.ID == p.Active {
			mark = "*"
		}
		b.WriteString(fmt.Sprintf("  %s %-18s %s\n", mark, cfg.ID, cfg.Label))
	}

	for _, n := range p.Notes {
		b.WriteString(fmt.Sprintf("! %s\n", n))
	}

	if p.Run != nil {
		r := p.Run
		b.WriteString(fmt.Sprintf("\n最近一次运行：%s\n", runSummary(r)))
		writeRunFields(&b, r)
	} else {
		b.WriteString("\n最近一次运行：未启动\n")
	}

	return b.String()
}

func summarizeProject(p api.ProjectView) string {
	return p.Name + "  [" + p.Kind + "]  " + statusColorized(p)
}

func writeRunFields(b *strings.Builder, r *proc.Run) {
	if r.Cmd != "" {
		b.WriteString(fmt.Sprintf("命令：%s\n", r.Cmd))
	}
	if !r.StartedAt.IsZero() {
		b.WriteString(fmt.Sprintf("启动于 %s\n", r.StartedAt.Format("2006-01-02 15:04:05")))
	}
	if r.Reason != "" {
		b.WriteString(fmt.Sprintf("原因：%s\n", r.Reason))
	}
	if r.LogPath != "" {
		b.WriteString(fmt.Sprintf("日志：%s\n", r.LogPath))
	}
}

func extractInputs(inputs []InputField) (dir, name, cmd string) {
	for i := range inputs {
		val := strings.TrimSpace(inputs[i].textInput.Value())
		switch i {
		case 0:
			dir = val
		case 1:
			name = val
		case 2:
			cmd = val
		}
	}
	return
}

func normalizeTimestamp(ts string) string {
	if ts == "" {
		return "--:--:--"
	}
	return ts
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// Msg represents background message payload - CQRS pattern. Commands only
// produce Msgs; every model mutation happens in handleMsg on the Update thread.
type Msg struct {
	projs  []api.ProjectView
	err    error
	lines  []logs.Line
	runID  string
	client *api.Client // resolved client, assigned to the model by handleMsg
	addOK  bool        // an add-project submission succeeded; leave add mode
	note   string      // human-readable result of a successful action
}

func runliteCLI() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, name := range []string{"runlite.exe", "runlite"} {
			cand := filepath.Join(dir, name)
			if st, err := os.Stat(cand); err == nil && !st.IsDir() {
				return cand
			}
		}
	}
	// 回退到 daemon.json 里记录的 exe（上次注册/启动 daemon 用的真实路径）
	if home := config.DefaultHome(); home != "" {
		if info, err := config.ReadDaemonInfo(config.For(home).Daemon); err == nil && info != nil && info.Exe != "" {
			if st, err := os.Stat(info.Exe); err == nil && !st.IsDir() {
				return info.Exe
			}
		}
	}
	if p, err := exec.LookPath("runlite"); err == nil {
		return p
	}
	return "runlite"
}

func runSummary(r *proc.Run) string {
	if r == nil {
		return "未启动"
	}

	var b strings.Builder
	b.WriteString(string(r.State))
	if r.PID > 0 && r.State.Active() {
		fmt.Fprintf(&b, " pid %d", r.PID)
	}
	if r.Port > 0 && r.State.Active() {
		fmt.Fprintf(&b, " 端口 %d", r.Port)
	}
	if r.State.Active() {
		fmt.Fprintf(&b, " 已运行 %s", durCompact(r.Uptime()))
	} else if !r.EndedAt.IsZero() {
		endTime := r.EndedAt.Local().Format("15:04:05")
		uptime := durCompact(r.Uptime())
		fmt.Fprintf(&b, " 结束于 %s(跑了 %s)", endTime, uptime)
		if showExitCode(r) {
			fmt.Fprintf(&b, "，退出码 %d", r.ExitCode)
		}
	}
	return b.String()
}

func showExitCode(r *proc.Run) bool {
	return r != nil && r.ExitCode != 0 && !r.NoOp && !r.Stopped
}

func (m *Model) trimLogWindow() {
	// Memory-bounded pruning - Chapter 6: Sliding window optimization
	const maxBytes = maxLogBytes

	lines := m.logLines
	if len(lines) <= hardLineCap {
		return
	}

	// Linear scan for byte budget - O(n) but small constant
	prefix := make([]int, len(lines)+1)
	for i, l := range lines {
		prefix[i+1] = prefix[i] + len(l.Text) + len(l.TS)
	}
	total := prefix[len(lines)]
	if total <= maxBytes {
		return
	}

	// Find cutoff point
	k := 1
	for k < len(lines) && total-prefix[k] > maxBytes {
		k++
	}

	n := make([]logs.Line, len(lines)-k)
	copy(n, lines[k:])
	m.logLines = n
}

func durCompact(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	s := int(d.Seconds())
	h, m := s/3600, (s/60)%60
	s %= 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func helpView() string {
	return ` Runlite TUI 操作指南

快捷键:
  ↑ / ↓      - 导航项目列表
  /          - 搜索过滤项目（esc 退出过滤）
  空格        - 启动选中的项目
  s          - 停止选中的项目
  r          - 重启选中的项目
  del        - 移除选中的项目（y 确认 / n 取消）
  a          - 添加新项目（表单内 ctrl+d 打开目录选择对话框）
  t          - 切换右侧标签页（日志/配置/高级）
  A / S      - 批量启动全部 / 批量停止全部
  tab        - 表单内切换输入框焦点
  esc        - 退出当前表单/模式
  q          - 退出程序
  ?          - 显示此帮助

按键可在 ~/.runlite/tui_keys.json 中自定义，例如 {"stop": ["x"]}
提示：右侧实时显示项目详情与日志
`
}

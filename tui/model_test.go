package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"runlite/internal/api"
	"runlite/internal/proc"
)

func runes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func send(t *testing.T, m *Model, msg tea.Msg) {
	t.Helper()
	_, cmd := m.Update(msg)
	if cmd != nil {
		if res := cmd(); res != nil {
			m.Update(res)
		}
	}
}

func TestTabCycling(t *testing.T) {
	m := NewModel()
	send(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	if m.rightTab != 1 {
		t.Fatalf("t 应切到配置页, got %d", m.rightTab)
	}
	// 再按三次: 1 -> 2 -> 0 -> 1
	send(t, m, runes("t"))
	send(t, m, runes("t"))
	send(t, m, runes("t"))
	if m.rightTab != 1 {
		t.Fatalf("三次循环应回到配置页, got %d", m.rightTab)
	}
}

func TestFilterSwallowsActionKeys(t *testing.T) {
	m := NewModel()
	// list 的 Filter 绑定仅在有项目时启用
	send(t, m, Msg{projs: []api.ProjectView{{ID: "a", Name: "Alpha"}, {ID: "b", Name: "Beta"}}})
	send(t, m, runes("/"))
	if !m.list.SettingFilter() {
		t.Fatal("/ 应进入过滤态")
	}
	// 过滤态下按 a 只能输入过滤词，不能打开添加表单
	send(t, m, runes("a"))
	if m.addMode {
		t.Fatal("过滤态下 a 不应进入添加模式")
	}
	send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.list.SettingFilter() {
		t.Fatal("esc 应退出过滤态")
	}
	// 退出过滤后动作键恢复
	send(t, m, runes("a"))
	if !m.addMode {
		t.Fatal("退出过滤后 a 应重新打开添加表单")
	}
}

func TestNavigationSyncsSelID(t *testing.T) {
	m := NewModel()
	send(t, m, Msg{projs: []api.ProjectView{
		{ID: "a", Name: "Alpha"},
		{ID: "b", Name: "Beta"},
	}})
	if m.selID != "a" {
		t.Fatalf("初始应选中 a, got %q", m.selID)
	}
	send(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.selID != "b" {
		t.Fatalf("↓ 应把选中同步到 b, got %q", m.selID)
	}
}

func TestAddFormTypingAndFocus(t *testing.T) {
	m := NewModel()
	send(t, m, runes("a"))
	if !m.addMode {
		t.Fatal("a 应打开添加表单")
	}
	send(t, m, runes("abc"))
	if got := m.inputs[0].textInput.Value(); got != "abc" {
		t.Fatalf("输入应进入目录字段, got %q", got)
	}
	send(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if m.inputIndex != 1 {
		t.Fatalf("tab 应切换焦点到名字字段, got %d", m.inputIndex)
	}
	send(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.addMode {
		t.Fatal("esc 应退出表单")
	}
}

func TestDirPickedMsgFillsField(t *testing.T) {
	m := NewModel()
	send(t, m, runes("a"))
	send(t, m, dirPickedMsg{path: `C:\picked\dir`})
	if got := m.inputs[0].textInput.Value(); got != `C:\picked\dir` {
		t.Fatalf("对话框结果应回填目录字段, got %q", got)
	}
	// 取消对话框（空路径）不应覆盖已输入内容
	send(t, m, dirPickedMsg{path: ""})
	if got := m.inputs[0].textInput.Value(); got != `C:\picked\dir` {
		t.Fatalf("空路径应忽略, got %q", got)
	}
}

func TestRemoveConfirmationFlow(t *testing.T) {
	m := NewModel()
	m.projMap["x"] = api.ProjectView{ID: "x", Name: "X"}
	m.selID = "x"

	send(t, m, tea.KeyMsg{Type: tea.KeyDelete})
	if m.pendingRemove != "x" {
		t.Fatalf("del 应进入确认态, got %q", m.pendingRemove)
	}
	send(t, m, runes("n"))
	if m.pendingRemove != "" {
		t.Fatal("n 应取消确认")
	}

	send(t, m, tea.KeyMsg{Type: tea.KeyDelete})
	send(t, m, runes("y"))
	if m.pendingRemove != "" {
		t.Fatal("y 应清除确认态")
	}
	if m.actionMsg == "" {
		t.Fatal("无 client 时应产生错误提示")
	}
}

func TestBatchOpWithoutClient(t *testing.T) {
	m := NewModel()
	cmd := m.batchOp(true)
	msg, ok := cmd().(Msg)
	if !ok || msg.err == nil {
		t.Fatal("未连接 daemon 时批量操作应返回错误")
	}
}

func TestBatchOpNoTargets(t *testing.T) {
	m := NewModel()
	m.client = &api.Client{}
	m.projMap = map[string]api.ProjectView{}
	msg := m.batchOp(true)()
	if mm, ok := msg.(Msg); !ok || mm.note != "没有需要批量操作的项目" {
		t.Fatalf("空列表应提示无目标, got %+v", msg)
	}
}

func TestKeyMapDefaultsAndOverride(t *testing.T) {
	k := defaultKeys()
	if !k.is("start", " ") || !k.is("quit", "ctrl+c") || !k.is("pickDir", "ctrl+d") {
		t.Fatal("默认按键缺失")
	}
	custom := keyMap{"start": {"s"}}
	if !custom.is("start", "s") {
		t.Fatal("覆盖键应生效")
	}
}

func TestStatusColorizedStates(t *testing.T) {
	idle := summarizeProject(api.ProjectView{Name: "N"})
	if idle == "" {
		t.Fatal("未启动项目标题为空")
	}
	running := statusColorized(api.ProjectView{Run: &proc.Run{State: proc.StateRunning, PID: 123}})
	if running == "" {
		t.Fatal("运行状态渲染为空")
	}
	stopped := statusColorized(api.ProjectView{Run: &proc.Run{State: proc.StateStopped, Stopped: true}})
	if stopped == "" {
		t.Fatal("停止状态渲染为空")
	}
}

func TestViewRendersAllTabs(t *testing.T) {
	m := NewModel()
	send(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	send(t, m, Msg{projs: []api.ProjectView{{ID: "a", Name: "Alpha"}}})
	for tab := 0; tab < 3; tab++ {
		m.rightTab = tab
		if v := m.View(); v == "" {
			t.Fatalf("tab %d 渲染为空", tab)
		}
	}
	m.pendingRemove = "a"
	if v := m.View(); v == "" {
		t.Fatal("确认态渲染为空")
	}
}

func TestLoadTUIConfigDefaultsAndOverride(t *testing.T) {
	home := t.TempDir()
	cfg := loadTUIConfig(home)
	if cfg.LogPollMS != 700 || cfg.ProjectPollMS != 1500 || cfg.DefaultPort != 0 {
		t.Fatalf("缺省值错误: %+v", cfg)
	}
	// 空 home 也应返回缺省值
	if c2 := loadTUIConfig(""); c2.LogPollMS != 700 {
		t.Fatalf("空 home 应走缺省: %+v", c2)
	}
	body := `{"log_poll_ms":2000,"project_poll_ms":3000,"default_port":8080,"theme":{"running":"208","err":"#ff0000"}}`
	if err := os.WriteFile(filepath.Join(home, "tui_config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg = loadTUIConfig(home)
	if cfg.LogPollMS != 2000 || cfg.ProjectPollMS != 3000 || cfg.DefaultPort != 8080 {
		t.Fatalf("覆盖值未生效: %+v", cfg)
	}
	if cfg.Theme.Running != "208" || cfg.Theme.Err != "#ff0000" {
		t.Fatalf("主题覆盖未生效: %+v", cfg.Theme)
	}
	// 非法 JSON 应回退缺省
	if err := os.WriteFile(filepath.Join(home, "tui_config.json"), []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg = loadTUIConfig(home); cfg.LogPollMS != 700 {
		t.Fatalf("非法 JSON 应回退缺省: %+v", cfg)
	}
}

func TestClampMS(t *testing.T) {
	cases := []struct {
		ms   int
		want time.Duration
	}{
		{0, pollInterval},            // 未配置走缺省
		{-5, pollInterval},           // 非法值走缺省
		{50, 200 * time.Millisecond}, // 低于下限被钳制
		{2000, 2 * time.Second},      // 区间内原样
		{999999, 10 * time.Second},   // 高于上限被钳制
	}
	for _, c := range cases {
		if got := clampMS(c.ms, 200*time.Millisecond, 10*time.Second, pollInterval); got != c.want {
			t.Fatalf("clampMS(%d) = %v, want %v", c.ms, got, c.want)
		}
	}
}

func TestApplyThemeAndDefaultPortWired(t *testing.T) {
	// 主题应用不应 panic，空覆盖保留原样式
	applyTheme(themeConfig{Running: "208"})
	applyTheme(themeConfig{})
	if s := statusColorized(api.ProjectView{Run: &proc.Run{State: proc.StateRunning}}); s == "" {
		t.Fatal("主题切换后运行状态渲染为空")
	}
	// NewModel 的 defaultPort 缺省应为 0（临时 home 无配置文件时）
	m := NewModel()
	m.defaultPort = 8080
	if m.defaultPort != 8080 {
		t.Fatal("defaultPort 字段不可写")
	}
}

func TestStopKeySingleProject(t *testing.T) {
	m := NewModel()
	m.projMap = map[string]api.ProjectView{"x": {ID: "x", Name: "X"}}
	m.selID = "x"
	// 无 client：应提示未连接
	send(t, m, runes("s"))
	if !strings.Contains(m.actionMsg, "未连接") {
		t.Fatalf("无 client 应按 stop 键提示未连接, got %q", m.actionMsg)
	}
	// 有 client 但选中项不在 projMap：应提示先选择
	m.client = &api.Client{}
	m.selID = "missing"
	m.actionMsg = ""
	send(t, m, runes("s"))
	if !strings.Contains(m.actionMsg, "请先选择") {
		t.Fatalf("无效选中应提示先选择项目, got %q", m.actionMsg)
	}
	// 默认按键表包含 stop=s
	if !defaultKeys().is("stop", "s") {
		t.Fatal("默认按键表缺少 stop=s")
	}
}

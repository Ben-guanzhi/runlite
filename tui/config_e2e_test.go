//go:build e2e

// One-off verification of tui_config.json features against the real daemon
// and the real ~/.runlite home. Run with:
//
//	go test -tags e2e -run TestConfigE2E -v
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/daemon"
	"runlite/internal/proc"
)

func activeRun() *proc.Run { return &proc.Run{State: proc.StateRunning, PID: 1} }

// TestConfigE2ETheme: applyTheme must actually change ANSI output.
func TestConfigE2ETheme(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(termenv.Ascii)

	before := statusColorized(api.ProjectView{Run: activeRun()})
	applyTheme(themeConfig{Running: "208"})
	after := statusColorized(api.ProjectView{Run: activeRun()})
	if before == after {
		t.Fatalf("主题覆盖未改变渲染: %q", before)
	}
	if !strings.Contains(after, "38;5;208") {
		t.Fatalf("期望 256 色 208, got %q", after)
	}
	// 还原默认，避免影响同包其它测试
	applyTheme(themeConfig{Running: "42"})
}

// TestConfigE2ENewModelReadsHome writes a real tui_config.json into
// ~/.runlite, constructs the model through the production path, then removes
// the file.
func TestConfigE2ENewModelReadsHome(t *testing.T) {
	home := config.DefaultHome()
	if home == "" {
		t.Skip("no home")
	}
	path := filepath.Join(home, "tui_config.json")
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("用户已有 %s，跳过以免覆盖；请手动验证", path)
	}
	body := `{"log_poll_ms":2000,"project_poll_ms":5000,"default_port":18080,"theme":{"running":"208"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	m := NewModel()
	if m.logPoll != 2*time.Second || m.projPoll != 5*time.Second || m.defaultPort != 18080 {
		t.Fatalf("NewModel 未读取配置: logPoll=%v projPoll=%v port=%d", m.logPoll, m.projPoll, m.defaultPort)
	}
	lipgloss.SetColorProfile(termenv.ANSI256)
	defer lipgloss.SetColorProfile(termenv.Ascii)
	if s := statusColorized(api.ProjectView{Run: activeRun()}); !strings.Contains(s, "38;5;208") {
		t.Fatalf("配置主题色未生效: %q", s)
	}
	applyTheme(themeConfig{Running: "42"})
}

// TestConfigE2EDefaultPortToDaemon drives submitAddProject with
// default_port=18080 against the real daemon and checks the stored config.
func TestConfigE2EDefaultPortToDaemon(t *testing.T) {
	exe := `d:\tools\db\runlite\dist\runlite-0.1.0-windows-amd64\runlite.exe`
	c, err := daemon.Client(exe, true, daemon.Spawn)
	if err != nil {
		t.Fatalf("daemon: %v", err)
	}
	dir := t.TempDir()
	// 同目录 ID 可能残留，先尽力清掉
	if projs, err := c.Projects(); err == nil {
		for _, p := range projs {
			if p.Dir == filepath.ToSlash(dir) || p.Name == "cfg-e2e-port" {
				c.Stop(p.ID, false)
				_ = c.Remove(p.ID)
			}
		}
	}
	t.Cleanup(func() {
		projs, err := c.Projects()
		if err != nil {
			return
		}
		for _, p := range projs {
			if p.Name == "cfg-e2e-port" {
				c.Stop(p.ID, false)
				_ = c.Remove(p.ID)
			}
		}
	})

	m := NewModel()
	m.client = c
	m.defaultPort = 18080
	m.inputs[0].textInput.SetValue(dir)
	m.inputs[1].textInput.SetValue("cfg-e2e-port")
	m.inputs[2].textInput.SetValue("ping -n 600 127.0.0.1")

	msg := m.submitAddProject()()
	mm, ok := msg.(Msg)
	if !ok || mm.err != nil || !mm.addOK {
		t.Fatalf("submit 失败: %+v", msg)
	}
	pv := mm.projs[0]
	if len(pv.Configs) == 0 || pv.Configs[0].Port != 18080 {
		t.Fatalf("默认端口未传给 daemon: %+v", pv.Configs)
	}
	// 走一遍 Update 线程确认 addOK 会退出表单态
	m.addMode = true
	_, _ = m.Update(mm)
	if m.addMode {
		t.Fatal("addOK 后应退出表单模式")
	}
}

//go:build e2e

// End-to-end batch operation verification against the real daemon.
// Run with: go test -tags e2e -run TestBatchE2E -v
//
// Safety: the daemon hosts user projects (lgsy-app / lgsy-web) that must NOT
// be stopped. Batch-start runs with the full projMap (their active state
// excludes them from targets); batch-stop is scoped to the two temp projects
// only, and the test asserts the user projects stayed running throughout.
package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"runlite/internal/api"
	"runlite/internal/daemon"
	"runlite/internal/service"
)

func e2eKey(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func e2eProjects(t *testing.T, c *api.Client) []api.ProjectView {
	t.Helper()
	projs, err := c.Projects()
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	return projs
}

func e2eActive(p api.ProjectView) bool {
	return p.Run != nil && p.Run.State.Active()
}

// e2eWait polls until every id matches wantActive.
func e2eWait(t *testing.T, c *api.Client, ids []string, wantActive bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ok := true
		for _, p := range e2eProjects(t, c) {
			for _, id := range ids {
				if p.ID == id && e2eActive(p) != wantActive {
					ok = false
				}
			}
		}
		if ok {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timeout waiting active=%v for %v", wantActive, ids)
}

// e2eAssertUserProjectsRunning fails if lgsy-app / lgsy-web got touched.
func e2eAssertUserProjectsRunning(t *testing.T, c *api.Client, stage string) {
	t.Helper()
	seen := 0
	for _, p := range e2eProjects(t, c) {
		if p.Name == "lgsy-app" || p.Name == "lgsy-web" {
			seen++
			if !e2eActive(p) {
				t.Fatalf("user project %s not running after %s", p.Name, stage)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("expected 2 user projects, saw %d", seen)
	}
}

func TestBatchE2E(t *testing.T) {
	exe := `d:\tools\db\runlite\dist\runlite-0.1.0-windows-amd64\runlite.exe`
	c, err := daemon.Client(exe, true, daemon.Spawn)
	if err != nil {
		t.Fatalf("daemon client: %v", err)
	}

	// project IDs derive from the directory, so each temp project needs its own.
	var ids []string
	dirs := []string{t.TempDir(), t.TempDir()}
	t.Cleanup(func() {
		for _, id := range ids {
			c.Stop(id, false)
			_ = c.Remove(id)
		}
	})
	for i, name := range []string{"tui-e2e-a", "tui-e2e-b"} {
		pv, err := c.Add(service.AddRequest{
			Dir:  dirs[i],
			Name: name,
			Cmd:  "ping -n 600 127.0.0.1",
		})
		if err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
		ids = append(ids, pv.ID)
	}

	projs := e2eProjects(t, c)
	m := NewModel()
	m.client = c
	m.projMap = make(map[string]api.ProjectView, len(projs))
	for _, p := range projs {
		m.projMap[p.ID] = p
	}

	// --- batch start (A): only the 2 inactive temp projects are targets ---
	_, cmd := m.Update(e2eKey("A"))
	if cmd == nil {
		t.Fatal("A produced no cmd")
	}
	m.Update(cmd())
	if m.actionMsg != "已批量启动 2 个项目" {
		t.Fatalf("start note = %q", m.actionMsg)
	}
	e2eWait(t, c, ids, true)
	e2eAssertUserProjectsRunning(t, c, "batch start")

	// --- batch stop (S): scoped to temp projects so user ones are spared ---
	m.projMap = map[string]api.ProjectView{}
	for _, id := range ids {
		pv, err := c.Project(id)
		if err != nil {
			t.Fatalf("project %s: %v", id, err)
		}
		m.projMap[pv.ID] = pv
	}
	_, cmd = m.Update(e2eKey("S"))
	if cmd == nil {
		t.Fatal("S produced no cmd")
	}
	m.Update(cmd())
	if m.actionMsg != "已批量停止 2 个项目" {
		t.Fatalf("stop note = %q", m.actionMsg)
	}
	e2eWait(t, c, ids, false)
	e2eAssertUserProjectsRunning(t, c, "batch stop")
}

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/proc"
	"runlite/internal/service"
)

// freePort 先占一个临时端口再放掉，拿来给 python 起静态站。
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取临时端口失败: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// startInProcessDaemon 在临时主目录里起一个真的 API 面并写好 daemon.json。
// 用真 server 而不是替身：这轮要验的正是"界面怎么连、连上以后读什么"，
// 替身会把 discovery（读 daemon.json + health）这段最值钱的路径绕过去。
func startInProcessDaemon(t *testing.T, home string) *config.DaemonInfo {
	t.Helper()
	svc, err := service.New(config.For(home))
	if err != nil {
		t.Fatalf("service.New: %v", err)
	}
	token := api.NewToken()
	srv := api.New(svc, token)
	port, err := srv.Listen(0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	info := &config.DaemonInfo{
		PID: os.Getpid(), Port: port, Token: token, Exe: svc.Executable(),
		Started: time.Now().Format(time.RFC3339),
	}
	if err := config.WriteDaemonInfo(config.For(home).Daemon, info); err != nil {
		t.Fatalf("写 daemon.json: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		svc.Close() // 兜底结束夹具进程，别把 python 留在机器上
	})
	return info
}

func (g *gui) waitIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !g.busy() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("%v 内动作没有结束（界面一直卡在“进行中”）", timeout)
}

// waitRunState 直接问 daemon，不等界面轮询——界面的 1.5s 节奏不该决定测试的时长。
func waitRunState(t *testing.T, c *api.Client, key string, want func(*proc.Run) bool, desc string) *proc.Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		pv, err := c.Project(key)
		if err != nil {
			t.Fatalf("查 %s: %v", key, err)
		}
		if pv.Run != nil && want(pv.Run) {
			return pv.Run
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("60s 内 %s 没有变成%s", key, desc)
	return nil
}

// TestGUIStartStopThroughWidgets 把界面从头过一遍：连 daemon → 列表出一行 →
// 点"启动" → 界面看见 running 和端口 → 日志区有内容 → 点"停止" → 变 stopped
// 且端口不再出现在那一行上。全程走真 widget 回调 + 真 daemon，不用替身。
func TestGUIStartStopThroughWidgets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RUNLITE_HOME", home)
	info := startInProcessDaemon(t, home)
	c := api.NewClient(info)

	httpPort := freePort(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>runlite gui fixture</h1>"), 0o644); err != nil {
		t.Fatalf("写夹具: %v", err)
	}
	// -u 是必须的：python 重定向到管道时按块缓冲，"Serving HTTP" 那行要等进程退出才吐出来，
	// 日志区就会一直是空的（本机实测：不加 -u 时 CLI 和 GUI 都只能看到 runlite 自己打的头三行）。
	added, err := c.Add(service.AddRequest{
		Dir: dir, Name: "gui-fixture", Port: httpPort,
		Cmd: fmt.Sprintf("python -u -m http.server %d", httpPort),
	})
	if err != nil {
		t.Fatalf("add 夹具: %v", err)
	}
	// 收尾顺序靠 LIFO：这条先跑，确保夹具进程被结束，之后 daemon 的 cleanup 才关句柄。
	t.Cleanup(func() { _, _ = c.Stop(added.ID, true) })

	a := test.NewApp()
	defer a.Quit()
	g := &gui{}
	g.app = a
	g.win = a.NewWindow("gui-test")
	g.build()
	g.win.Show()
	// 给窗口一个真实尺寸：TextGrid 只有被布局过才有滚动条，"跟随日志"那条路径
	// 要在有尺寸的状态下走，才和真窗口一致。
	g.win.Resize(fyne.NewSize(900, 600))

	// ① 连接与列表。
	g.refreshProjects()
	if n := g.list.Length(); n != 1 {
		t.Fatalf("列表应有 1 行，实得 %d（status=%q）", n, g.status.Text)
	}
	if !strings.Contains(g.status.Text, "daemon 在线") {
		t.Fatalf("状态条没报告连上，实得 %q", g.status.Text)
	}
	if !strings.Contains(g.detail.Text, "gui-fixture") {
		t.Fatalf("选中后详情没填上，实得 %q", g.detail.Text)
	}
	if row := rowText(added); !strings.Contains(row, "未启动") {
		t.Fatalf("新加的项目该是未启动，实得 %q", row)
	}

	// ② 点"启动"：回调走 goroutine，等它收工。
	test.Tap(g.startBtn)
	g.waitIdle(t, 90*time.Second)
	run := waitRunState(t, c, added.ID, func(r *proc.Run) bool {
		return r.State == proc.StateRunning && r.Port > 0
	}, "running 且认领到端口")
	if run.Port != httpPort {
		t.Fatalf("认领的端口 %d 不是夹具的 %d", run.Port, httpPort)
	}
	g.refreshProjects()
	if len(g.proj) != 1 || g.proj[0].Run == nil {
		t.Fatalf("刷新后界面没拿到运行态")
	}
	if got := rowText(g.proj[0]); !strings.Contains(got, "running") || !strings.Contains(got, fmt.Sprintf("端口 %d", httpPort)) {
		t.Fatalf("列表行没写出 running 与端口，实得 %q", got)
	}
	if !strings.Contains(g.detail.Text, "端口 "+fmt.Sprint(httpPort)) {
		t.Fatalf("详情里缺端口，实得 %q", g.detail.Text)
	}

	// ③ 日志：python 的 "Serving HTTP" 走 stderr，必须带 ! 前缀出现在日志区。
	// ③ 日志：python 的启动横幅走 stdout，每个请求的访问日志走 stderr。
	// 打一次真的 GET，才能端到端证明"stderr 行带 ! 前缀"这条口径（Spring Boot 的异常栈就在 stderr）。
	if resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", httpPort)); err != nil {
		t.Fatalf("访问夹具失败: %v", err)
	} else {
		resp.Body.Close()
	}
	g.refreshLogs(true)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(g.grid.Text(), "GET /") {
		time.Sleep(200 * time.Millisecond)
		g.refreshLogs(false)
	}
	if g.grid.Text() == "" {
		t.Fatal("点『启动』之后日志区一直是空的")
	}
	// 两条都要成立：stdout 的行前面没有 ! ，stderr 的行必须有 —— 这是"异常栈看得出来"的前提。
	var gotOut, gotErr bool
	for _, line := range strings.Split(g.grid.Text(), "\n") {
		marked := strings.Contains(line, "! ")
		switch {
		case strings.Contains(line, "Serving HTTP"):
			gotOut = true
			if marked {
				t.Fatalf("stdout 横幅被错标成 stderr: %q", line)
			}
		case strings.Contains(line, "GET /"):
			gotErr = marked
			if !marked {
				t.Fatalf("stderr 访问日志没打 ! 前缀: %q", line)
			}
		}
	}
	if !gotOut {
		t.Fatalf("日志区没看到夹具的 stdout 横幅，实得:\n%s", g.grid.Text())
	}
	if !gotErr {
		t.Fatalf("日志区没看到那次 GET 的 stderr 访问日志，实得:\n%s", g.grid.Text())
	}

	// ④ 点"停止"。
	test.Tap(g.stopBtn)
	g.waitIdle(t, 60*time.Second)
	stopped := waitRunState(t, c, added.ID, func(r *proc.Run) bool {
		return !r.State.Active() && !r.EndedAt.IsZero()
	}, "stopped")
	if !stopped.Stopped {
		t.Fatalf("界面点出来的这次结束该记成用户主动停止，实得 %+v", stopped)
	}
	g.refreshProjects()
	if got := rowText(g.proj[0]); strings.Contains(got, "端口 ") || strings.Contains(got, "pid ") {
		t.Fatalf("stopped 的行还带着端口/pid，会把历史读数说成现状：%q", got)
	}

	// ⑤ 重复点"停止"必须是空操作，不能报错也不能改账。
	test.Tap(g.stopBtn)
	g.waitIdle(t, 60*time.Second)
	if !strings.Contains(g.action.Text, "空操作") {
		t.Fatalf("重复停止该报告是空操作，实得 %q（连接行=%q）", g.action.Text, g.status.Text)
	}
}

// walkCanvasObjects 在测试里遍历弹窗内容。Dialog 的 OverlayContainer 是内部类型，
// 子对象不公开，只能借渲染器钻一层；其余容器直接读字段，别依赖渲染器的布局时机。
func walkCanvasObjects(o fyne.CanvasObject, visit func(fyne.CanvasObject)) {
	if o == nil {
		return
	}
	visit(o)
	switch v := o.(type) {
	case *widget.PopUp:
		walkCanvasObjects(v.Content, visit)
		return
	case *fyne.Container:
		for _, child := range v.Objects {
			walkCanvasObjects(child, visit)
		}
		return
	case *widget.Form:
		for _, item := range v.Items {
			walkCanvasObjects(item.Widget, visit)
		}
		return
	}
	if w, ok := o.(fyne.Widget); ok {
		if r := test.WidgetRenderer(w); r != nil {
			for _, child := range r.Objects() {
				walkCanvasObjects(child, visit)
			}
		}
	}
}

// TestAddFormUsesFolderPicker 盯住"添加项目"的目录行：入口必须是选文件夹，
// 而且空目录时"注册"是灰的——空路径在表单这一层就被挡住，不用关了弹窗再骂一句。
func TestAddFormUsesFolderPicker(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	g := &gui{app: a}
	g.win = a.NewWindow("gui-add")
	g.build()
	g.win.Show()
	g.win.Resize(fyne.NewSize(900, 600))

	g.doAdd()
	var dir *widget.Entry
	var register *widget.Button
	for _, overlay := range g.win.Canvas().Overlays().List() {
		walkCanvasObjects(overlay, func(o fyne.CanvasObject) {
			switch v := o.(type) {
			case *widget.Entry:
				if v.PlaceHolder == "点右侧文件夹图标选择" {
					dir = v
				}
			case *widget.Button:
				if v.Text == "注册" {
					register = v
				}
			}
		})
	}
	if dir == nil {
		t.Fatal("弹窗里没有目录输入框")
	}
	if dir.ActionItem == nil {
		t.Fatal("目录输入框没有『选择文件夹』入口")
	}
	if _, ok := dir.ActionItem.(fyne.Tappable); !ok {
		t.Fatal("文件夹入口不可点")
	}
	if register == nil {
		t.Fatal("弹窗里没有『注册』按钮")
	}
	if !register.Disabled() {
		t.Fatal("还没选目录，『注册』就该是灰的")
	}
	dir.SetText(t.TempDir())
	if register.Disabled() {
		t.Fatal("选好目录后『注册』仍是灰的")
	}
}

// TestGUIDisconnectedShowsRetryState 覆盖 daemon 拉不起来的时候：界面必须说清"没连上"、
// 怎么恢复。GUI 现在会自动尝试拉起 daemon（ensureClient 走 daemon.Client），这里用
// 一个失败的 spawn stub 模拟"拉不起来"，从而确定性地走到断连空态。这片如果没有明确
// 空态，用户看到的就是空白列表，和"注册表里真的没有项目"完全分不出来。
func TestGUIDisconnectedShowsRetryState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("RUNLITE_HOME", home) // 只有目录，没有 daemon.json

	a := test.NewApp()
	defer a.Quit()
	g := &gui{}
	g.app = a
	g.win = a.NewWindow("gui-offline")
	// 注入一个失败的 spawn：模拟"自动拉起 daemon 失败"，让首刷确定性地进断连分支。
	g.spawn = func(exe string, paths *config.Paths) error { return errors.New("stub: 拉不起来") }
	g.build()
	g.win.Show()
	g.win.Resize(fyne.NewSize(900, 600))

	g.refreshProjects()
	if !strings.Contains(g.status.Text, "未连接") {
		t.Fatalf("状态条该报告未连接，实得 %q", g.status.Text)
	}
	if !g.disconnected {
		t.Fatal("没打上断连标记，重连后不会换回正常布局")
	}
	if strings.Contains(g.status.Text, "daemon 在线") {
		t.Fatal("断连状态下还在报在线")
	}
	// 再刷一次不能重建空态（每 1.5s 重建一次会把手指下的"重试"按钮抽走）。
	before := g.win.Canvas().Content()
	g.refreshProjects()
	if g.win.Canvas().Content() != before {
		t.Fatal("断连期间每轮刷新都在重建整块内容")
	}

	// daemon 起来之后必须自动换回正常布局。spawn stub 仍然失败，但 daemon.json 已经
	// 有了、health 也通，ensureClient 直接连上，根本不会走到 spawn。
	info := startInProcessDaemon(t, home)
	_ = info
	g.refreshProjects()
	if g.disconnected {
		t.Fatal("连上之后还挂在空态上")
	}
	if g.win.Canvas().Content() == before {
		t.Fatal("连上之后没有换回正常布局")
	}
	if !strings.Contains(g.status.Text, "daemon 在线") {
		t.Fatalf("重连后状态条没恢复，实得 %q", g.status.Text)
	}
}

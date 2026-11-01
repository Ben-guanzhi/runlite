package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/daemon"
	"runlite/internal/logs"
	"runlite/internal/proc"
	"runlite/internal/service"
)

// 轮询价照着两件事定：daemon 是唯一事实源（界面只读、不记账），以及 jeecg 这种项目
// 启动要 128–133s。列表 1.5s 一轮足够看出"它在动"，日志 0.7s 只取游标之后的增量，
// 免得一个正在编译的项目把 /api 面打满。
const (
	projectsEvery = 1500 * time.Millisecond
	logsEvery     = 700 * time.Millisecond
	logTailLines  = 500
	// maxLogBytes 限制界面里留存的日志总量，按字节预算封顶。日志轮询每 0.7s 把整窗灌进
	// TextGrid 重绘一次：只按行数限，遇到一行几百 KB 的 dump 还是会胀到 GB 级；而且 Fyne
	// 每次整窗重绘都会把全部字形送进渲染缓存，翻得越勤越占。用字节预算从窗口头丢最老的行，
	// 文本总量和单次重绘的文本量都封顶——本机 runlite-gui 光 2000 行中文日志就跑到 2.8 GB
	// （用户提的"内存快赶上 IDEA"就是它）。512 KB 的滚动窗口足够回溯几十屏日志。
	maxLogBytes = 512 * 1024
)

type gui struct {
	app fyne.App
	win fyne.Window

	// body 是正常布局。断连时整块被换成空态，重连成功后要换回来。
	body   *container.Split
	list   *widget.List
	detail *widget.Label
	grid   *widget.TextGrid
	// logScroll 是我们自己套在外面的滚动容器。TextGrid 内置的滚动条要等渲染器建好
	// 才存在（没画过就直接是 nil，实测点"启动"崩过一次），而日志是后台线程灌进来的，
	// 完全可能早于第一次绘制；自己持有外层 Scroll 之后，"跟随日志"随时能滚。
	logScroll *container.Scroll
	status    *widget.Label
	// action 只写最近一次操作的结果。和连接状况分行：合成一行的话，点完"停止"看到的
	// 会是下一轮刷新的"daemon 在线"，操作结果被吃掉（这条是单测逼出来的）。
	action   *widget.Label
	follow   *widget.Check
	startBtn *widget.Button
	stopBtn  *widget.Button

	mu     sync.Mutex
	proj   []api.ProjectView
	selID  string // 选中项按 id 记：序号会被另一边的 add/remove 顶掉
	client *api.Client

	// 日志游标绑在具体那次运行上：换一次运行（Run.ID 变了）ring 从头计数，
	// 不清零就会把新日志当成"已经看过的老行"整批丢掉。
	curRun string
	cursor int
	logMu  sync.Mutex // 串行化日志取数，避免两次取数把同一批行灌两遍
	// lines 是界面内存里的日志滑窗。受 logMu 保护；超字节预算时丢最老的行，
	// 避免 TextGrid 的字符网格随运行时间无限膨胀（见 maxLogBytes 注释）。
	lines      []logs.Line
	busyAction bool

	// disconnected 只由 UI 线程读写：断连时空态已经挂上，就别每 1.5s 重建一次。
	disconnected bool

	// spawn 是"拉起 daemon"的动作，默认 daemon.Spawn；测试可注入 stub 以便确定性地
	// 走断连分支，而不是真去拉一个 daemon。
	spawn func(exe string, paths *config.Paths) error
}

func main() {
	g := &gui{}
	g.app = app.NewWithID("com.runlite.gui")
	g.spawn = daemon.Spawn
	g.win = g.app.NewWindow("Runlite")
	g.win.Resize(fyne.NewSize(1150, 720))
	g.build()
	go g.pollProjects()
	go g.pollLogs()
	go func() { g.refreshProjects(); g.refreshLogs(true) }()
	g.win.ShowAndRun()
}

// 线程规矩：下面标了 (UI) 的函数只在 Fyne 主线程跑（由按钮回调或 fyne.Do 进来），
// 它们内部绝不再调 fyne.Do——文档写明 Do 只该从自己起的 goroutine 里调用。
// 轮询和动作走 goroutine，改控件前先 fyne.Do 回主线程。

func (g *gui) build() {
	g.list = widget.NewList(
		func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.proj)
		},
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(id widget.ListItemID, obj fyne.CanvasObject) {
			g.mu.Lock()
			var text string
			if id >= 0 && id < len(g.proj) {
				text = rowText(g.proj[id])
			}
			g.mu.Unlock()
			obj.(*widget.Label).SetText(text)
		})
	// (UI) 选中只改状态并踢一次日志重取；真正取数在 goroutine 里。
	g.list.OnSelected = func(id widget.ListItemID) {
		g.mu.Lock()
		var p api.ProjectView
		ok := id >= 0 && id < len(g.proj)
		if ok {
			p = g.proj[id]
			g.selID = p.ID
		}
		g.mu.Unlock()
		if !ok {
			return
		}
		g.detail.SetText(detailText(p))
		go g.refreshLogs(true)
	}

	g.detail = widget.NewLabel("（还没有项目：点“添加项目…”）")
	g.detail.Wrapping = fyne.TextWrapOff
	g.detail.TextStyle = fyne.TextStyle{Monospace: true}
	g.grid = widget.NewTextGrid()
	g.grid.Scroll = fyne.ScrollNone
	g.logScroll = container.NewScroll(g.grid)
	g.status = widget.NewLabel("连接中…")
	g.action = widget.NewLabel("")
	g.follow = widget.NewCheck("跟随日志", nil)
	g.follow.SetChecked(true)

	g.startBtn = widget.NewButton("启动", func() { g.act("启动", g.doStart) })
	g.stopBtn = widget.NewButton("停止", func() { g.act("停止", g.doStop) })
	restartBtn := widget.NewButton("重启", func() { g.act("重启", g.doRestart) })
	removeBtn := widget.NewButton("移除", g.askRemove)
	addBtn := widget.NewButton("添加项目…", g.doAdd)
	refreshBtn := widget.NewButton("刷新", func() { go g.refreshProjects() })

	bar := container.NewHBox(refreshBtn, addBtn, g.startBtn, g.stopBtn, restartBtn, removeBtn, g.follow)
	top := container.NewBorder(bar, container.NewVBox(g.status, g.action), nil, nil, nil)

	right := container.NewVSplit(container.NewScroll(g.detail), g.logScroll)
	right.SetOffset(0.34)

	g.body = container.NewHSplit(g.list, right)
	// 0.34：左侧一行要装下"名字 [类型] running pid N 端口 N"，实机截图上 0.28 会把
	// 端口截掉——端口恰恰是这行最该看见的东西。分隔线可以拖，默认值不该让人去拖。
	g.body.SetOffset(0.34)
	g.win.SetContent(container.NewBorder(top, nil, nil, nil, g.body))
}

// ---------- daemon 连接 ----------

// runliteCLI 找同目录（或 PATH 上）的 runlite.exe 去拉起 daemon。
// GUI 自己的 exe 不是 daemon（runlite-gui.exe 没有 daemon exec 子命令），
// 所以必须用 CLI 二进制；同目录没有就退回 PATH 上的 runlite。
func runliteCLI() string {
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "runlite.exe")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	if p, err := exec.LookPath("runlite"); err == nil {
		return p
	}
	return "runlite"
}

// ensureClient 复用已有连接，探活失败才重读 daemon.json（daemon 重启可能换了端口）。
// daemon 不在或连不上时，会自动把它拉起来再连——这样双击 GUI 就能用，不必先去终端
// 敲 runlite daemon start。慢的 spawn/等待放锁外，不阻塞 UI 的其它动作。
//
// 探活走带 token 的 Projects 而非免 token 的 /api/health：daemon 重启换 token 后
// 旧 client 的 health 照样通过，会被复用，之后所有带 token 的调用 401，GUI 卡死。
func (g *gui) ensureClient() (*api.Client, error) {
	g.mu.Lock()
	if g.client != nil {
		if _, perr := g.client.Projects(); perr == nil {
			g.mu.Unlock()
			return g.client, nil
		}
		g.client = nil
	}
	g.mu.Unlock()

	spawn := g.spawn
	if spawn == nil {
		spawn = daemon.Spawn
	}
	c, err := daemon.Client(runliteCLI(), true, spawn)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.client = c
	g.mu.Unlock()
	return c, nil
}

// ---------- 轮询 ----------

func (g *gui) pollProjects() {
	for range time.NewTicker(projectsEvery).C {
		if g.busy() {
			continue
		}
		g.refreshProjects()
	}
}

func (g *gui) pollLogs() {
	for range time.NewTicker(logsEvery).C {
		if g.busy() {
			continue
		}
		g.refreshLogs(false)
	}
}

func (g *gui) busy() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.busyAction
}

func (g *gui) refreshProjects() {
	c, err := g.ensureClient()
	var projs []api.ProjectView
	if err == nil {
		projs, err = c.Projects()
	}
	if err != nil {
		fyne.Do(func() { g.showDisconnected(err) })
		return
	}
	g.mu.Lock()
	g.proj = projs
	g.mu.Unlock()
	fyne.Do(func() { g.applyProjects(projs) })
}

// applyProjects (UI)
func (g *gui) applyProjects(projs []api.ProjectView) {
	if g.takeDisconnectRestore() {
		g.win.SetContent(g.body)
	}
	g.list.Refresh()
	g.status.SetText(fmt.Sprintf("daemon 在线：%d 个项目｜每 %v 刷新｜详情与日志都来自 /api，界面不另存事实", len(projs), projectsEvery))
	g.syncSelection(projs)
}

// takeDisconnectRestore 报告"上一轮还挂在断连空态上，现在连回来了"，并把标记清掉。
// 只在 UI 线程调用（Fyne 主线程串行，flag 不需要额外加锁）。
func (g *gui) takeDisconnectRestore() bool {
	if !g.disconnected {
		return false
	}
	g.disconnected = false
	return true
}

// syncSelection (UI) 按 id 找回选中项。按序号找回会在另一边 add/remove 之后
// 把详情从一个项目跳到另一个项目，而列表看上去"没动"。
// List.Select 对同一个 id 直接返回，所以每轮调用不会反复触发 OnSelected。
func (g *gui) syncSelection(projs []api.ProjectView) {
	g.mu.Lock()
	selID := g.selID
	g.mu.Unlock()
	idx := -1
	for i := range projs {
		if projs[i].ID == selID {
			idx = i
			break
		}
	}
	switch {
	case idx >= 0:
		g.list.Select(idx)
		g.detail.SetText(detailText(projs[idx]))
	case len(projs) > 0:
		g.list.Select(0)
		g.detail.SetText(detailText(projs[0]))
	default:
		g.list.UnselectAll()
		g.detail.SetText("（还没有项目：点“添加项目…”）")
		g.grid.SetText("")
	}
}

// refreshLogs 取选中项目的日志增量（在 goroutine 里跑）。
// logMu 串行化：轮询、选中、动作收尾都可能触发取数，两次取数并行会把同一批行灌两遍。
func (g *gui) refreshLogs(full bool) {
	g.logMu.Lock()
	defer g.logMu.Unlock()
	g.fetchLogsLocked(full)
}

// fetchLogsLocked 假定调用方已经拿着 logMu。
func (g *gui) fetchLogsLocked(full bool) {
	g.mu.Lock()
	c, key := g.client, g.selID
	if full {
		g.curRun, g.cursor = "", 0
	}
	cur, run := g.cursor, g.curRun
	g.mu.Unlock()
	if c == nil || key == "" {
		return
	}

	resp, err := c.Logs(key, logTailLines, cur, "")
	if err != nil {
		// 刚 add 完的项目没有运行记录是正常状态，不该每 0.7s 在状态条上吼一次。
		if !errors.Is(err, proc.ErrNotRunning) && !strings.Contains(err.Error(), "还没有运行记录") {
			msg := "日志: " + err.Error()
			fyne.Do(func() { g.action.SetText(msg) })
		}
		return
	}

	rid := ""
	if resp.Run != nil {
		rid = resp.Run.ID
	}
	if rid != run {
		g.mu.Lock()
		g.curRun, g.cursor = rid, 0
		g.mu.Unlock()
		fyne.Do(func() { g.grid.SetText("") })
		if len(resp.Lines) == 0 {
			return
		}
	}
	g.mu.Lock()
	g.cursor = advanceCursor(resp.Lines, g.cursor)
	g.mu.Unlock()

	if len(resp.Lines) == 0 {
		return
	}
	// fetchLogsLocked 的调用方已持有 logMu（见 refreshLogs），这里直接操作 g.lines。
	if rid != run {
		g.lines = g.lines[:0]
	}
	g.lines = append(g.lines, resp.Lines...)
	trimLogWindow(&g.lines)
	text := logText(g.lines)

	fyne.Do(func() {
		g.grid.SetText(text)
		if g.follow.Checked {
			g.logScroll.ScrollToBottom()
		}
	})
}

// ---------- 动作 ----------

// act 把一次 daemon 调用丢进 goroutine：wait=true 的启动会阻塞两分钟，
// 放在 UI 线程上等于整个窗口冻住。期间 busyAction 让轮询闭嘴，免得旧结果盖掉新结果。
func (g *gui) act(name string, fn func(*api.Client) (string, error)) {
	if !g.setBusy(true) {
		return
	}
	go func() {
		defer g.setBusy(false)
		fyne.Do(func() { g.action.SetText(name + "：进行中…") })
		c, err := g.ensureClient()
		msg := ""
		if err == nil {
			msg, err = fn(c)
		}
		out := name + "："
		switch {
		case err != nil:
			out += err.Error()
		case msg != "":
			out += msg
		default:
			out += "完成"
		}
		fyne.Do(func() { g.action.SetText(out) })
		g.refreshProjects()
		g.refreshLogs(true)
	}()
}

func (g *gui) setBusy(v bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if v && g.busyAction {
		return false
	}
	g.busyAction = v
	return true
}

func (g *gui) selected() (api.ProjectView, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range g.proj {
		if p.ID == g.selID {
			return p, true
		}
	}
	return api.ProjectView{}, false
}

func (g *gui) doStart(c *api.Client) (string, error) {
	p, ok := g.selected()
	if !ok {
		return "", errors.New("先在左侧选一个项目")
	}
	// wait=false：启动由 daemon 记账，界面靠轮询看它从 starting 走到 running。
	run, err := c.Start(p.ID, service.StartRequest{Key: p.ID}, false)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已发起 %s（pid %d，配置 %s）", run.Name, run.PID, orDash(run.ConfigID)), nil
}

func (g *gui) doStop(c *api.Client) (string, error) {
	p, ok := g.selected()
	if !ok {
		return "", errors.New("先在左侧选一个项目")
	}
	run, err := c.Stop(p.ID, true)
	if err != nil {
		return "", err
	}
	if run.NoOp {
		return "空操作：" + run.Reason, nil
	}
	return fmt.Sprintf("已结束 %s", run.Name), nil
}

func (g *gui) doRestart(c *api.Client) (string, error) {
	p, ok := g.selected()
	if !ok {
		return "", errors.New("先在左侧选一个项目")
	}
	if _, err := c.Stop(p.ID, false); err != nil && !strings.Contains(err.Error(), "没有在运行") {
		return "", fmt.Errorf("停止阶段失败: %w", err)
	}
	run, err := c.Start(p.ID, service.StartRequest{Key: p.ID}, false)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已重启 %s（pid %d）", run.Name, run.PID), nil
}

// askRemove (UI) 确认在 UI 线程弹，真正的删除仍走 act 的 goroutine。
func (g *gui) askRemove() {
	p, ok := g.selected()
	if !ok {
		g.action.SetText("移除：先在左侧选一个项目")
		return
	}
	msg := fmt.Sprintf("移除 %s？只删注册表里这一条，磁盘上的项目不动，正在运行的进程也不会被结束。", p.Name)
	dialog.ShowConfirm("移除项目", msg, func(yes bool) {
		if !yes {
			return
		}
		g.act("移除", func(c *api.Client) (string, error) {
			return "", c.Remove(p.ID)
		})
	}, g.win)
}

// doAdd (UI)
//
// 目录的主路径是"选文件夹"：输入框右侧的文件夹图标拉起目录选择器，选完回填绝对路径。
// 输入框仍可编辑（粘一条路径比在目录树里点六级快），但挂上校验器后空目录不能提交，
// 错误直接显示在表单里，而不是关掉弹窗再在状态条上骂一句。
func (g *gui) doAdd() {
	dir := widget.NewEntry()
	dir.SetPlaceHolder("点右侧文件夹图标选择")
	dir.Validator = func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("先选择项目文件夹")
		}
		return nil
	}
	name := widget.NewEntry()
	name.SetPlaceHolder("留空则用目录名")
	cmd := widget.NewEntry()
	cmd.SetPlaceHolder("留空则自动检测（Maven/Gradle/Node）；填了按自定义命令注册")

	// 选择器入口挂在 Entry 自己的 action item 上，不另占一行：弹窗本来就窄，
	// 再并一个"浏览…"按钮会把输入框挤成一条缝。
	autoName := "" // 上一次自动填进去的名字，用来判断"用户改过没有"
	dir.ActionItem = widget.NewButtonWithIcon("", theme.FolderOpenIcon(), func() {
		picker := dialog.NewFolderOpen(func(lu fyne.ListableURI, err error) {
			switch {
			case err != nil:
				dialog.ShowError(err, g.win)
			case lu == nil: // 取消：保留原来填的内容
			default:
				p := lu.Path()
				dir.SetText(p)
				// 名字留空、或者还是上一次自动填的那个，就跟着目录走；
				// 用户自己敲过的名字不覆盖（否则选第二个目录时名字会停在第一个上）。
				if name.Text == "" || name.Text == autoName {
					autoName = defaultProjectName(p)
					name.SetText(autoName)
				}
			}
		}, g.win)
		picker.Resize(fyne.NewSize(820, 560))
		picker.Show()
	})

	items := []*widget.FormItem{
		widget.NewFormItem("目录", dir),
		widget.NewFormItem("名字", name),
		widget.NewFormItem("自定义命令", cmd),
	}
	d := dialog.NewForm("添加项目", "注册", "取消", items, func(ok bool) {
		if !ok {
			return
		}
		// 正常情况下注册按钮在校验失败时是灰的；这里只是兜底，别把空路径发给 daemon。
		dpath := strings.TrimSpace(dir.Text)
		if dpath == "" {
			g.action.SetText("添加：目录不能为空")
			return
		}
		nm, cm := name.Text, cmd.Text
		g.act("添加", func(c *api.Client) (string, error) {
			pv, err := c.Add(service.AddRequest{Dir: dpath, Name: nm, Cmd: cm})
			if err != nil {
				return "", err
			}
			g.mu.Lock()
			g.selID = pv.ID
			g.mu.Unlock()
			return pv.Name + " 已注册", nil
		})
	}, g.win)
	// 默认宽度按最小尺寸算，三个输入框和占位提示会被截断（实测"留空则…"只剩几个字）；
	// 680 放得下一条完整的 Maven 命令，也还在 1150 宽的主窗口之内。
	d.Resize(fyne.NewSize(680, 300))
	d.Show()
}

// ---------- 断连空态 ----------

// showDisconnected (UI) 把整块内容换成一句明确的话：为什么是空的、怎么恢复。
// 只留一片空白的话，"没有项目"和"没连上 daemon"就分不出来了。
func (g *gui) showDisconnected(err error) {
	g.status.SetText("daemon 未连接")
	if g.disconnected {
		return // 已经挂在空态上了：只更新状态条，不反复重建整块内容
	}
	g.disconnected = true
	msg := "daemon 没有连接\n\n" + err.Error() + "\n\nGUI 已经尝试自动拉起 daemon（跟 runlite daemon start 同一套）。\n还是不行的话：确认 runlite.exe 在同目录或 PATH 上，然后点重试。"
	label := widget.NewLabel(msg)
	label.Wrapping = fyne.TextWrapWord
	retry := widget.NewButton("重试", func() {
		g.mu.Lock()
		g.client = nil
		g.mu.Unlock()
		go g.refreshProjects()
	})
	box := container.NewVBox(container.NewCenter(label), container.NewCenter(retry))
	g.win.SetContent(container.NewPadded(box))
}

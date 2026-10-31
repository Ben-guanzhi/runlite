// Package proc 负责把项目拉起来、判定它真的起来了、再把它停干净。
package proc

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"runlite/internal/env"
	"runlite/internal/logs"
	"runlite/internal/netx"
	"runlite/internal/project"
)

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopping State = "stopping"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	StateZombie   State = "zombie" // 自认为还在跑，但进程已不在
	// StateOrphan：进程确实还在，但是由上一个 daemon 拉起的 —— 没有日志管道，
	// 只能靠 meta 里记的 PID 结束它。宁可明说"接管不了日志"，也不要报成"停止"。
	StateOrphan State = "orphan"
)

func (s State) Active() bool {
	return s == StateStarting || s == StateRunning || s == StateStopping || s == StateOrphan
}

type Options struct {
	RunsDir        string
	RingSize       int
	StartTimeout   time.Duration
	DefaultGrace   time.Duration
	PortScanGrace  time.Duration // 启动后多久开始用"新端口差集"兜底找端口
	ScanInterval   time.Duration
	Report         *env.Report
	ReportProvider func() *env.Report
}

// RunInfo 是一次运行的对外快照字段。单独成结构体是为了让 Clone 能只复制这些纯数据：
// 直接 *new = *r 会把 sync.Mutex 一起复制过去，既过不了 vet，也会留下一把已经上锁的副本。
type RunInfo struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	Name      string `json:"name"`
	ConfigID  string `json:"configId"`
	State     State  `json:"state"`
	Cmd       string `json:"cmd"`
	Dir       string `json:"dir"`
	PID       int    `json:"pid,omitempty"`
	// PidCreated 是该进程的创建时间（Windows FILETIME）。PID 会被系统回收复用，
	// 只有 PID+创建时间才真正标识"当初那个进程对象"；跨 daemon 生命周期接管或结束
	// 进程时必须核对它，否则可能去杀一个恰好捡到同一个号码的无关进程。
	PidCreated uint64    `json:"pidCreated,omitempty"`
	Port       int       `json:"port,omitempty"`
	LogPath    string    `json:"logPath,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	EndedAt    time.Time `json:"endedAt,omitempty"`
	ExitCode   int       `json:"exitCode,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Stopped    bool      `json:"stoppedByUser,omitempty"`
	// NoOp 标记"这次 stop 其实什么都没做，项目本来就没在跑"。没有它，CLI 会把上一次
	// 运行遗留的退出码当成这次的成果印出来，用户看不出自己是在重复操作。
	NoOp bool `json:"noOp,omitempty"`
}

type Run struct {
	RunInfo

	mu     sync.Mutex
	log    *logs.Log
	cmd    *exec.Cmd
	doneCh chan struct{}
	// misses 记录连续多少次"进程查不到"。外部工具会偶发失败，
	// 单次负样本不足以判定进程已消失，否则活着的 mvn 会被误标成 zombie。
	misses int
}

func (r *Run) Uptime() time.Duration {
	if r.StartedAt.IsZero() {
		return 0
	}
	if r.EndedAt.IsZero() {
		return time.Since(r.StartedAt)
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// Clone 取一份对外快照。只复制 RunInfo：把整个 Run 拷过来会连 sync.Mutex 一起拷，
// 副本里那把锁的状态不可控，而且这正是之前两处自锁死锁的来源。
func (r *Run) Clone() *Run {
	r.mu.Lock()
	info := r.RunInfo
	r.mu.Unlock()
	return &Run{RunInfo: info}
}

type StartSpec struct {
	Project *project.Project
	Config  *project.LaunchConfig
	Args    []string // 追加参数
}

type Manager struct {
	opt     Options
	mu      sync.Mutex
	active  map[string]*Run
	history map[string][]*Run
	bus     *logs.Bus
	stopCh  chan struct{}
}

func NewManager(opt Options) *Manager {
	if opt.RingSize <= 0 {
		opt.RingSize = 2000
	}
	if opt.StartTimeout <= 0 {
		opt.StartTimeout = 120 * time.Second
	}
	if opt.DefaultGrace <= 0 {
		opt.DefaultGrace = 15 * time.Second
	}
	if opt.PortScanGrace <= 0 {
		opt.PortScanGrace = 10 * time.Second
	}
	if opt.ScanInterval <= 0 {
		// 每轮都要 spawn 一个 netstat，300ms 太密：它和 tasklist 抢同一批进程查询，
		// 高频互扰会让 tasklist 偶发失败，进而把活进程误判成已退出。
		opt.ScanInterval = time.Second
	}
	m := &Manager{
		opt: opt, active: map[string]*Run{}, history: map[string][]*Run{},
		bus: logs.NewBus(50), stopCh: make(chan struct{}),
	}
	go m.reapLoop()
	return m
}

func (m *Manager) Close() { close(m.stopCh) }

func (m *Manager) Bus() *logs.Bus { return m.bus }

var reStarted = regexp.MustCompile(`Started\s+[A-Za-z0-9_$.<>\[\]-]+\s+in\s+[0-9.]+\s`)
var reFirstError = regexp.MustCompile(`(?i)(^\s*ERROR|Caused by:|BUILD FAILURE|APPLICATION FAILED TO START|Exception in thread)`)

// Start 拉起一个项目；同一项目已在跑则返回现有运行，不重复启动。
func (m *Manager) Start(spec StartSpec) (*Run, error) {
	p := spec.Project
	if p == nil {
		return nil, errors.New("缺少项目")
	}
	cfg := spec.Config
	if cfg == nil {
		c, err := p.ActiveConfig()
		if err != nil {
			return nil, err
		}
		cfg = c
	}

	// direct-main 配置要先解析 classpath、写 argfile（要跑一次 mvn，可能几秒到几十秒），
	// 必须在拿全局锁之前做完，否则整个 daemon 的启停都被这一下阻塞。
	var prepared *PreparedLaunch
	if cfg.MainClass != "" {
		var perr error
		prepared, perr = m.prepareDirectMain(cfg, filepath.FromSlash(p.Dir), spec.Args)
		if perr != nil {
			return nil, perr
		}
	}

	m.mu.Lock()
	if r, ok := m.active[p.ID]; ok && r.State.Active() {
		m.mu.Unlock()
		return nil, fmt.Errorf("项目 %s 已在运行（%s，pid %d，端口 %d）", p.Name, r.State, r.PID, r.Port)
	}
	// Second-granularity IDs collide when two projects start in the same
	// second (or a fast-fail restart), and bus/history key on the ID — the
	// later Put silently overwrote the earlier run's log. A monotonic
	// millisecond component keeps the readable prefix and the uniqueness.
	runID := time.Now().Format("20060102-150405") + fmt.Sprintf("-%03d", time.Now().Nanosecond()/1e6)
	r := &Run{
		RunInfo: RunInfo{
			ID: runID, ProjectID: p.ID, Name: p.Name, ConfigID: cfg.ID,
			State: StateStarting, Dir: filepath.FromSlash(p.Dir), StartedAt: time.Now(),
		},
		doneCh: make(chan struct{}),
	}

	name, args := "java", []string{}
	if prepared != nil {
		// 单 JVM 直跑 main：argv 已经在 prepare 里备好（java @argfile）
		name, args = prepared.Name, prepared.Args
	} else {
		var err error
		name, args, err = buildArgv(p.Kind, cfg, filepath.FromSlash(p.Dir), spec.Args)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
	}
	r.Cmd = strings.Join(append([]string{name}, args...), " ")

	if err := os.MkdirAll(m.opt.RunsDir, 0o700); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	r.LogPath = filepath.Join(m.opt.RunsDir, sanitize(p.ID)+"-"+runID+".log")
	lg, err := logs.Create(r.LogPath, m.opt.RingSize)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	r.log = lg

	cmd := exec.Command(name, args...)
	cmd.Dir = r.Dir
	cmd.Env = buildEnv(p.Kind, cfg, m.report())
	decorate(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		lg.Close()
		m.mu.Unlock()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		lg.Close()
		m.mu.Unlock()
		return nil, err
	}
	lg.Append(false, "$ "+r.Cmd)
	lg.Append(false, "  工作目录: "+r.Dir)
	lg.Append(false, "  环境: "+strings.Join(envSummary(cmd.Env, p.Kind), "  "))
	// baseline 必须在 cmd.Start() 之前取：进程起来后、取快照前它就可能已经监听，
	// 那个端口于是落在 baseline 里，差集永远认不出它（本机实测：子进程 60ms 内
	// 绑定完成，runlite 一直报"没有监听端口"）。
	baseline := netx.SnapshotPorts()
	if err := cmd.Start(); err != nil {
		lg.Append(true, "启动失败: "+err.Error())
		lg.Close()
		m.mu.Unlock()
		return nil, fmt.Errorf("启动 %s 失败: %w", name, err)
	}
	r.cmd = cmd
	r.PID = cmd.Process.Pid
	// 立刻记下进程对象的创建时间：daemon 重启后要靠它认出"还是不是当初那个进程"，
	// 隔得久了 PID 可能被系统发给别的进程。
	if id, ok := netx.ProcIdent(r.PID); ok {
		r.PidCreated = id
	}
	m.active[p.ID] = r
	m.recordLocked(r)
	m.mu.Unlock()
	m.writeMeta(r)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stderr, lg, true) }()
	go func() { defer wg.Done(); pump(stdout, lg, false) }()
	go func() {
		wg.Wait()
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = exitCode(err)
		}
		m.finish(r, code, err)
	}()
	go m.watchReady(r, cfg, baseline)
	go m.flushLoop(r, lg)
	return r.Clone(), nil
}

func (m *Manager) report() *env.Report {
	if m.opt.ReportProvider != nil {
		return m.opt.ReportProvider()
	}
	return m.opt.Report
}

// watchReady 判定 starting → running；超时不杀进程，只把状态降级为 failed 并给出定位。
func (m *Manager) watchReady(r *Run, cfg *project.LaunchConfig, baseline map[int]int) {
	r.mu.Lock()
	pid := r.PID
	r.mu.Unlock()
	deadline := time.Now().Add(m.opt.StartTimeout)
	slowWarned := false
	tick := time.NewTicker(m.opt.ScanInterval)
	defer tick.Stop()
	seenSeq := 0
	for {
		select {
		case <-r.doneCh:
			return
		case <-m.stopCh:
			return
		case <-tick.C:
		}
		lines, newSeq := r.tailSince(seenSeq)
		seenSeq = newSeq
		readyByLog := false
		for _, ln := range lines {
			if matchesReadiness(ln.Text, cfg.Readiness) {
				readyByLog = true
				break
			}
		}
		now := netx.SnapshotPorts()
		port := 0
		if cfg.Port > 0 {
			// 端口还没监听时根本不需要查进程树——那是每个心跳最贵的调用，
			// 启动阶段前几十秒它每次都被白算一次。
			if owner, listening := now[cfg.Port]; listening {
				if netx.TreePIDs(pid)[owner] {
					port = cfg.Port
				}
			}
		} else if time.Since(r.StartedAt) > m.opt.PortScanGrace {
			// 兜底找端口才需要进程树：差集里每个候选端口都要确认归属，
			// 提前 PortScanGrace 秒不做这件事，正好省掉冷启动那段纯浪费。
			tree := netx.TreePIDs(pid)
			for _, p := range netx.NewPorts(baseline, now) {
				if tree[now[p]] {
					port = p
					break
				}
			}
		}
		if readyByLog || port > 0 {
			m.markRunning(r, port)
			return
		}
		if time.Now().After(deadline) {
			if !slowWarned {
				slowWarned = true
				m.markSlow(r, lines)
			}
			// 到点不等于失败：本机实测一个 jeecg 应用 132s 才就绪，而 120s 的门槛
			// 会把它判成 failed —— 于是端口在听、进程在跑，runlite 却说它死了，
			// 后续 stop 还会因为这个标签直接跳过。进程自己会给出结论：真退出时
			// finish() 按退出码定 failed/stopped，就绪了就转 running。
			continue
		}
	}
}

func (r *Run) tailSince(since int) ([]logs.Line, int) {
	// 传 seenSeq 让 Tail 只取"从上一次检查之后"的新行，而不是每次把整个 2000 行环形
	// 缓冲都翻一遍——Spring Boot 冷启动要一两分钟，这段时间每秒都在做全量扫描是浪费。
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.log == nil {
		return nil, since
	}
	lines := r.log.Tail(0, since)
	if len(lines) > 0 {
		since = lines[len(lines)-1].Seq
	}
	return lines, since
}

func (m *Manager) markRunning(r *Run, port int) {
	r.mu.Lock()
	if r.State == StateStarting {
		r.State = StateRunning
		r.Reason = ""
		if port > 0 {
			r.Port = port
		}
	}
	r.mu.Unlock()
	m.writeMeta(r)
}

// markSlow 只记录"启动比预期慢"这个事实，不改变状态：进程还活着、还在启动，
// 把它写成 failed 会让 stop 以为无需结束它，也会让 list 报出一个假失败。
func (m *Manager) markSlow(r *Run, lines []logs.Line) {
	r.mu.Lock()
	if r.State == StateStarting {
		r.Reason = fmt.Sprintf("启动较慢：超过 %s 仍未出现就绪日志，也没有监听端口（进程仍在，继续观察）", m.opt.StartTimeout)
		r.noteFirstError(lines)
		if r.log != nil {
			r.log.Append(false, fmt.Sprintf("— 启动已超过 %s，仍未就绪，继续观察 —", m.opt.StartTimeout))
		}
	}
	r.mu.Unlock()
	m.writeMeta(r)
}

func matchesReadiness(text string, custom []string) bool {
	if reStarted.MatchString(text) {
		return true
	}
	for _, pat := range custom {
		if re := compileReadiness(pat); re != nil && re.MatchString(text) {
			return true
		}
	}
	return false
}

// reCache 记住编译好的自定义就绪正则。matchesReadiness 在每个项目的每个心跳、
// 对每一行新日志跑一次——原先每次都在 regexp.Compile，JVM 冷启动时一秒几百行，
// 那点正则解析开销比正则自身还大。缓存一次后就是纯匹配，没有分配。
//
// 用 sync.Map：key 就是用户写的 pattern 字符串，天然不可变；LoadOrStore 天然
// 处理并发首次访问，即使两个 goroutine 同时编译也没关系（后者丢弃自己那份）。
var reCache sync.Map // map[string]*regexp.Regexp

func compileReadiness(pat string) *regexp.Regexp {
	if v, ok := reCache.Load(pat); ok {
		return v.(*regexp.Regexp)
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		// 存 nil 会让每次调用都重新尝试编译同一个坏 pattern（浪费但正确）；
		// 更省的是缓存一个 sentinel，但那要为"坏 pattern"引入第二种类型，得不偿失。
		return nil
	}
	actual, _ := reCache.LoadOrStore(pat, re)
	return actual.(*regexp.Regexp)
}

func (r *Run) noteFirstError(lines []logs.Line) {
	for _, ln := range lines {
		if reFirstError.MatchString(ln.Text) {
			r.Reason += "；首条错误线索: " + truncate(ln.Text, 200)
			return
		}
	}
}

func (m *Manager) finish(r *Run, code int, err error) {
	r.mu.Lock()
	if r.log != nil {
		r.log.Flush()
	}
	r.ExitCode = code
	r.EndedAt = time.Now()
	switch {
	case r.State == StateStopping:
		r.State = StateStopped
	case r.State == StateFailed:
		// 超时判定已经写了 Reason，进程随后退出时不覆盖
	case r.Stopped:
		r.State = StateStopped
	case code == 0:
		r.State = StateStopped
		r.Reason = "进程正常退出"
	default:
		r.State = StateFailed
		if err != nil {
			r.Reason = "进程异常退出: " + err.Error()
		} else {
			r.Reason = fmt.Sprintf("进程退出，退出码 %d", code)
		}
	}
	if r.log != nil {
		up := r.Uptime().Round(time.Second)
		mark := fmt.Sprintf("— 进程结束，退出码 %d，运行 %s —", code, up)
		switch {
		case r.Stopped:
			mark = fmt.Sprintf("— 已按请求停止，运行 %s —", up)
		case r.State == StateFailed:
			mark = fmt.Sprintf("— 进程异常退出，退出码 %d，运行 %s：%s —", code, up, r.Reason)
		}
		r.log.Append(false, mark)
		r.log.Close()
		r.log = nil
	}
	close(r.doneCh)
	r.mu.Unlock()

	m.mu.Lock()
	if cur, ok := m.active[r.ProjectID]; ok && cur == r {
		delete(m.active, r.ProjectID)
	}
	m.mu.Unlock()
	m.writeMeta(r)
}

func (m *Manager) flushLoop(r *Run, lg *logs.Log) {
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-r.doneCh:
			return
		case <-m.stopCh:
			return
		case <-t.C:
			lg.Flush()
		}
	}
}

// ErrNotRunning 表示这个项目当前没在运行、没有可停的东西。做成哨兵是为了让上层
// 能把"已经是停止状态"和"真的出错"分开——stop 在脚本里的语义是"确保停下来"。
var ErrNotRunning = errors.New("当前没有在运行")

// Stop 停掉一次运行：先普通结束，超过宽限期再强制，最后核对端口是否真的释放。
func (m *Manager) Stop(projectID string, force bool) (*Run, error) {
	m.mu.Lock()
	r, ok := m.active[projectID]
	m.mu.Unlock()
	if !ok || r == nil {
		return nil, ErrNotRunning
	}
	return m.stopRun(r, force)
}

func (m *Manager) stopRun(r *Run, force bool) (*Run, error) {
	// 停不停得看事实，不看标签：failed/zombie 只代表 runlite 当时没等到结论，
	// 进程完全可能还在跑（本机实测：应用 132s 就绪、8081 一直在听，而状态是 failed）。
	// 按标签直接返回就是"告诉用户已经停了，其实服务还在端口上"。
	if !m.trackedActive(r) && !m.sameProc(r) {
		return r.Clone(), nil
	}
	r.mu.Lock()
	if !r.State.Active() {
		r.State = StateStarting // 标签与事实不符：先当作在跑，下面按真实进程处理
	}
	r.Stopped = true
	if !force {
		r.State = StateStopping
	}
	grace := m.opt.DefaultGrace
	r.mu.Unlock()

	pid := r.PID
	orphan := r.doneCh == nil
	if orphan {
		// 孤儿是上一次 daemon 生命周期留下的记录，只有 PID 是它和外界的联系。
		// PID 会被系统回收复用：核对创建时间，认不出本人就绝不 taskkill，
		// 否则打到同号码的新进程上，等于无缘无故杀了别人家的程序。
		if !m.sameProc(r) {
			r.mu.Lock()
			r.Reason = fmt.Sprintf("pid %d 已经不是当初那个进程（进程号被系统复用），没有结束任何进程", pid)
			r.mu.Unlock()
			m.forgetRun(r)
			return r.Clone(), nil
		}
	}
	if force {
		_ = netx.KillTree(pid, true)
	} else {
		if err := netx.KillTree(pid, false); err != nil {
			// 普通结束请求被拒是常态（无窗口进程），直接升级
			force = true
		}
	}
	if force {
		_ = netx.KillTree(pid, true)
	}

	// 孤儿没有 Wait 协程，doneCh 是 nil（对 nil channel 的接收会永久阻塞），只能轮询进程对象。
	if orphan {
		if !m.waitForRunGone(r, grace) {
			_ = netx.KillTree(pid, true)
			if !m.waitForRunGone(r, 5*time.Second) {
				r.mu.Lock()
				r.State = StateZombie
				r.Reason = "强制结束后 5s 内进程对象仍存在"
				r.mu.Unlock()
			}
		}
		m.forgetRun(r)
	} else {
		select {
		case <-r.doneCh:
		case <-time.After(grace):
			_ = netx.KillTree(pid, true)
			select {
			case <-r.doneCh:
			case <-time.After(5 * time.Second):
				r.mu.Lock()
				r.State = StateZombie
				r.Reason = "强制结束后 5s 内仍未确认进程退出"
				r.mu.Unlock()
			}
		}
	}

	r.mu.Lock()
	port := r.Port
	r.mu.Unlock()
	// 端口的复核要看"还有谁在监听"，并且要给操作系统留收尾时间：刚 taskkill 完的
	// 一瞬间监听套接字常还没撤，用一次 TCP 握手下结论，就会把已经停成功的项目报成
	// zombie（本机实测：stop 报"端口 8081 仍被 PID [] 占用"，而那个空列表恰恰说明没人占用）。
	if port > 0 {
		if owners := waitPortCleared(port, 3*time.Second); len(owners) > 0 {
			r.mu.Lock()
			if r.State != StateZombie {
				r.State = StateZombie
			}
			r.Reason = fmt.Sprintf("端口 %d 仍被 PID %v 监听；那不是我刚结束的进程（pid %d），需要手工处理", port, owners, r.PID)
			r.mu.Unlock()
		}
	}
	return r.Clone(), nil
}

// waitPortCleared 等端口上不再有 LISTENING；返回超时那一刻仍在监听的 PID，空表示已释放。
// 只看 LISTENING 是刻意的：TIME_WAIT 是已经结束的连接的正常余温，把它当"占用"会误报。
func waitPortCleared(port int, timeout time.Duration) []int {
	deadline := time.Now().Add(timeout)
	for {
		owners := netx.PIDsOnPort(port)
		if len(owners) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return owners
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// trackedActive 是"runlite 自己认为这次运行还在进行中"。只用于和事实对照，
// 不能拿它单独决定要不要结束进程——见 stopRun 里的说明。
func (m *Manager) trackedActive(r *Run) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.State.Active()
}

// sameProc 核对"记录里那个进程对象现在还在不在"。
// 不能只持有 r.mu 时调用：netx 那边要开句柄做系统调用，抱着锁等内核不划算。
func (m *Manager) sameProc(r *Run) bool {
	r.mu.Lock()
	pid, born := r.PID, r.PidCreated
	r.mu.Unlock()
	return netx.PidSame(pid, born)
}

func (m *Manager) waitForRunGone(r *Run, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !m.sameProc(r) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !m.sameProc(r)
}

// forgetRun 把一次已结束的孤儿运行从 active 挪走（正常运行的结束由 Wait 协程负责）。
func (m *Manager) forgetRun(r *Run) {
	m.mu.Lock()
	if cur, ok := m.active[r.ProjectID]; ok && cur == r {
		delete(m.active, r.ProjectID)
	}
	m.mu.Unlock()
	r.mu.Lock()
	if r.EndedAt.IsZero() {
		r.EndedAt = time.Now()
	}
	if r.State != StateZombie {
		r.State = StateStopped
	}
	r.mu.Unlock()
	m.writeMeta(r)
}

// Restart 先停后起；停止失败也继续尝试启动，把两件事的结果都回传。
func (m *Manager) Restart(spec StartSpec) (*Run, error) {
	m.mu.Lock()
	r, ok := m.active[spec.Project.ID]
	m.mu.Unlock()
	if ok && r.State.Active() {
		if _, err := m.stopRun(r, false); err != nil {
			return nil, err
		}
	}
	return m.Start(spec)
}

func (m *Manager) Remove(projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.active[projectID]; ok && r.State.Active() {
		r.mu.Lock()
		r.Stopped = true
		pid, born := r.PID, r.PidCreated
		r.mu.Unlock()
		// The same discipline stopRun follows: the PID may have been reused
		// (Reclaim took over an orphan and the original died), and taskkill
		// on a stranger is exactly what the identity checks exist to prevent.
		if netx.PidSame(pid, born) {
			_ = netx.KillTree(pid, true)
		}
		delete(m.active, projectID)
	}
	delete(m.history, projectID)
}

// Prune 把"记录里还在跑、实际进程对象已不在"的运行标成 zombie。
// 要连续两轮都查不到才动手：被强杀的进程对象会滞后一小会儿才消失，
// 一轮负样本可能只是撞在它的收尾上。
func (m *Manager) Prune() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.active {
		if !r.State.Active() {
			continue
		}
		if r.PID <= 0 {
			continue
		}
		r.mu.Lock()
		if netx.PidSame(r.PID, r.PidCreated) {
			r.misses = 0
			r.mu.Unlock()
			continue
		}
		r.misses++
		if r.misses < 2 {
			r.mu.Unlock()
			continue
		}
		r.State = StateZombie
		r.Reason = "进程已不存在（可能由外部结束），runlite 无法确认其退出码"
		r.EndedAt = time.Now()
		r.mu.Unlock()
		n++
	}
	return n
}

func (m *Manager) Get(projectID string) (*Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.active[projectID]
	if !ok {
		return nil, false
	}
	return r.Clone(), true
}

// Latest 返回该项目最近一次运行（含已结束的）。列表和状态页靠它把 failed、
// zombie 这类结论留在案面上：只报"停止"会把一次启动失败说成风平浪静。
func (m *Manager) Latest(projectID string) (*Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.history[projectID]
	if len(rs) == 0 {
		return nil, false
	}
	return rs[0].Clone(), true
}

func (m *Manager) List() []*Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Run
	for _, rs := range m.history {
		for _, r := range rs {
			out = append(out, r.Clone())
		}
	}
	sortRuns(out)
	return out
}

func (m *Manager) Logs(runID string) (*logs.Log, bool) { return m.bus.Get(runID) }

func (m *Manager) recordLocked(r *Run) {
	m.history[r.ProjectID] = append([]*Run{r}, m.history[r.ProjectID]...)
	if len(m.history[r.ProjectID]) > 20 {
		m.history[r.ProjectID] = m.history[r.ProjectID][:20]
	}
	m.bus.Put(r.ID, r.log)
}

// writeMeta 落一份运行元数据，供 daemon 重启后修账与事后排查。
// 只能在未持有 r.mu 时调用（Clone 会再锁一次，Go 的 Mutex 不可重入）。
func (m *Manager) writeMeta(r *Run) {
	if m.opt.RunsDir == "" {
		return
	}
	raw, err := json.MarshalIndent(r.Clone(), "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(m.opt.RunsDir, 0o700)
	_ = os.WriteFile(filepath.Join(m.opt.RunsDir, sanitize(r.ProjectID)+"-"+r.ID+".meta.json"), raw, 0o600)
}

// Reclaim 在 daemon 启动时扫上一次的运行记录：进程确实还活着的，以 orphan 状态接管
// （能停，但没有日志管道）；进程已消失的，把记录改写成 zombie，避免下次又当"在跑"。
// 没有这一步，重启 daemon 后一个仍占着端口的 java 会被报成"停止"，那是假陈述。
func (m *Manager) Reclaim() (adopted int, cleared int) {
	if m.opt.RunsDir == "" {
		return 0, 0
	}
	metas, err := filepath.Glob(filepath.Join(m.opt.RunsDir, "*.meta.json"))
	if err != nil {
		return 0, 0
	}
	// 两段式：读文件与进程探查（PidSame / ListeningPortOf 都是系统调用）
	// 在锁外完成，m.mu 只圈住最后的记账突变——此前整套系统调用抱着锁，
	// daemon 重启窗口内 Start/Stop 全被阻塞。
	type decision struct {
		path      string
		info      RunInfo
		adopt     bool
		portFound bool
	}
	decisions := make([]decision, 0, len(metas))
	for _, path := range metas {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		// meta 里只有对外字段，直接反序列化成 RunInfo：
		// 拷一个 Run 值会连 sync.Mutex 一起拷，接管进来的运行本来就没有锁状态可言。
		var info RunInfo
		if err := json.Unmarshal(raw, &info); err != nil || info.ProjectID == "" {
			continue
		}
		// 接管也必须认身份：老记录里的 PID 可能已被系统发给另一个进程。
		// 先问事实，再看标签：meta 里的 state 只是上一次 daemon 的判断，
		// 进程可能照旧在跑（本机实测：应用被判"启动超时 failed"，daemon 重启时
		// 8081 仍在监听）。按标签归档成已结束，等于当场忘掉一个活服务。
		alive := info.PID != 0 && netx.PidSame(info.PID, info.PidCreated)
		d := decision{path: path, info: info, adopt: alive}
		if alive {
			if info.Port == 0 {
				// 接管进来的运行没有日志管道可查，端口是外界唯一能确认"它还在服务"的证据：
				// 直接按 PID 反查监听端口补上，否则 list 只会显示一个空列。
				if ports := netx.ListeningPortOf([]int{info.PID}); len(ports) > 0 {
					info.Port = ports[0]
					d.portFound = true
				}
			}
		} else if info.State.Active() && info.EndedAt.IsZero() {
			info.EndedAt = time.Now()
		}
		decisions = append(decisions, d)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range decisions {
		info := d.info
		if d.adopt {
			if _, exists := m.active[info.ProjectID]; exists {
				continue
			}
			info.State = StateOrphan
			info.Reason = "由上一次 daemon 拉起，进程仍在（pid " + strconv.Itoa(info.PID) + "）；日志管道已随旧进程丢失，只能强制结束"
			if d.portFound {
				info.Reason += "；实测监听端口 " + strconv.Itoa(info.Port)
			}
			r := &Run{RunInfo: info} // log/cmd/doneCh 全空：这是一次只能停、不能看日志的接管
			m.active[info.ProjectID] = r
			m.pushHistoryLocked(r)
			_ = os.WriteFile(d.path, mustJSON(info), 0o600)
			adopted++
			continue
		}
		if info.State.Active() {
			info.State = StateZombie
			info.Reason = "daemon 重启后发现进程已不存在，无法确认退出码"
			_ = os.WriteFile(d.path, mustJSON(info), 0o600)
			m.pushHistoryLocked(&Run{RunInfo: info})
			cleared++
			continue
		}
		// 已结束的旧运行也要进历史。不然 daemon 重启后 list/status 只能说"停止"，
		// 把上一次 failed、zombie 这些结论全丢了——那是产品最该留下的信息。
		m.pushHistoryLocked(&Run{RunInfo: info})
	}
	return adopted, cleared
}

// pushHistoryLocked 把一次运行放到该项目历史的最前面，只留最近 20 次。
func (m *Manager) pushHistoryLocked(r *Run) {
	m.history[r.ProjectID] = append([]*Run{r}, m.history[r.ProjectID]...)
	if len(m.history[r.ProjectID]) > 20 {
		m.history[r.ProjectID] = m.history[r.ProjectID][:20]
	}
}

func mustJSON(v any) []byte {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func (m *Manager) reapLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-t.C:
			m.Prune()
			// 已结束且空闲的日志退回磁盘：2000 行 × 几十个历史运行是 daemon 自身堆内存
			// 的大头，不按期释放，一个 daemon 跑一整天内存就水涨船高（"接近 IDEA"的那
			// 部分正是这里，而不是那个 Go 二进制本身的固定开销）。
			m.bus.ReleaseIdle(60 * time.Second)
		}
	}
}

func prependPath(dir, path string) string {
	if dir == "" {
		return path
	}
	if path == "" {
		return dir
	}
	return dir + string(os.PathListSeparator) + path
}

func sortRuns(rs []*Run) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].StartedAt.After(rs[j-1].StartedAt); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func pump(r io.Reader, lg *logs.Log, isErr bool) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		lg.Append(isErr, sc.Text())
	}
}

// buildArgv 把 LaunchConfig 翻译成 argv，不经过 shell（除非目标本身就是 .bat/.cmd，
// 那种文件在 Windows 上无法直接 CreateProcess，只能用 cmd.exe /c 包一层）。
func buildArgv(kind project.Kind, cfg *project.LaunchConfig, dir string, extra []string) (string, []string, error) {
	task := strings.TrimSpace(cfg.Task)
	if task == "" {
		return "", nil, fmt.Errorf("启动配置 %s 没有 task", cfg.ID)
	}
	// direct-main 配置要走 prepareDirectMain（解析 classpath + 写 argfile），
	// buildArgv 是纯命令行拼接，不碰 I/O，也不该把它当 mvn/exec 去猜。
	if cfg.MainClass != "" {
		return "", nil, fmt.Errorf("direct-main 配置（main class %s）需先经 prepareDirectMain 解析", cfg.MainClass)
	}
	// 任务本身是 jar 启动（task 以 -jar 开头）时，无论项目声明成 maven 还是 gradle，
	// 执行体都是 java——maven 项目上挂着 detect 生成的 "java-jar" 配置就是这种情况，
	// 之前 buildArgv 只看项目 kind 挑启动器，把 java -jar 跑成了 mvn -jar，直接报
	// "Unrecognized option: -jar"（本机 lgsy-app 实测）。
	if strings.HasPrefix(task, "-jar ") {
		args := strings.Fields(task)
		if cfg.Profile != "" {
			args = append(args, "--spring.profiles.active="+cfg.Profile)
		}
		args = append(args, extra...)
		return "java", args, nil
	}
	var name string
	var args []string
	switch kind {
	case project.KindMaven:
		name, args = "mvn", nil
	case project.KindGradle:
		name, args = gradleLauncher(dir), nil
	case project.KindJar:
		name, args = "java", nil
	case project.KindNode:
		name, args = nodeLauncher(dir, task), nil
	case project.KindExec:
		parts := splitArgs(task)
		if len(parts) == 0 {
			return "", nil, errors.New("exec 配置的 task 为空")
		}
		name, args = parts[0], parts[1:]
		if cfg.Profile != "" {
			args = injectProfile(kind, cfg, args)
		}
		args = append(args, extra...)
		return name, args, nil
	default:
		return "", nil, fmt.Errorf("未知项目类型 %s", kind)
	}
	// 结构化类型（maven/gradle/jar/node）：cfg.Args 是工具选项，放在任务词之前，
	// 与 detect 生成的 label（如 "mvn -pl <模块> spring-boot:run"）一致。
	// 之前只取 task 忽略 Args，多模块 reactor 的 -pl 被丢掉，spring-boot:run
	// 会跑去根 pom 找 main class 而失败——本机 lgsy-app 实测就是这个错。
	if len(cfg.Args) > 0 {
		args = append(args, cfg.Args...)
	}
	args = append(args, strings.Fields(task)...)
	// 单 JVM：spring-boot:run 默认 fork 一个独立 JVM 跑应用，于是同时存在 Maven 的堆和
	// 应用的堆两块（一个 Spring Boot 项目吃掉 1G+，正是"接近 IDEA"的来源之一）。IDEA 的做法
	// 是只在一个 JVM 里跑 main——fork=false 让应用直接跑进 Maven 这个 JVM，一块堆、一个进程树，
	// 顺带让 KillTree 一次结束整个服务，也让 Windows 上那条 classpath 过长（error 206）不再发生。
	if singleJVM(kind, cfg, task) {
		args = append(args, "-Dspring-boot.run.fork=false")
	}
	if cfg.Profile != "" {
		args = injectProfile(kind, cfg, args)
	}
	args = append(args, extra...)
	return name, args, nil
}

// singleJVM 判定这次 maven 启动要不要加 -Dspring-boot.run.fork=false。
// 只对 spring-boot:run 生效（别的 goal 不 fork）；SingleJVM 显式 *false 就恢复 fork。
func singleJVM(kind project.Kind, cfg *project.LaunchConfig, task string) bool {
	if kind != project.KindMaven {
		return false
	}
	if !strings.Contains(task, "spring-boot:run") {
		return false
	}
	return cfg.SingleJVM == nil || *cfg.SingleJVM
}

// injectProfile：Maven/Gradle 走插件参数，jar/exec 走 Spring 命令行参数，
// Node 没有统一约定，改用 NODE_ENV。
func injectProfile(kind project.Kind, cfg *project.LaunchConfig, args []string) []string {
	switch kind {
	case project.KindMaven:
		return append(args, "-Dspring-boot.run.profiles="+cfg.Profile)
	case project.KindGradle:
		return append(args, "--args=--spring.profiles.active="+cfg.Profile)
	case project.KindJar, project.KindExec:
		return append(args, "--spring.profiles.active="+cfg.Profile)
	default:
		return args
	}
}

func gradleLauncher(dir string) string {
	if _, err := os.Stat(filepath.Join(dir, "gradlew.bat")); err == nil {
		return filepath.Join(dir, "gradlew.bat")
	}
	if _, err := os.Stat(filepath.Join(dir, "gradlew")); err == nil {
		return filepath.Join(dir, "gradlew")
	}
	return "gradle"
}

// nodeLauncher 按 lockfile 选包管理器；"node index.js" 这类 task 不加前缀。
func nodeLauncher(dir, task string) string {
	if !strings.HasPrefix(task, "run ") {
		return "node"
	}
	switch {
	case fileExists(dir, "pnpm-lock.yaml"):
		return "pnpm"
	case fileExists(dir, "yarn.lock"):
		return "yarn"
	case fileExists(dir, "bun.lockb"), fileExists(dir, "bun.lock"):
		return "bun"
	default:
		return "npm"
	}
}

func fileExists(dir, name string) bool {
	st, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !st.IsDir()
}

func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t':
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func buildEnv(kind project.Kind, cfg *project.LaunchConfig, rep *env.Report) []string {
	e := os.Environ()
	if cfg.Env != nil {
		for k, v := range cfg.Env {
			e = setEnv(e, k, v)
		}
	}
	if cfg.Profile != "" && kind == project.KindNode {
		e = setEnv(e, "NODE_ENV", cfg.Profile)
	}
	jdk := cfg.JdkPath
	if jdk == "" && rep != nil {
		jdk = rep.JavaHome()
	}
	if jdk != "" {
		e = setEnv(e, "JAVA_HOME", jdk)
		e = setEnv(e, "PATH", prependPath(binDir(jdk), valueEnv(e, "PATH")))
	}
	if kind == project.KindMaven {
		e = setEnv(e, "MAVEN_BATCH_ECHO", "off")
		e = setEnv(e, "MAVEN_TERMINATE_CMD", "on")
	}
	// 中文 Windows 上 JVM 往管道里写的是 GBK，runlite 按 UTF-8 存盘/渲染就会花屏
	// （javac 的中文报错首当其冲）。输出既然一定进管道，就显式要求 UTF-8。
	// exec 类项目常常就是一条 java 命令，所以只排除 Node；非 JVM 工具会无视这个变量。
	if kind != project.KindNode {
		e = setEnv(e, "JAVA_TOOL_OPTIONS",
			"-Dsun.stdout.encoding=UTF-8 -Dsun.stderr.encoding=UTF-8 -Dstdout.encoding=UTF-8 -Dstderr.encoding=UTF-8")
	}
	return e
}

func binDir(jdk string) string {
	if st, err := os.Stat(filepath.Join(jdk, "bin")); err == nil && st.IsDir() {
		return filepath.Join(jdk, "bin")
	}
	return jdk
}

func setEnv(env []string, k, v string) []string {
	prefix := k + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) || strings.HasPrefix(strings.ToUpper(e), strings.ToUpper(prefix)) {
			env[i] = prefix + v
			return env
		}
	}
	return append(env, prefix+v)
}

func valueEnv(env []string, k string) string {
	for _, e := range env {
		if len(e) > len(k) && strings.EqualFold(e[:len(k)+1], k+"=") {
			return e[len(k)+1:]
		}
	}
	return ""
}

func envSummary(env []string, kind project.Kind) []string {
	var out []string
	if kind != project.KindNode {
		if j := valueEnv(env, "JAVA_HOME"); j != "" {
			out = append(out, "JAVA_HOME="+j)
		}
	}
	if p := valueEnv(env, "NODE_ENV"); p != "" {
		out = append(out, "NODE_ENV="+p)
	}
	if len(out) == 0 {
		out = append(out, "使用系统默认环境")
	}
	return out
}

var sanitizeRepl = []string{"/", "_", "\\", "_", ":", "_"}

func sanitize(s string) string {
	for i := 0; i < len(sanitizeRepl); i += 2 {
		s = strings.ReplaceAll(s, sanitizeRepl[i], sanitizeRepl[i+1])
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// Package service 把注册表、检测、启停引擎粘成一个可被 HTTP/CLI/MCP 共用的门面。
package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"runlite/internal/config"
	"runlite/internal/detect"
	"runlite/internal/env"
	"runlite/internal/logs"
	"runlite/internal/netx"
	"runlite/internal/proc"
	"runlite/internal/project"
)

type Service struct {
	paths *config.Paths
	reg   *project.Registry
	mgr   *proc.Manager
	set   *config.Settings

	mu       sync.Mutex
	report   *env.Report
	reportAt time.Time
}

func New(paths *config.Paths) (*Service, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	set, err := config.LoadSettings(paths.Settings)
	if err != nil {
		return nil, err
	}
	reg, err := project.LoadRegistry(paths.Projects)
	if err != nil {
		return nil, err
	}
	s := &Service{paths: paths, reg: reg, set: set}
	s.mgr = proc.NewManager(proc.Options{
		RunsDir:        paths.Runs,
		RingSize:       set.RingSize,
		StartTimeout:   duration(set.StartTimeout, 120*time.Second),
		DefaultGrace:   duration(set.StopGrace, 15*time.Second),
		ReportProvider: func() *env.Report { return s.Report() },
	})
	return s, nil
}

func duration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

func (s *Service) Close() { s.mgr.Close() }

func (s *Service) Report() *env.Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.report == nil || time.Since(s.reportAt) > 5*time.Minute {
		s.report = env.Detect(s.set.JdkPaths)
		s.reportAt = time.Now()
	}
	return s.report
}

func (s *Service) InvalidateReport() {
	s.mu.Lock()
	s.report = nil
	s.mu.Unlock()
}

func (s *Service) Projects() []*project.Project { return s.reg.Snapshot() }

func (s *Service) Resolve(key string) (*project.Project, error) { return s.reg.Resolve(key) }

type AddRequest struct {
	Dir     string
	Name    string
	Cmd     string // 指定则按 exec 类型注册，绕过自动检测
	Profile string
	Port    int
}

func (s *Service) Add(req AddRequest) (*project.Project, error) {
	// daemon 的 cwd 是 ~/.runlite，相对路径在这里已经失去意义，必须由客户端先展开
	if !filepath.IsAbs(req.Dir) {
		return nil, fmt.Errorf("目录必须是绝对路径（收到 %q）", req.Dir)
	}
	dir, err := project.NormalizeDir(req.Dir)
	if err != nil {
		return nil, err
	}
	var p *project.Project
	if req.Cmd != "" {
		p = &project.Project{
			Name: firstNonEmpty(req.Name, filepath.Base(dir)),
			Dir:  filepath.ToSlash(dir),
			Kind: project.KindExec,
			Configs: []project.LaunchConfig{{
				ID: "exec", Label: req.Cmd, Task: req.Cmd, Port: req.Port, Profile: req.Profile,
			}},
			Active: "exec",
			Notes:  []string{"手工指定的命令行项目，未做自动检测"},
		}
	} else {
		res, err := detect.Detect(dir)
		if err != nil {
			return nil, err
		}
		p = res.Project
		if req.Name != "" {
			p.Name = req.Name
		}
		if req.Port > 0 {
			for i := range p.Configs {
				p.Configs[i].Port = req.Port
			}
		}
		if req.Profile != "" {
			for i := range p.Configs {
				p.Configs[i].Profile = req.Profile
			}
		}
	}
	p.ID = project.IDFor(dir)
	p.AddedAt = time.Now()
	if err := s.reg.Put(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *Service) Remove(key string) error {
	p, err := s.reg.Resolve(key)
	if err != nil {
		return err
	}
	if r, ok := s.mgr.Get(p.ID); ok && r.State.Active() {
		return fmt.Errorf("项目 %s 正在运行，先 runlite stop %s 再移除", p.Name, p.Name)
	}
	if _, err := s.reg.Remove(p.ID); err != nil {
		return err
	}
	s.mgr.Remove(p.ID)
	return nil
}

type StartRequest struct {
	Key    string
	Config string
	Args   []string
}

func (s *Service) Start(req StartRequest) (*proc.Run, error) {
	p, err := s.reg.Resolve(req.Key)
	if err != nil {
		return nil, err
	}
	cfg, err := pickConfig(p, req.Config)
	if err != nil {
		return nil, err
	}
	if cfg.Port > 0 && netx.PortBusy(cfg.Port) {
		owners := netx.PIDsOnPort(cfg.Port)
		return nil, fmt.Errorf("端口 %d 已被 PID %v 占用；先停掉占用者，或改配置里的 port（runlite 不会自动换端口，Spring Boot 依赖固定端口调前端）", cfg.Port, owners)
	}
	return s.mgr.Start(proc.StartSpec{Project: p, Config: cfg, Args: req.Args})
}

func pickConfig(p *project.Project, id string) (*project.LaunchConfig, error) {
	if id == "" {
		return p.ActiveConfig()
	}
	return p.Config(id)
}

func (s *Service) Stop(key string, force bool) (*proc.Run, error) {
	p, err := s.reg.Resolve(key)
	if err != nil {
		return nil, err
	}
	r, err := s.mgr.Stop(p.ID, force)
	if !errors.Is(err, proc.ErrNotRunning) {
		return r, err
	}
	// "没在跑"不当失败：stop 在脚本里的语义是"确保停下来"，重复执行应该成功并说清
	// 这次没做事。本机实测连按两次 stop 第二次 rc=1，看起来像上一条命令出了错。
	return s.stoppedRecord(p), nil
}

// stoppedRecord 造一条"本来就没在运行"的说明：有历史就带上上一次的结局，没有就直说没记录。
func (s *Service) stoppedRecord(p *project.Project) *proc.Run {
	if last, ok := s.mgr.Latest(p.ID); ok {
		last.NoOp = true
		last.Reason = "本来就没有在运行（" + lastSummary(last) + "）"
		return last
	}
	return &proc.Run{RunInfo: proc.RunInfo{ProjectID: p.ID, Name: p.Name,
		State: proc.StateStopped, NoOp: true, Reason: "本来就没有在运行（没有运行记录）"}}
}

func lastSummary(r *proc.Run) string {
	if r.EndedAt.IsZero() {
		return "上次记录 " + string(r.State)
	}
	return fmt.Sprintf("上次 %s，%s 结束，运行 %s",
		r.State, r.EndedAt.Local().Format("15:04:05"), r.Uptime().Round(time.Second))
}

func (s *Service) Restart(req StartRequest) (*proc.Run, error) {
	p, err := s.reg.Resolve(req.Key)
	if err != nil {
		return nil, err
	}
	cfg, err := pickConfig(p, req.Config)
	if err != nil {
		return nil, err
	}
	return s.mgr.Restart(proc.StartSpec{Project: p, Config: cfg, Args: req.Args})
}

func (s *Service) Status(key string) (*project.Project, *proc.Run, error) {
	p, err := s.reg.Resolve(key)
	if err != nil {
		return nil, nil, err
	}
	r, _ := s.mgr.Get(p.ID)
	return p, r, nil
}

// RunByID 供 API 组装视图时使用（调用方已经拿到项目 ID，不必再解析名称）。
func (s *Service) RunByID(projectID string) (*proc.Run, bool) { return s.mgr.Get(projectID) }

// LastRun 是该项目最近一次运行，包含已经结束的。
func (s *Service) LastRun(projectID string) (*proc.Run, bool) { return s.mgr.Latest(projectID) }

func (s *Service) Runs() []*proc.Run { return s.mgr.List() }

func (s *Service) Prune() int { return s.mgr.Prune() }

// Reclaim 在 daemon 启动时接管上一次遗留、仍在运行的项目进程。
func (s *Service) Reclaim() (int, int) { return s.mgr.Reclaim() }

// Logs 返回某项目最近一次运行的日志尾部；grep 为可选的正则过滤。
func (s *Service) Logs(key string, n int, since int, grep string) (*proc.Run, []logs.Line, error) {
	p, err := s.reg.Resolve(key)
	if err != nil {
		return nil, nil, err
	}
	r, ok := s.mgr.Get(p.ID)
	if !ok {
		r, ok = s.mgr.Latest(p.ID)
	}
	if !ok {
		return nil, nil, fmt.Errorf("项目 %s 还没有运行记录", p.Name)
	}
	var out []logs.Line
	if lg, ok := s.mgr.Logs(r.ID); ok {
		out = lg.Tail(n, since)
	} else if r.LogPath != "" {
		// daemon 重启后内存里没有这次运行的日志了，但文件一直都在：
		// 看日志是这产品最主要用途，不能让用户自己去翻 runs 目录。
		fromFile, err := logs.ReadFile(r.LogPath, n, since)
		if err != nil {
			return r, nil, fmt.Errorf("读日志文件 %s 失败: %w", r.LogPath, err)
		}
		out = fromFile
	}
	if grep != "" {
		re, err := regexp.Compile(grep)
		if err != nil {
			return r, nil, fmt.Errorf("grep 不是合法正则: %w", err)
		}
		var filtered []logs.Line
		for _, ln := range out {
			if re.MatchString(ln.Text) {
				filtered = append(filtered, ln)
			}
		}
		out = filtered
	}
	return r, out, nil
}

func (s *Service) LogPath(key string) (string, error) {
	p, err := s.reg.Resolve(key)
	if err != nil {
		return "", err
	}
	r, ok := s.mgr.Get(p.ID)
	if !ok {
		return "", fmt.Errorf("项目 %s 当前没有运行中的实例", p.Name)
	}
	return filepath.ToSlash(r.LogPath), nil
}

func (s *Service) Paths() *config.Paths { return s.paths }

func (s *Service) Executable() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return exe
}

// Home 是本 daemon 的存放目录。写进 /api/health 是为了让客户端在"端口被同行占了"时
// 能说清那是哪一个主目录的 daemon，而不是让人对着 15 秒超时发现场猜。
func (s *Service) Home() string { return s.paths.Home }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

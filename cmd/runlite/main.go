// Command runlite 是本机项目启停工具。
//
// 结构上 daemon 是唯一事实源：GUI、CLI、MCP 都只是它的 HTTP 客户端，
// 这样 CLI 退出后项目继续跑，三个面看到的永远是同一批状态。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/daemon"
	"runlite/internal/env"
	"runlite/internal/mcp"
	"runlite/internal/proc"
	"runlite/internal/service"
)

const usage = `runlite — 把本地 Java / Spring Boot / Node 项目跑起来、停下来、看日志

用法:
  runlite add <目录> [--name n] [--cmd "..."] [--profile p] [--port 8080]
  runlite list
  runlite start <项目> [-c 配置] [-wait] [-- 追加参数]
  runlite stop <项目> [-force]
  runlite restart <项目> [-c 配置]
  runlite status [<项目>] [-json]
  runlite logs <项目> [-n 200] [-f] [-grep 正则]
  runlite remove <项目>
  runlite doctor [-refresh] [-json]
  runlite daemon <start|stop|status|exec>
  runlite version
  runlite mcp

项目可用 id、id 前缀、名称或所在目录指定；名称不唯一时会要求改用 id。
runlite mcp 以 MCP（JSON-RPC over stdio）对外提供同样的动作，供 Agent 接入；
它自己不留状态，所有动作仍然转给 daemon。
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		fmt.Print(usage)
		return nil
	}
	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "help", "--help", "-h":
		fmt.Print(usage)
	case "add":
		return cmdAdd(rest)
	case "list", "ls":
		return cmdList(rest)
	case "start":
		return cmdStart(rest)
	case "stop":
		return cmdStop(rest)
	case "restart":
		return cmdRestart(rest)
	case "status":
		return cmdStatus(rest)
	case "logs":
		return cmdLogs(rest)
	case "remove", "rm":
		return cmdRemove(rest)
	case "doctor":
		return cmdDoctor(rest)
	case "daemon":
		return cmdDaemon(rest)
	case "version":
		fmt.Println("runlite " + api.Version)
	case "mcp":
		return cmdMCP(rest)
	default:
		return fmt.Errorf("未知命令 %q\n\n%s", cmd, usage)
	}
	return nil
}

// client 拿到可用的 daemon 客户端，必要时自动拉起 daemon。
// 拉起/连接逻辑抽到 internal/daemon，CLI 传自己，GUI 传同目录的 runlite.exe。
func client(autoStart bool) (*api.Client, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return daemon.Client(exe, autoStart, daemon.Spawn)
}

func cmdAdd(argv []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	name := fs.String("name", "", "项目显示名，默认取构建文件里的 artifactId / package.json name")
	cmdStr := fs.String("cmd", "", "跳过自动检测，直接指定启动命令行（注册为 exec 类型项目）")
	profile := fs.String("profile", "", "spring.profiles.active / NODE_ENV")
	port := fs.Int("port", 0, "覆盖自动探测到的端口")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return errors.New("缺少目录：runlite add <目录>")
	}
	// 必须在客户端展开：daemon 的 cwd 是 ~/.runlite，把 "." 原样发过去会解析到错误目录。
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	view, err := c.Add(service.AddRequest{Dir: abs, Name: *name, Cmd: *cmdStr, Profile: *profile, Port: *port})
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(view)
	}
	fmt.Printf("已添加 %s (%s)\n  id:   %s\n  目录: %s\n", view.Name, view.Kind, view.ID, view.Dir)
	for _, cfg := range view.Configs {
		mark := "  "
		if cfg.ID == view.Active {
			mark = "* "
		}
		port := ""
		if cfg.Port > 0 {
			port = fmt.Sprintf("  端口 %d", cfg.Port)
		}
		fmt.Printf("  %s%-18s %s%s\n", mark, cfg.ID, cfg.Label, port)
	}
	for _, n := range view.Notes {
		fmt.Printf("  ! %s\n", n)
	}
	return nil
}

func cmdList(argv []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "JSON 输出")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	ps, err := c.Projects()
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(ps)
	}
	if len(ps) == 0 {
		fmt.Println("还没有项目。用 runlite add <目录> 添加。")
		return nil
	}
	w := tabWriter()
	defer w.Flush()
	fmt.Fprintln(w, "状态\t项目\t类型\t端口\t运行时长\tid")
	for _, p := range ps {
		state, port, up := "停止", "-", "-"
		if p.Run != nil {
			state = string(p.Run.State)
			// 端口只在"还在跑"的时候印：stopped 行挂着一个 8081 会被读成"服务还在监听"，
			// 而它其实只是上一次记录（本机实测：用户据此以为项目没停掉，又去 stop 一次）。
			if p.Run.Port > 0 && p.Run.State.Active() {
				port = strconv.Itoa(p.Run.Port)
			}
			if p.Run.State.Active() {
				up = humanDur(p.Run.Uptime())
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", state, p.Name, p.Kind, port, up, p.ID)
	}
	return nil
}

func cmdStart(argv []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	cfgID := fs.String("c", "", "启动配置 id，默认用项目的 active 配置")
	wait := fs.Bool("wait", false, "阻塞到就绪或失败再返回")
	extra := fs.String("args", "", "追加到命令行的参数（空格分隔）")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return errors.New("缺少项目：runlite start <项目>")
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	req := service.StartRequest{Key: key, Config: *cfgID}
	if strings.TrimSpace(*extra) != "" {
		req.Args = strings.Fields(*extra)
	}
	run, err := c.Start(key, req, *wait)
	if err != nil {
		return err
	}
	fmt.Printf("已启动 %s: pid %d，状态 %s\n", run.Name, run.PID, run.State)
	if run.Port > 0 {
		fmt.Printf("  端口 %d\n", run.Port)
	}
	fmt.Printf("  日志: runlite logs %s\n", run.Name)
	if !*wait {
		fmt.Println("  （未加 -wait：状态可能仍是 starting，用 runlite status 确认）")
	}
	if run.State == proc.StateFailed {
		return errors.New("启动失败: " + run.Reason)
	}
	if run.State == proc.StateStarting && run.Reason != "" {
		// 大应用（jeecg 这类）冷启动确实会超过默认 120s：这不是失败，
		// 进程还在起。结论留给 status，不在这里替它下判决。
		fmt.Printf("  %s\n  继续观察: runlite status %s\n", run.Reason, run.Name)
	}
	return nil
}

func cmdStop(argv []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	force := fs.Bool("force", false, "跳过宽限期直接强制结束")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return errors.New("缺少项目：runlite stop <项目>")
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	run, err := c.Stop(key, *force)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s", run.Name, run.State)
	if worthShowingExitCode(run) {
		fmt.Printf("（退出码 %d）", run.ExitCode)
	}
	fmt.Println()
	if run.Reason != "" {
		fmt.Println("  " + run.Reason)
	}
	if run.State == proc.StateZombie {
		return errors.New("没有停干净，需要手工处理")
	}
	return nil
}

// worthShowingExitCode 决定 stop 的收尾行要不要带退出码。
// 只有"进程自己结束"时那个数字才有信息量：runlite 主动结束之后，它是本机强制
// 结束的副产物（实测 ping 被打成 1、java 被打成 0xFFFFFFFF），印出来像"程序崩了"；
// NoOp 那次带的是上一轮运行遗留的数字，同样不代表这次操作。
// 日志尾行早就按这个口径写了"已按请求停止"，CLI 这行不该比日志更啰嗦。
func worthShowingExitCode(r *proc.Run) bool {
	return r != nil && r.ExitCode != 0 && !r.NoOp && !r.Stopped
}

func cmdRestart(argv []string) error {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	cfgID := fs.String("c", "", "启动配置 id")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return errors.New("缺少项目：runlite restart <项目>")
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	if _, err := c.Stop(key, false); err != nil && !strings.Contains(err.Error(), "没有在运行") {
		fmt.Fprintln(os.Stderr, "  停止阶段:", err)
	}
	run, err := c.Start(key, service.StartRequest{Key: key, Config: *cfgID}, false)
	if err != nil {
		return err
	}
	fmt.Printf("已重启 %s: pid %d\n", run.Name, run.PID)
	return nil
}

func cmdStatus(argv []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "JSON 输出")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return cmdList(argv)
	}
	view, err := c.Project(key)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(view)
	}
	fmt.Printf("%s  [%s]\n  目录: %s\n  类型: %s  当前配置: %s\n", view.Name, view.ID, view.Dir, view.Kind, view.Active)
	for _, cfg := range view.Configs {
		fmt.Printf("  - %-18s %s\n", cfg.ID, cfg.Label)
	}
	if view.Run == nil {
		fmt.Println("  状态: 停止")
		return nil
	}
	r := view.Run
	if r.State.Active() {
		fmt.Printf("  状态: %s  pid %d  运行 %s\n", r.State, r.PID, humanDur(r.Uptime()))
	} else {
		fmt.Printf("  状态: %s  pid %d 已于 %s 结束（运行 %s）\n",
			r.State, r.PID, r.EndedAt.Local().Format("15:04:05"), humanDur(r.Uptime()))
	}
	if r.Port > 0 {
		if r.State.Active() {
			fmt.Printf("  端口: %d\n", r.Port)
		} else {
			fmt.Printf("  端口: %d（结束前的记录，不代表现在还在监听）\n", r.Port)
		}
	}
	fmt.Printf("  命令: %s\n", r.Cmd)
	if r.Reason != "" {
		fmt.Printf("  说明: %s\n", r.Reason)
	}
	return nil
}

func cmdLogs(argv []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	n := fs.Int("n", 200, "取最后多少行；-1 表示全部（受内存环形缓冲限制）")
	follow := fs.Bool("f", false, "持续输出新增行")
	grep := fs.String("grep", "", "按正则过滤")
	interval := fs.Duration("i", 500*time.Millisecond, "跟随模式下的轮询间隔")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return errors.New("缺少项目：runlite logs <项目>")
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	since := 0
	for {
		resp, err := c.Logs(key, *n, since, *grep)
		if err != nil {
			return err
		}
		for _, ln := range resp.Lines {
			since = ln.Seq
			prefix := ""
			if ln.Err {
				prefix = "[stderr] "
			}
			fmt.Printf("%s%s\n", prefix, ln.Text)
		}
		if !*follow {
			return nil
		}
		if resp.Run != nil && !resp.Run.State.Active() {
			fmt.Printf("— 进程已退出（%s），结束跟随 —\n", resp.Run.State)
			return nil
		}
		time.Sleep(*interval)
	}
}

func cmdRemove(argv []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	key := fs.Arg(0)
	if key == "" {
		return errors.New("缺少项目：runlite remove <项目>")
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	return c.Remove(key)
}

func cmdDoctor(argv []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	refresh := fs.Bool("refresh", false, "重新探测")
	jsonOut := fs.Bool("json", false, "JSON 输出")
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	// doctor 不依赖 daemon：没装好 daemon 时也要能诊断环境
	paths := config.For(config.DefaultHome())
	if info, err := config.ReadDaemonInfo(paths.Daemon); err == nil {
		c := api.NewClient(info)
		if err := c.Health(); err == nil {
			rep, err := c.Doctor()
			if err == nil {
				if *jsonOut {
					return printJSON(rep)
				}
				fmt.Print(rep.String())
				return nil
			}
		}
	}
	if *refresh {
		fmt.Println("（daemon 未运行，-refresh 无效果，本次直接探测）")
	}
	set, _ := config.LoadSettings(paths.Settings)
	rep := env.Detect(set.JdkPaths)
	if *jsonOut {
		return printJSON(rep)
	}
	fmt.Print(rep.String())
	return nil
}

func printJSON(v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

// parseFlags 允许把选项写在位置参数之后（`runlite add . --cmd "..."`）。
// 标准库 flag 遇到第一个非选项就停止，这里按 FlagSet 自身的定义重排：
// 布尔型不吃后续值，其余的吃掉一个值 —— 元数由定义决定，不靠猜。
func parseFlags(fs *flag.FlagSet, argv []string) error {
	var flags, positional []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" {
			positional = append(positional, argv[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(name, "=") {
			f := fs.Lookup(name)
			if f != nil && !isBoolFlag(f) && i+1 < len(argv) {
				flags = append(flags, a, argv[i+1])
				i++
				continue
			}
		}
		flags = append(flags, a)
	}
	return fs.Parse(append(flags, positional...))
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func humanDur(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// cmdMCP 让 Agent 用 MCP 直接驱动 daemon。协议走 stdin/stdout，
// 提示与故障信息一律走 stderr——stdout 里混进一行中文就够客户端解析失败一次。
func cmdMCP(argv []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	if err := parseFlags(fs, argv); err != nil {
		return err
	}
	c, err := client(true)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "runlite mcp: 已接上 daemon，等待 JSON-RPC 消息（每行一条）")
	return mcp.Serve(os.Stdin, os.Stdout, c)
}

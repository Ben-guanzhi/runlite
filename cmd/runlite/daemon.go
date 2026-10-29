package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/daemon"
	"runlite/internal/netx"
	"runlite/internal/service"
)

func tabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
}

// daemonCreated 记下本进程的创建时间，写进 daemon.json。
// 客户端拿它配合 PID 判断"那位是还在启动，还是号码已经被系统发给别的进程了"。
func daemonCreated() uint64 {
	if id, ok := netx.ProcIdent(os.Getpid()); ok {
		return id
	}
	return 0
}

func cmdDaemon(argv []string) error {
	sub := "status"
	if len(argv) > 0 {
		sub = argv[0]
		argv = argv[1:]
	}
	switch sub {
	case "exec":
		return daemonForeground(argv)
	case "start":
		return daemonStart()
	case "stop":
		return daemonStop()
	case "status":
		return daemonStatus()
	default:
		return fmt.Errorf("未知 daemon 子命令 %q（可用: start|stop|status|exec）", sub)
	}
}

// daemonForeground 是真正的 daemon 主体，由 spawnDaemon 以分离进程方式拉起。
func daemonForeground(argv []string) error {
	fs := flag.NewFlagSet("daemon exec", flag.ContinueOnError)
	logFile := fs.String("log", "", "把 daemon 自身输出写到该文件")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	// 分离进程没有控制台，标准输出只能靠 spawnDaemon 重定向；
	// -log 留给手工前台运行 daemon 时使用。
	if *logFile != "" {
		if f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			os.Stdout = f
			os.Stderr = f
		}
	}
	paths := config.For(config.DefaultHome())
	svc, err := service.New(paths)
	if err != nil {
		return err
	}
	defer svc.Close()
	token := api.NewToken()
	srv := api.New(svc, token)
	port, err := srv.Listen(daemon.PreferredPort())
	if err != nil {
		if errors.Is(err, api.ErrDaemonExists) {
			// 已有 daemon 在首选端口上：安静退出，不写 daemon.json、不留第二个事实源。
			// 拉起我的那个客户端会轮询到已存在 daemon 的记录，不会误以为启动失败。
			fmt.Printf("runlite daemon 未启动：%v\n", err)
			return nil
		}
		return err
	}
	if err := config.WriteDaemonInfo(paths.Daemon, &config.DaemonInfo{
		PID: os.Getpid(), Port: port, Token: token, Exe: svc.Executable(),
		Started: time.Now().Format(time.RFC3339), Created: daemonCreated(),
	}); err != nil {
		return err
	}
	adopted, cleared := svc.Reclaim()
	pruned := svc.Prune()
	fmt.Printf("runlite daemon pid=%d port=%d 接管遗留 %d 条，修账 %d 条，清理 %d 条\n", os.Getpid(), port, adopted, cleared, pruned)
	srv.OnShutdown(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx) // 让 Serve() 以 ErrServerClosed 返回，defer 里的 svc.Close() 只关管理器
	})
	err = srv.Serve()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func daemonStart() error {
	if c, err := client(false); err == nil {
		if err := c.Health(); err == nil {
			fmt.Println("daemon 已在运行")
			return nil
		}
	}
	// 只走 client(true) 这一条判定：它自己决定要不要 spawn，且最多一次。
	// 这里再手动 spawn 一次，就会和它拉的那个撞成两个 daemon。
	if _, err := client(true); err != nil {
		return err
	}
	fmt.Println("daemon 已启动")
	return nil
}

func daemonStop() error {
	paths := config.For(config.DefaultHome())
	info, err := config.ReadDaemonInfo(paths.Daemon)
	if err != nil {
		fmt.Println("daemon 没有在运行")
		return nil
	}
	c := api.NewClient(info)
	if err := c.Shutdown(); err != nil {
		return fmt.Errorf("请求 daemon 退出失败: %w（pid %d 可能仍在运行，需要手工结束）", err, info.PID)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := c.Health(); err != nil {
			_ = os.Remove(paths.Daemon)
			fmt.Println("daemon 已退出（被管理的项目进程不受影响，仍在运行）")
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return errors.New("daemon 5s 内未退出")
}

func daemonStatus() error {
	paths := config.For(config.DefaultHome())
	info, err := config.ReadDaemonInfo(paths.Daemon)
	if err != nil {
		fmt.Println("daemon 没有在运行")
		return nil
	}
	c := api.NewClient(info)
	if err := c.Health(); err != nil {
		fmt.Printf("daemon.json 指向 pid %d 端口 %d，但连不上（残留文件，下次调用会自动重新拉起）\n", info.PID, info.Port)
		return nil
	}
	ps, err := c.Projects()
	if err != nil {
		return err
	}
	running := 0
	for _, p := range ps {
		if p.Run != nil && p.Run.State.Active() {
			running++
		}
	}
	fmt.Printf("daemon 运行中: pid %d, 端口 %d, 项目 %d 个（在跑 %d 个）\n", info.PID, info.Port, len(ps), running)
	return nil
}

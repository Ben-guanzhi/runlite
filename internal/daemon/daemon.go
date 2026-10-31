// Package daemon 提供"拉起并连接 daemon"的共用逻辑，供 CLI 与 GUI 复用。
//
// 关键区别：GUI 自己的 exe 不是 daemon（runlite-gui.exe 没有 daemon exec 子命令），
// 所以它必须拿同目录（或 PATH 上）的 runlite.exe 去拉起 daemon；CLI 则传自己。
package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"runlite/internal/api"
	"runlite/internal/config"
	"runlite/internal/netx"
)

const defaultPort = 47613

// PreferredPort 固定端口让 daemon.json 丢了也能重连；被别的程序占用时自动退到随机端口。
// RUNLITE_PORT 可覆盖（打包验收与多主目录并存都靠它绕开默认端口）。
func PreferredPort() int {
	if v := os.Getenv("RUNLITE_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 65536 {
			return n
		}
	}
	return defaultPort
}

// Spawn 以分离进程方式拉起 daemon（用 exe 参数指定二进制：CLI 传自己，GUI 传 runlite.exe）。
// 分离后不继承调用方控制台，调用方退出后 daemon 继续活着；daemon 自己写 daemon.json。
func Spawn(exe string, paths *config.Paths) error {
	logPath := filepath.Join(paths.Home, "daemon.log")
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		return err
	}
	// The daemon is long-lived by design; without a cap its log only ever
	// grew. At spawn time an oversized log rotates one generation back.
	if info, serr := os.Stat(logPath); serr == nil && info.Size() > 5<<20 {
		_ = os.Rename(logPath, logPath+".old")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	cmd := exec.Command(exe, "daemon", "exec")
	cmd.Dir = paths.Home
	decorateSpawn(cmd)
	cmd.Stdin = nil
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// Client 拿到可用 daemon 客户端；连不上且 autoStart 时用 spawn 拉起一个。
// spawn 参数可注入（GUI 测试里用 stub 确定性地走断连分支）。
func Client(exe string, autoStart bool, spawn func(string, *config.Paths) error) (*api.Client, error) {
	paths := config.For(config.DefaultHome())
	info, err := config.ReadDaemonInfo(paths.Daemon)
	if err == nil {
		c := api.NewClient(info)
		if herr := c.Health(); herr == nil {
			return c, nil
		}
	}
	if !autoStart {
		return nil, errors.New("daemon 没有运行，先执行 runlite daemon start")
	}
	if msg := foreignDaemon(paths); msg != "" {
		return nil, errors.New(msg)
	}
	// 记录里那个进程对象还活着却连不上，是它正在启动（或换了端口）——先等它，不抢先拉第二个：
	// daemon 是唯一事实源，两个 daemon 各持一份内存状态，客户端会看到互相矛盾的结果。
	// 判活必须用 PID+创建时间：PID 会被系统回收，光看号码会把"别人家的新进程"当成我们的 daemon。
	if err == nil && info != nil && netx.PidSame(info.PID, info.Created) {
		return waitReady(paths, 20*time.Second, exe, info.PID, info.Created, spawn)
	}
	if err := spawn(exe, paths); err != nil {
		return nil, err
	}
	return waitReady(paths, 15*time.Second, exe, 0, 0, spawn)
}

// waitReady 轮询 daemon.json 直到连上。rescuePID > 0 表示"我们在等一个记录里还活着、
// 但暂时连不上的 daemon"（它多半正在启动）；等待期间复查那个进程对象，一旦确认它其实
// 没了——被强杀时进程对象会滞后一会儿才消失，单次判定必然看不准——就补拉一次，且只补这一次。
func waitReady(paths *config.Paths, timeout time.Duration, exe string, rescuePID int, rescueCreated uint64, spawn func(string, *config.Paths) error) (*api.Client, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		if info, err := config.ReadDaemonInfo(paths.Daemon); err == nil {
			c := api.NewClient(info)
			if err := c.Health(); err == nil {
				return c, nil
			}
		}
		if rescuePID > 0 && !netx.PidSame(rescuePID, rescueCreated) {
			if err := spawn(exe, paths); err != nil {
				return nil, err
			}
			rescuePID = 0
		}
	}
	if msg := foreignDaemon(paths); msg != "" {
		return nil, errors.New(msg)
	}
	return nil, fmt.Errorf("%s 内 daemon 没有就绪，日志见 %s", timeout, filepath.Join(paths.Home, "daemon.log"))
}

// foreignDaemon 首选端口上如果站着"另一个主目录"的 daemon，客户端等下去也不会有结果。
// 本机实测撞过一次：一个 daemon 用 RUNLITE_HOME 指到别处先起了，另一个主目录的调用
// 只能报"超时"，既不说谁占着端口，也不说该去哪儿停。
// 返回空串表示没有冲突（端口空着，或那个 daemon 就是我这边的）。
func foreignDaemon(paths *config.Paths) string {
	port := PreferredPort()
	pid, home := api.Probe(port, 600*time.Millisecond)
	if pid == 0 || home == "" || sameHome(home, paths.Home) {
		return ""
	}
	return fmt.Sprintf("端口 %d 上的 daemon（pid %d）认的主目录是 %s，这个调用用的是 %s。\n"+
		"两边各持一份事实源，不会合并：到 %s 那边执行 runlite daemon stop，"+
		"或者把 RUNLITE_HOME 指到 %s 再操作。",
		port, pid, home, paths.Home, home, paths.Home)
}

// sameHome 比较两个主目录。RUNLITE_HOME 里正反斜杠、大小写都可能不一样，直接比字符串
// 会把同一个目录说成两个，让用户对着一个并不存在的冲突找半天。
func sameHome(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

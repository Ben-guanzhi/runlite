//go:build windows

package netx

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPidAliveSelfAndBogus(t *testing.T) {
	if !PidAlive(os.Getpid()) {
		t.Fatal("PidAlive 对自己的 PID 返回 false，进程查询有问题")
	}
	if PidAlive(999999) {
		t.Error("PidAlive 对不存在的 PID 返回 true")
	}
}

// TestPidAliveCmdWrapper 盯住真实踩过的坑：mvn 在 Windows 上是 mvn.cmd，
// Go 会经由 cmd.exe 启动它，此时记录的 PID 属于谁、cmd.exe 是否中途退出。
func TestPidAliveCmdWrapper(t *testing.T) {
	// ping -n 60 ≈ 60s，远大于下面的采样窗口，避免把"进程确实结束了"误判成 bug
	cmd := exec.Command("cmd.exe", "/d", "/c", "ping -n 60 127.0.0.1 >nul")
	cmd.SysProcAttr = hideWindow
	if err := cmd.Start(); err != nil {
		t.Skip("本机无法启动 cmd.exe:", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	for i := 0; i < 5; i++ {
		if !PidAlive(pid) {
			t.Fatalf("第 %d 次采样时 PID %d 已判为不存在，但子进程还在跑", i, pid)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestPidIdentity 盯住 PID 复用：号码本身不能证明"还是当初那个进程"。
// 本机真遇上过——daemon 被 taskkill 后，客户端只按号码判活，结果一直等那位
// 已经不存在的 daemon，新 daemon 又不敢拉，命令卡死 20s。
func TestPidIdentity(t *testing.T) {
	self := os.Getpid()
	created, ok := ProcIdent(self)
	if !ok || created == 0 {
		t.Fatalf("查不到自己的进程创建时间（ok=%v created=%d）", ok, created)
	}
	if !PidSame(self, created) {
		t.Error("PidSame 把自己认成了别的进程")
	}
	if PidSame(self, created+1) {
		t.Error("创建时间对不上仍判为同一进程：PID 复用时会被骗过")
	}
	if PidSame(999999, created) {
		t.Error("不存在的 PID 判为同一进程")
	}
	// created 为 0 表示老记录没存身份，只能退化成看号码活不活，两条都要是预期答案
	if !PidSame(self, 0) {
		t.Error("没有身份记录时，活进程应退化成 PidAlive=true")
	}
	if PidSame(999999, 0) {
		t.Error("没有身份记录时，不存在的 PID 应退化成 false")
	}
}

// TestPidAliveAfterRealExit 是 daemon 单实例判定的地基：
// 上一个 daemon 被外部 taskkill 结束后，客户端必须认得出"记录里的 pid 已经死了"，
// 否则它会以为 daemon 还在启动中，永远等下去（真实踩过：强杀 daemon 后再也起不来）。
func TestPidAliveAfterRealExit(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/d", "/c", "ping -n 30 127.0.0.1")
	cmd.SysProcAttr = hideWindow
	if err := cmd.Start(); err != nil {
		t.Skip("本机无法启动 cmd.exe:", err)
	}
	pid := cmd.Process.Pid
	created, ok := ProcIdent(pid)
	if !ok {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("刚启动的 PID %d 查不到身份", pid)
	}
	if !PidAlive(pid) {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("PID %d 刚启动就判为不存在", pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		_ = cmd.Wait()
		t.Skip("无法结束子进程:", err)
	}
	_ = cmd.Wait() // 释放 Go 自己持有的进程句柄

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !PidAlive(pid) && !PidSame(pid, created) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("PID %d 已结束并 Wait 过，PidAlive/PidSame 仍报存活", pid)
}

// TestDecodeConsole 盯住 GBK 还原。这些字节是 JDK 8 和 mvn.cmd 在中文 Windows 的
// 管道里真会写出来的东西：不转码，日志文件里就是一屏乱码。
func TestDecodeConsole(t *testing.T) {
	// 还原按系统 ANSI 代码页（CP_ACP）走：只有 ACP=936（简中）的机器上 GBK
	// 字节才解得回中文。英文区域的机器/runner 上产品行为就是 mojibake——
	// 用与下方断言语料无关的探针字节探测区域支持，不支持则跳过。
	if got := DecodeConsole("\xd6\xd0\xce\xc4"); got != "中文" {
		t.Skipf("系统 ACP 非 936，GBK 还原路径不可用（探针还原为 %q）", got)
	}
	gbk := "\xd5\xe2\xca\xc7\xd2\xbb\xcc\xf5\xc2\xeb\xc1\xf7" // GBK 的“这是一条码流”
	if got := DecodeConsole(gbk); got != "这是一条码流" {
		t.Errorf("GBK 没还原成 UTF-8：got %q want %q", got, "这是一条码流")
	}
	for _, s := range []string{"", "plain ascii 123", "已经是 UTF-8 的中文", "Started E2eServer in 1.930 seconds"} {
		if got := DecodeConsole(s); got != s {
			t.Errorf("合法输入被动过：in %q got %q", s, got)
		}
	}
}

// TestTreePIDsSeesChildren 拿操作系统自己的父子关系（Get-CimInstance）当独立期望，
// 核对工具快照真的向下走完了整棵树。只算自己的话，别人家进程此刻开的端口
// 就会被认领成"这个项目在监听"。
func TestTreePIDsSeesChildren(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/d", "/c", "ping -n 60 127.0.0.1 >nul")
	cmd.SysProcAttr = hideWindow
	if err := cmd.Start(); err != nil {
		t.Skip("本机无法启动 cmd.exe:", err)
	}
	pid := cmd.Process.Pid
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	time.Sleep(500 * time.Millisecond)
	out, err := run("powershell.exe", "-NoProfile", "-Command",
		fmt.Sprintf("Get-CimInstance Win32_Process -Filter 'ParentProcessId=%d' | ForEach-Object { $_.ProcessId }", pid))
	if err != nil {
		t.Skip("本机取不到父子关系:", err)
	}
	var kids []int
	for _, f := range strings.Fields(DecodeConsole(out)) {
		if n, err := strconv.Atoi(f); err == nil {
			kids = append(kids, n)
		}
	}
	if len(kids) == 0 {
		t.Skip("ping 还没成为子进程，没有可核对的对象")
	}
	tree := TreePIDs(pid)
	for _, k := range kids {
		if !tree[k] {
			t.Errorf("子进程 pid %d 不在 TreePIDs(%d) 里", k, pid)
		}
	}
	if tree[os.Getpid()] {
		t.Errorf("TreePIDs(%d) 把父进程（测试自己，pid %d）也算进去了：应当只向下走", pid, os.Getpid())
	}
}

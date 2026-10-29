//go:build !windows

package netx

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const isWindows = false

var hideWindow = &syscall.SysProcAttr{}

func KillTree(pid int, force bool) error {
	sig := syscall.SIGTERM
	if force {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pid, sig) // 负号 = 进程组
}

func PidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// ProcIdent 在非 Windows 上不提供进程身份：拿创建时间要读 /proc/<pid>/stat 再换算开机时刻，
// 而本工具的主战场是 Windows。返回 (0,false) 后 PidSame 自动退化成只看存活。
func ProcIdent(pid int) (uint64, bool) { return 0, false }

// PidSame 在不支持身份校验的平台上只能退回"只看 PID 在不在"。
func PidSame(pid int, created uint64) bool { return PidAlive(pid) }

var reProcNet = regexp.MustCompile(`^\s*[0-9A-Fa-f]+:\s+([0-9A-Fa-f]+):.*?\s+0A\s`)

// listenersUnix 解析 /proc/net/tcp{,6} 的 LISTEN 状态（0A），再回落到 ss。
// DecodeConsole 在非 Windows 上是直通：终端输出按约定就是 UTF-8，没有 ANSI 代码页要还原。
func DecodeConsole(s string) string { return s }

func listenersUnix() ([]Listener, error) {
	var out []Listener
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, ln := range strings.Split(string(raw), "\n")[1:] {
			m := reProcNet.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			p, err := strconv.ParseInt(m[1], 16, 32)
			if err != nil {
				continue
			}
			out = append(out, Listener{Port: int(p), Proto: "tcp"})
		}
	}
	if len(out) == 0 {
		// 没有 /proc（macOS 等）时用 ss/netstat，PID 可能拿不到
		_ = exec.Command("true").Run()
	}
	return out, nil
}

// TreePIDs 在 Linux 上读 /proc/<pid>/stat 的父进程字段做同样的 breadth-first；
// 没有 /proc 的平台（macOS 等）只能退回"只算自己"，此时端口归属会偏保守。
func TreePIDs(root int) map[int]bool {
	out := map[int]bool{root: true}
	if root <= 0 {
		return out
	}
	parents := map[int]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// comm 字段可能带空格和括号，从最后一个 ')' 之后开始切
		s := string(raw)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+1:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		parents[pid] = ppid
	}
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for pid, ppid := range parents {
			if ppid == cur && !out[pid] {
				out[pid] = true
				queue = append(queue, pid)
			}
		}
	}
	return out
}

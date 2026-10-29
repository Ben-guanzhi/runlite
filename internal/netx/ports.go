// Package netx 提供端口占用与进程归属查询（不依赖外部工具，Windows 走 netstat）。
package netx

import (
	"net"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Listener struct {
	Port  int
	Proto string // tcp | tcp6
	Addr  string
	PID   int
	Name  string
}

var reWindowsProto = regexp.MustCompile(`(?i)^(tcp4|tcp6|udp4|udp6)\s+(\S+)\s+(\S+)\s+(\S+)\s+(\d+)`)

// Listeners 返回当前处于 LISTENING（或 Windows UDP 的无状态条目）的 TCP 端口及属主 PID。
func Listeners() ([]Listener, error) {
	if !isWindows {
		return listenersUnix()
	}
	out, err := run("netstat", "-ano", "-p", "tcp")
	if err != nil {
		return nil, err
	}
	return parseNetstatTCP(out), nil
}

// parseNetstatTCP 解析 `netstat -ano -p tcp` 的输出，只保留 LISTENING 行。
func parseNetstatTCP(out string) []Listener {
	var res []Listener
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(strings.ReplaceAll(ln, "\r", ""))
		if !strings.HasPrefix(ln, "TCP") {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) < 5 {
			continue
		}
		if strings.ToUpper(fields[3]) != "LISTENING" {
			continue
		}
		// 列序：TCP | 本地地址 | 远端地址 | 状态 | PID —— 端口只能取本地地址，
		// 取 fields[2] 会拿到远端（LISTENING 时恒为 0.0.0.0:0），于是永远解析不出监听端口。
		port, ok := portOf(fields[1])
		if !ok {
			continue
		}
		pid, _ := strconv.Atoi(fields[len(fields)-1])
		res = append(res, Listener{Port: port, Proto: "tcp", Addr: fields[1], PID: pid})
	}
	sort.Slice(res, func(i, j int) bool {
		if res[i].Port != res[j].Port {
			return res[i].Port < res[j].Port
		}
		return res[i].PID < res[j].PID
	})
	return res
}

func portOf(addr string) (int, bool) {
	// 形如 0.0.0.0:8080 / [::]:8080 / 127.0.0.1:54321
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return 0, false
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// PIDsOnPort 返回监听指定端口的进程 PID（去重）。
func PIDsOnPort(port int) []int {
	ls, err := Listeners()
	if err != nil {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, l := range ls {
		if l.Port == port && !seen[l.PID] {
			seen[l.PID] = true
			out = append(out, l.PID)
		}
	}
	return out
}

// PortBusy 用一次 TCP 握手判断端口是否已被占用（比扫 netstat 便宜，适合启动前预检）。
func PortBusy(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// ListeningPortOf 在给定 PID 集合里找出它们监听的端口（升序、去重）。
func ListeningPortOf(pids []int) []int {
	ls, err := Listeners()
	if err != nil {
		return nil
	}
	want := map[int]bool{}
	for _, p := range pids {
		want[p] = true
	}
	seen := map[int]bool{}
	var out []int
	for _, l := range ls {
		if want[l.PID] && !seen[l.Port] {
			seen[l.Port] = true
			out = append(out, l.Port)
		}
	}
	sort.Ints(out)
	return out
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = hideWindow
	out, err := cmd.Output()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

// SnapshotPorts 返回当前 LISTEN 端口及监听它的 pid，用于启动前后做差集定位真实监听端口。
// 差集本身不说明端口属于谁，所以认领前要拿 TreePIDs（内核快照，不起外部进程）核对归属。
func SnapshotPorts() map[int]int { // port -> pid
	ls, err := Listeners()
	if err != nil {
		return map[int]int{}
	}
	m := map[int]int{}
	for _, l := range ls {
		if _, ok := m[l.Port]; !ok {
			m[l.Port] = l.PID
		}
	}
	return m
}

// NewPorts 返回 after 相对 before 新增的监听端口（升序）。
func NewPorts(before, after map[int]int) []int {
	var out []int
	for p := range after {
		if _, ok := before[p]; !ok {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	return out
}

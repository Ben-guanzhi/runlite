// Package main 是 runlite 的桌面端。
//
// 它和 CLI、MCP 一样，只是 daemon 的又一个客户端：界面里不保存任何项目事实，
// 每一行文本都是从 /api 读回来再拼的。所以这个文件所在的模块单独有 go.mod
// （module runlite/gui + replace runlite => ../），为的是把 Fyne 这一坨依赖
// 挡在零依赖的 CLI 之外——CLI 那条路仍然 CGO-free、clone 下来就能 build。
package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"runlite/internal/api"
	"runlite/internal/logs"
	"runlite/internal/proc"
)

// rowText 是左侧列表里的一行：名字 + 类型 + 最近一次运行的现状。
func rowText(p api.ProjectView) string {
	s := p.Name + "  [" + p.Kind + "]"
	if p.Run == nil {
		return s + "  未启动"
	}
	return s + "  " + runSummary(p.Run)
}

// runSummary 把一次运行写成状态栏那样的一句话。
//
// 端口只在活动状态下出现：把"结束前的记录"和"现在还在监听"混在同一行，
// 用户会拿着一个早就不存在的端口去排查（PLAN §14.1 实测踩过）。
// 同理退出码只在"进程自己结束"时才有意义，见 showExitCode 的口径。
func runSummary(r *proc.Run) string {
	if r == nil {
		return "未启动"
	}
	var b strings.Builder
	b.WriteString(string(r.State))
	if r.PID > 0 && r.State.Active() {
		fmt.Fprintf(&b, "  pid %d", r.PID)
	}
	if r.Port > 0 && r.State.Active() {
		fmt.Fprintf(&b, "  端口 %d", r.Port)
	}
	if r.State.Active() {
		fmt.Fprintf(&b, "  已运行 %s", durCompact(r.Uptime()))
	} else if !r.EndedAt.IsZero() {
		fmt.Fprintf(&b, "  结束于 %s（跑了 %s）", r.EndedAt.Format("15:04:05"), durCompact(r.Uptime()))
		if showExitCode(r) {
			fmt.Fprintf(&b, "，退出码 %d", r.ExitCode)
		}
	}
	if r.State == proc.StateOrphan {
		b.WriteString("  日志接管不了，只能按 pid 结束")
	}
	if r.State == proc.StateZombie {
		b.WriteString("  进程已不在，runlite 没等到结论")
	}
	return b.String()
}

// showExitCode 与 cmd/runlite 的 worthShowingExitCode 同口径：runlite 主动结束
// 进程后那个非 0 码是 kill 的副产物（实测 ping 被打成 1），NoOp 那次带的是上一轮
// 遗留的数字，两者印出来都像"程序崩了"。
func showExitCode(r *proc.Run) bool {
	return r != nil && r.ExitCode != 0 && !r.NoOp && !r.Stopped
}

// detailText 是右上方那块详情：目录、配置清单、检测给出的提醒、最近一次运行的账。
func detailText(p api.ProjectView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", p.Name)
	fmt.Fprintf(&b, "目录: %s\n", p.Dir)
	fmt.Fprintf(&b, "类型: %s   当前配置: %s\n", p.Kind, orDash(p.Active))
	for _, cfg := range p.Configs {
		mark := " "
		if cfg.ID == p.Active {
			mark = "*"
		}
		fmt.Fprintf(&b, "  %s %-18s %s\n", mark, cfg.ID, cfg.Label)
	}
	for _, n := range p.Notes {
		fmt.Fprintf(&b, "! %s\n", n)
	}
	if p.Run != nil {
		r := p.Run
		fmt.Fprintf(&b, "\n最近一次运行: %s\n", runSummary(r))
		if r.Cmd != "" {
			fmt.Fprintf(&b, "命令: %s\n", r.Cmd)
		}
		if !r.StartedAt.IsZero() {
			fmt.Fprintf(&b, "启动于 %s\n", r.StartedAt.Format("2006-01-02 15:04:05"))
		}
		if r.Reason != "" {
			fmt.Fprintf(&b, "原因: %s\n", r.Reason)
		}
		if r.LogPath != "" {
			fmt.Fprintf(&b, "日志: %s\n", r.LogPath)
		}
	} else {
		fmt.Fprintf(&b, "\n最近一次运行: 未启动\n")
	}
	return b.String()
}

// logText 把日志行拼成 TextGrid 的一块内容。
// stderr 行前面加 "! "：Spring Boot 的异常栈走的是 stderr，光看文本分不出来。
func logText(lines []logs.Line) string {
	var b strings.Builder
	for _, l := range lines {
		mark := " "
		if l.Err {
			mark = "! "
		}
		ts := l.TS
		if ts == "" {
			ts = "--:--:--"
		}
		fmt.Fprintf(&b, "%s %s%s\n", ts, mark, l.Text)
	}
	return b.String()
}

// advanceCursor 取本批行里最大的 Seq，作为下一次 since 的游标。
// 必须单调不回退：环形缓冲区满了以后会淘汰最老的行，若拿"最后一行的 Seq"以外
// 的任何东西当游标，重启 daemon 或换缓冲区大小都可能把老行重新当成新行灌进来。
func advanceCursor(lines []logs.Line, cur int) int {
	for _, l := range lines {
		if l.Seq > cur {
			cur = l.Seq
		}
	}
	return cur
}

// trimLogWindow 把日志滑窗压回字节预算内：从窗口头丢最老的行，直到剩余文本总量不超过
// maxLogBytes。只按行数封顶挡不住"一行几百 KB"的 dump，也挡不住 TextGrid 每次整窗
// 重绘把全部字形送进 Fyne 渲染缓存——字节预算才是内存的硬上限。
// 顺带留一个行数上限，防止上千行都是空行/单字符这种"字节小但行数爆炸"的输入。
func trimLogWindow(lines *[]logs.Line) {
	const hardLineCap = 4000
	ls := *lines
	if len(ls) > hardLineCap {
		ls = append([]logs.Line(nil), ls[len(ls)-hardLineCap:]...)
	}
	// 找最大的"后缀窗口"（从某个下标到末尾）让总字节 ≤ maxLogBytes，至少保留一行。
	// 从最老到最新，把每个前缀的字节都记下来：前缀和单调递增，所以"该丢多少行"是
	// 最后一个前缀和 ≤ maxLogBytes 的边界。一趟 O(n)。
	prefix := make([]int, len(ls)+1)
	for i, l := range ls {
		prefix[i+1] = prefix[i] + len(l.Text) + len(l.TS)
	}
	total := prefix[len(ls)]
	if total <= maxLogBytes {
		*lines = ls
		return
	}
	// 保留一个后缀，其字节 = total - prefix[k] ≤ maxLogBytes，k 尽量小（多留最新）。
	// 从最老端 k=0 往右找第一个满足 total-prefix[k] ≤ 预算的下标；k=0 就是整窗（超了），
	// 于是跳过超预算的头部，直到剩余部分放进预算。k 从 1 起找（至少丢最老一行）。
	k := 1
	for k < len(ls) && total-prefix[k] > maxLogBytes {
		k++
	}
	// 结果窗口为 ls[k:]。
	n := make([]logs.Line, len(ls)-k)
	copy(n, ls[k:])
	*lines = n
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// defaultProjectName 是"添加项目"里跟着目录自动填的名字：取目录名。
// 盘符根（D:\、/）没有"名字"可取，返回空串——否则会把 "\" 当成项目名存进去，
// 列表里那一行看起来就像坏掉了。
func defaultProjectName(dir string) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

func durCompact(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Second)
	s := int(d.Seconds())
	h := s / 3600
	m := (s / 60) % 60
	s %= 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

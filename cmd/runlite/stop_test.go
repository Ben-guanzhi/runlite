package main

import (
	"testing"

	"runlite/internal/proc"
)

// TestWorthShowingExitCode 钉住"被 runlite 自己结束的进程不该带退出码"：
// 那个数字是强制结束的副产物（本机实测 ping 被打成 1），印出来会被读成程序崩了。
// 反面对照是第一条——进程自己退出时非 0 码必须照打，否则这个门就成了哑门。
func TestWorthShowingExitCode(t *testing.T) {
	cases := []struct {
		name string
		run  *proc.Run
		want bool
	}{
		{"进程自己崩溃，退出码 2", &proc.Run{RunInfo: proc.RunInfo{ExitCode: 2}}, true},
		{"自己正常退出，退出码 0", &proc.Run{RunInfo: proc.RunInfo{ExitCode: 0}}, false},
		{"用户 stop 之后残留的 1", &proc.Run{RunInfo: proc.RunInfo{ExitCode: 1, Stopped: true}}, false},
		{"重复 stop 带的上一轮退出码", &proc.Run{RunInfo: proc.RunInfo{ExitCode: 3, NoOp: true}}, false},
		{"用户 stop 且是重复操作", &proc.Run{RunInfo: proc.RunInfo{ExitCode: 1, Stopped: true, NoOp: true}}, false},
		{"没有运行记录", nil, false},
	}
	shown := 0
	for _, c := range cases {
		if got := worthShowingExitCode(c.run); got != c.want {
			t.Errorf("%s: worthShowingExitCode = %v，期望 %v", c.name, got, c.want)
		}
		if c.want {
			shown++
		}
	}
	if shown == 0 {
		t.Fatal("没有一个用例要求显示退出码——这个门什么都没在守")
	}
}

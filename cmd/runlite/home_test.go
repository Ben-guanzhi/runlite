package main

import (
	"testing"

	"runlite/internal/api"
	"runlite/internal/daemon"
)

// TestVersionIsReported 让"产物名里的版本号"和"二进制自报的版本号"必须同源：
// build.ps1 读 api.Version 给 zip 命名，用户拿到的 exe 也必须说同一个数，
// 否则出问题时根本分不清对方跑的是哪一份。
func TestVersionIsReported(t *testing.T) {
	if api.Version == "" {
		t.Fatal("api.Version 是空串，build.ps1 会打出 runlite--windows-amd64.zip")
	}
}

// TestPreferredPortHonoursEnvOverride：多主目录并存（日常 daemon 还在 47613 上，
// 打包验收要用临时主目录再起一个）时必须能换端口，否则新 daemon 永远起不来。
func TestPreferredPortHonoursEnvOverride(t *testing.T) {
	if got := daemon.PreferredPort(); got != 47613 {
		t.Errorf("未设 RUNLITE_PORT 时首选端口 = %d，期望 %d", got, 47613)
	}
	for _, c := range []struct {
		env  string
		want int
	}{
		{"48000", 48000},
		{"1", 1},
		{"65535", 65535},
		{"0", 47613}, // 0 会绑到随机端口，写回 daemon.json 后重连语义就没了
		{"65536", 47613},
		{"-1", 47613},
		{"abc", 47613},
		{"", 47613},
	} {
		t.Setenv("RUNLITE_PORT", c.env)
		if got := daemon.PreferredPort(); got != c.want {
			t.Errorf("RUNLITE_PORT=%q → %d，期望 %d", c.env, got, c.want)
		}
	}
}

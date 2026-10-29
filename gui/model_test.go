package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"runlite/internal/api"
	"runlite/internal/logs"
	"runlite/internal/proc"
	"runlite/internal/project"
)

func TestRowTextUnstarted(t *testing.T) {
	got := rowText(api.ProjectView{Name: "lgsy-app", Kind: "maven"})
	if got != "lgsy-app  [maven]  未启动" {
		t.Fatalf("rowText = %q", got)
	}
}

// 端口/pid 只许出现在活动状态那一行：PLAN §14.1 实测过一次"把结束前的记录读成还在监听"。
func TestRunSummaryActiveVersusStoppedOwnership(t *testing.T) {
	now := time.Now()
	active := &proc.Run{RunInfo: proc.RunInfo{
		State: proc.StateRunning, PID: 4321, Port: 8080, StartedAt: now.Add(-70 * time.Second),
	}}
	got := runSummary(active)
	for _, want := range []string{"running", "pid 4321", "端口 8080"} {
		if !strings.Contains(got, want) {
			t.Errorf("active 行缺 %q，实得 %q", want, got)
		}
	}
	// 活动行的时长是现读的墙上时间，只能按窗口断言：启动于 70s 前，进位后允许 1m10s/1m11s。
	if !strings.Contains(got, "已运行 1m10s") && !strings.Contains(got, "已运行 1m11s") {
		t.Errorf("已运行时长不在预期窗口内，实得 %q", got)
	}

	done := &proc.Run{RunInfo: proc.RunInfo{
		State: proc.StateStopped, PID: 4321, Port: 8080,
		StartedAt: now.Add(-70 * time.Second), EndedAt: now,
	}}
	got = runSummary(done)
	for _, bad := range []string{"pid ", "端口 "} {
		if strings.Contains(got, bad) {
			t.Errorf("stopped 行漏出 %q：历史端口不能当作现在还在监听，实得 %q", bad, got)
		}
	}
	if !strings.Contains(got, "结束于 ") || !strings.Contains(got, "跑了 1m10s") {
		t.Errorf("stopped 行缺结束时间与时长，实得 %q", got)
	}
}

// 退出码的三档：自己崩掉要印；runlite 主动结束的 1 是 kill 的副产物；NoOp 带的是上一轮遗留。
func TestRunSummaryExitCodeGate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		run  *proc.Run
		want bool
	}{
		{"自己退出", &proc.Run{RunInfo: proc.RunInfo{State: proc.StateFailed, ExitCode: 1, StartedAt: now.Add(-time.Second), EndedAt: now}}, true},
		{"被runlite结束", &proc.Run{RunInfo: proc.RunInfo{State: proc.StateStopped, ExitCode: 1, Stopped: true, StartedAt: now.Add(-time.Second), EndedAt: now}}, false},
		{"空操作", &proc.Run{RunInfo: proc.RunInfo{State: proc.StateStopped, ExitCode: 1, NoOp: true, StartedAt: now.Add(-time.Second), EndedAt: now}}, false},
		{"码为0", &proc.Run{RunInfo: proc.RunInfo{State: proc.StateStopped, StartedAt: now.Add(-time.Second), EndedAt: now}}, false},
	}
	for _, c := range cases {
		got := runSummary(c.run)
		has := strings.Contains(got, "退出码")
		if has != c.want {
			t.Errorf("%s：是否该印退出码 = %v，实得 %q", c.name, c.want, got)
		}
		if c.want && !strings.Contains(got, "退出码 1") {
			t.Errorf("%s：退出码数字丢失，实得 %q", c.name, got)
		}
	}
}

func TestRunSummaryOrphanAndZombie(t *testing.T) {
	now := time.Now()
	orphan := runSummary(&proc.Run{RunInfo: proc.RunInfo{State: proc.StateOrphan, PID: 9, Port: 18323, StartedAt: now.Add(-time.Minute)}})
	if !strings.Contains(orphan, "端口 18323") || !strings.Contains(orphan, "只能按 pid 结束") {
		t.Errorf("orphan 行要么丢了端口要么丢了提示，实得 %q", orphan)
	}
	zombie := runSummary(&proc.Run{RunInfo: proc.RunInfo{State: proc.StateZombie, PID: 9}})
	if !strings.Contains(zombie, "进程已不在") || strings.Contains(zombie, "pid ") {
		t.Errorf("zombie 行该说『进程已不在』且不印 pid，实得 %q", zombie)
	}
}

func TestDurCompact(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{-5 * time.Second, "-"},
		{4 * time.Second, "4s"},
		{70 * time.Second, "1m10s"},
		{59 * time.Second, "59s"},
		{3661 * time.Second, "1h01m"},
	} {
		if got := durCompact(c.d); got != c.want {
			t.Errorf("durCompact(%v) = %q，想要 %q", c.d, got, c.want)
		}
	}
	// 负对照：四舍五入到秒，60s 要进位成 1m00s 而不是印成 "60s"。
	if got := durCompact(59999 * time.Millisecond); got != "1m00s" {
		t.Errorf("59.999s 该进位成 1m00s，实得 %q", got)
	}
	if got := durCompact(59400 * time.Millisecond); got != "59s" {
		t.Errorf("59.4s 舍到 59s，实得 %q", got)
	}
	if got := durCompact(60*time.Second + 400*time.Millisecond); got != "1m00s" {
		t.Errorf("60.4s 该进位成 1m00s，实得 %q", got)
	}
}

func TestDetailTextMarksActiveConfigAndNotes(t *testing.T) {
	p := api.ProjectView{
		Name: "lgsy-app",
		Dir:  `D:\liugang\lgsy-app`,
		Kind: "maven",
		Run:  nil,
		Configs: []project.LaunchConfig{
			{ID: "package", Label: "mvn package"},
			{ID: "spring-boot-run", Label: "mvn -pl ... spring-boot:run"},
		},
		Active: "spring-boot-run",
		Notes:  []string{"根 pom 只做聚合，启动模块是 lgsy-app-module/lgsy-app-start"},
	}
	got := detailText(p)
	if !strings.Contains(got, "* spring-boot-run") {
		t.Errorf("当前配置没打星，实得\n%s", got)
	}
	if strings.Contains(got, "* package") {
		t.Errorf("非当前配置也被打星，实得\n%s", got)
	}
	if !strings.Contains(got, "! 根 pom 只做聚合") {
		t.Errorf("检测提醒没进详情，实得\n%s", got)
	}
	if !strings.Contains(got, "最近一次运行") {
		t.Errorf("没有运行时也要说明，实得\n%s", got)
	}
}

func TestLogTextStderrMarkAndMissingTS(t *testing.T) {
	got := logText([]logs.Line{
		{Seq: 1, TS: "14:07:18", Text: "Tomcat started"},
		{Seq: 2, Err: true, Text: "java.net.BindException"},
	})
	if !strings.Contains(got, "14:07:18  Tomcat started") {
		t.Errorf("stdout 行格式不对，实得 %q", got)
	}
	if !strings.Contains(got, "--:--:-- ! java.net.BindException") {
		t.Errorf("stderr 行要带 ! 前缀且缺时间要补占位，实得 %q", got)
	}
	if logText(nil) != "" {
		t.Error("空批次要出空串，不能出一行占位")
	}
}

// 游标只许前进：环形缓冲淘汰老行后，回退的游标会把老行重新灌成新行。
func TestAdvanceCursorIsMonotonic(t *testing.T) {
	lines := []logs.Line{{Seq: 5}, {Seq: 9}, {Seq: 7}}
	if got := advanceCursor(lines, 0); got != 9 {
		t.Errorf("advanceCursor = %d，想要 9", got)
	}
	if got := advanceCursor(lines, 12); got != 12 {
		t.Errorf("批次里没有更大的 Seq 时游标必须不动，实得 %d", got)
	}
	if got := advanceCursor(nil, 3); got != 3 {
		t.Errorf("空批次不许重置游标，实得 %d", got)
	}
}

// 选完目录顺手填的名字取目录名；盘符根/根目录没有名字可取，必须给空串，
// 否则列表里会冒出一行名字是 "\" 的项目。
func TestDefaultProjectName(t *testing.T) {
	proj := filepath.Join("D:", "liugang", "lgsy-app")
	for _, c := range []struct{ dir, want string }{
		{proj, "lgsy-app"},
		{proj + string(filepath.Separator), "lgsy-app"},
		{proj + string(filepath.Separator) + "..", "liugang"},
		{string(filepath.Separator), ""},
		{"", ""},
		{".", ""},
	} {
		if got := defaultProjectName(c.dir); got != c.want {
			t.Errorf("defaultProjectName(%q) = %q，想要 %q", c.dir, got, c.want)
		}
	}
}

// trimLogWindow 必须按"总字节"封顶，不能只按行数：一行几百 KB 的 dump 会把窗口胀爆。
// 并且要保留最新行、丢最老行，语义和日志滚动一致。
func TestTrimLogWindowByteBudget(t *testing.T) {
	// 一行 200KB 的巨型行：5 行就 1MB，超过 512KB 预算。
	big := strings.Repeat("A", 200*1024)
	lines := []logs.Line{
		{Seq: 1, Text: big},
		{Seq: 2, Text: big},
		{Seq: 3, Text: big},
		{Seq: 4, Text: big},
		{Seq: 5, Text: big},
	}
	trimLogWindow(&lines)
	// 结果窗口必须 ≤ 预算字节，且保留最新（最大 Seq 仍在）。
	total := 0
	for _, l := range lines {
		total += len(l.Text)
	}
	if total > maxLogBytes {
		t.Fatalf("窗口总字节 %d 超过预算 %d（保留 %d 行）", total, maxLogBytes, len(lines))
	}
	if len(lines) == 0 || lines[len(lines)-1].Seq != 5 {
		t.Fatalf("最新行（Seq 5）被丢了，窗口尾巴 = %+v", lines)
	}
	// 全部在预算内的输入不动。
	small := []logs.Line{{Seq: 1, Text: "ok"}, {Seq: 2, Text: "fine"}}
	orig := len(small)
	trimLogWindow(&small)
	if len(small) != orig {
		t.Fatalf("预算内输入被误裁剪：%d -> %d 行", orig, len(small))
	}
}

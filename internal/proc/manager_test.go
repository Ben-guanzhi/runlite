package proc

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runlite/internal/netx"
	"testing"
	"time"

	"runlite/internal/project"
)

// fixture 返回仓库根 testdata 下的目录（单测的工作目录是本包目录）。
func fixture(name string) string {
	return filepath.FromSlash(filepath.Join("..", "..", "testdata", name))
}

func TestBuildArgvPerKind(t *testing.T) {
	cases := []struct {
		name string
		kind project.Kind
		cfg  project.LaunchConfig
		dir  string
		want []string
	}{
		{
			name: "maven spring-boot:run",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "c", Task: "spring-boot:run"},
			want: []string{"mvn", "spring-boot:run", "-Dspring-boot.run.fork=false"},
		},
		{
			name: "maven 多参数 task",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "c", Task: "package", Args: nil},
			want: []string{"mvn", "package"},
		},
		{
			name: "maven 多模块 reactor 带上 -pl 选启动模块",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "c", Task: "spring-boot:run", Args: []string{"-pl", "lgsy-app-module/lgsy-app-start"}},
			want: []string{"mvn", "-pl", "lgsy-app-module/lgsy-app-start", "spring-boot:run", "-Dspring-boot.run.fork=false"},
		},
		{
			name: "jar 补 java 前缀",
			kind: project.KindJar,
			cfg:  project.LaunchConfig{ID: "c", Task: "-jar target/app.jar"},
			want: []string{"java", "-jar", "target/app.jar"},
		},
		{
			name: "maven 项目上的 java-jar 配置仍用 java 启动",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "java-jar", Task: "-jar target/dist/app/app.jar"},
			want: []string{"java", "-jar", "target/dist/app/app.jar"},
		},
		{
			name: "gradle 用 wrapper",
			kind: project.KindGradle,
			cfg:  project.LaunchConfig{ID: "c", Task: "bootRun"},
			dir:  fixture("fake-gradle"),
			want: []string{filepath.Join(fixture("fake-gradle"), "gradlew.bat"), "bootRun"},
		},
		{
			name: "gradle 无 wrapper 退回系统命令",
			kind: project.KindGradle,
			cfg:  project.LaunchConfig{ID: "c", Task: "bootRun"},
			dir:  fixture("empty"),
			want: []string{"gradle", "bootRun"},
		},
		{
			name: "node run dev 用 npm",
			kind: project.KindNode,
			cfg:  project.LaunchConfig{ID: "c", Task: "run dev"},
			dir:  fixture("empty"),
			want: []string{"npm", "run", "dev"},
		},
		{
			name: "node pnpm lockfile 决定包管理器",
			kind: project.KindNode,
			cfg:  project.LaunchConfig{ID: "c", Task: "run dev"},
			dir:  fixture("pnpm-project"),
			want: []string{"pnpm", "run", "dev"},
		},
		{
			name: "node 直接跑文件不加包管理器",
			kind: project.KindNode,
			cfg:  project.LaunchConfig{ID: "c", Task: "server.js"},
			dir:  fixture("empty"),
			want: []string{"node", "server.js"},
		},
		{
			name: "exec 保留引号分组",
			kind: project.KindExec,
			cfg:  project.LaunchConfig{ID: "c", Task: `java -cp "lib a.jar" Main`},
			want: []string{"java", "-cp", "lib a.jar", "Main"},
		},
		{
			name: "maven profile 走插件参数",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "c", Task: "spring-boot:run", Profile: "dev"},
			want: []string{"mvn", "spring-boot:run", "-Dspring-boot.run.fork=false", "-Dspring-boot.run.profiles=dev"},
		},
		{
			name: "maven SingleJVM=false 恢复 fork（不留 fork=false）",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "c", Task: "spring-boot:run", SingleJVM: boolPtr(false)},
			want: []string{"mvn", "spring-boot:run"},
		},
		{
			name: "非 spring-boot:run 的 maven 任务即使开 SingleJVM 也不加 fork 参数",
			kind: project.KindMaven,
			cfg:  project.LaunchConfig{ID: "c", Task: "package", SingleJVM: boolPtr(true)},
			want: []string{"mvn", "package"},
		},
		{
			name: "jar profile 走应用参数",
			kind: project.KindJar,
			cfg:  project.LaunchConfig{ID: "c", Task: "-jar app.jar", Profile: "dev"},
			want: []string{"java", "-jar", "app.jar", "--spring.profiles.active=dev"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir
			if dir == "" {
				dir = t.TempDir()
			}
			name, args, err := buildArgv(tc.kind, &tc.cfg, dir, nil)
			if err != nil {
				t.Fatal(err)
			}
			all := append([]string{name}, args...)
			if len(all) != len(tc.want) {
				t.Fatalf("argv = %v, 期望 %v", all, tc.want)
			}
			for i := range all {
				if filepath.Clean(all[i]) != filepath.Clean(tc.want[i]) {
					t.Errorf("argv[%d] = %q, 期望 %q", i, all[i], tc.want[i])
				}
			}
		})
	}
}

func TestBuildArgvRejectsEmptyTask(t *testing.T) {
	if _, _, err := buildArgv(project.KindMaven, &project.LaunchConfig{ID: "c"}, ".", nil); err == nil {
		t.Fatal("空 task 必须报错")
	}
}

func TestReadinessRegex(t *testing.T) {
	yes := []string{
		"com.example.demo.DemoApplication : Started DemoApplication in 5.781 seconds (process running for 6.863)",
		"o.s.b.a.h2.H2ConsoleAutoConfiguration    : Started H2 in 0.4 seconds",
	}
	for _, s := range yes {
		if !matchesReadiness(s, nil) {
			t.Errorf("应判为就绪: %q", s)
		}
	}
	// "Starting"（还没起来）、BUILD FAILURE、以及缺秒数部分的 "Tomcat started on port"
	// 都不能算就绪，否则会把启动中的进程误报成成功。
	no := []string{
		"Starting DemoApplication using Java 17",
		"[INFO] BUILD FAILURE",
		"Tomcat started on port 18099",
	}
	for _, s := range no {
		if matchesReadiness(s, nil) {
			t.Errorf("不应判为就绪: %q", s)
		}
	}
	if !matchesReadiness("Server startup completed", []string{`Server startup completed`}) {
		t.Error("自定义就绪正则未生效")
	}
}

// TestReclaimAdoptsLiveAndClearsDead 覆盖 daemon 重启后的修账：
// PID 活着的一律以 orphan 接管（能停、但没日志），PID 已消失的改写成 zombie。
func TestReclaimAdoptsLiveAndClearsDead(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(Options{RunsDir: dir, StartTimeout: time.Second})
	defer m.Close()

	live := &Run{RunInfo: RunInfo{ID: "live", ProjectID: "p1", Name: "live", State: StateRunning, PID: os.Getpid(), StartedAt: time.Now()}}
	dead := &Run{RunInfo: RunInfo{ID: "dead", ProjectID: "p2", Name: "dead", State: StateRunning, PID: 999999, StartedAt: time.Now()}}
	metaPath := func(r *Run) string {
		return filepath.Join(dir, sanitize(r.ProjectID)+"-"+r.ID+".meta.json")
	}
	for _, r := range []*Run{live, dead} {
		if err := os.WriteFile(metaPath(r), mustJSON(r), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	adopted, cleared := m.Reclaim()
	if adopted != 1 {
		t.Errorf("接管 %d 条, 期望 1", adopted)
	}
	if cleared != 1 {
		t.Errorf("修账 %d 条, 期望 1", cleared)
	}
	r, ok := m.Get("p1")
	if !ok {
		t.Fatal("活进程没被接管，重启后会被报成\"停止\"")
	}
	if r.State != StateOrphan {
		t.Errorf("状态 = %s, 期望 orphan", r.State)
	}
	if _, ok := m.Get("p2"); ok {
		t.Error("死进程不该进 active")
	}

	// 死进程的记录必须就地改写成 zombie，否则下次 Reclaim 又会把它当"在跑"
	var onDisk Run
	raw, err := os.ReadFile(metaPath(dead))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.State != StateZombie {
		t.Errorf("盘上记录 state = %s, 期望 zombie", onDisk.State)
	}

	// 再启一次不接管死记录
	m2 := NewManager(Options{RunsDir: dir})
	defer m2.Close()
	if a, c := m2.Reclaim(); a != 1 || c != 0 {
		t.Errorf("第二次 Reclaim 接管 %d、修账 %d, 期望 1、0", a, c)
	}
}

// TestBlockForever 只作为被测子进程存在：父测试把它拉起来挂在进程表里，
// 用来验证"标签说结束了、进程其实还在"时 runlite 的处理。正常跑单测时直接跳过。
func TestBlockForever(t *testing.T) {
	if os.Getenv("RUNLITE_TEST_CHILD") != "1" {
		t.Skip("只作为被测子进程运行")
	}
	if os.Getenv("RUNLITE_TEST_LISTEN") == "1" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
	}
	select {}
}

func spawnBlockingChild(t *testing.T) (int, uint64) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestBlockForever$", "-test.timeout=10m")
	cmd.Env = append(os.Environ(), "RUNLITE_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal("拉起被测子进程失败:", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pid := cmd.Process.Pid
	created, ok := netx.ProcIdent(pid)
	if !ok {
		t.Fatalf("刚启动的子进程 pid %d 查不到身份", pid)
	}
	return pid, created
}

// TestReclaimAdoptsFinishedLabelWithLivePid 盯住盘上标签与事实相反的记录：
// 上一版 daemon 把 132s 才就绪的应用判成 failed，重启后如果按标签归档，
// 这个还在监听端口的服务就被 runlite 忘掉了，谁也不会再去停它。
func TestReclaimAdoptsFinishedLabelWithLivePid(t *testing.T) {
	pid, created := spawnBlockingChild(t)
	dir := t.TempDir()
	info := RunInfo{ID: "slow", ProjectID: "p1", Name: "slow", State: StateFailed,
		PID: pid, PidCreated: created, StartedAt: time.Now(), Port: 8081}
	if err := os.WriteFile(filepath.Join(dir, sanitize("p1")+"-slow.meta.json"), mustJSON(info), 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(Options{RunsDir: dir})
	defer m.Close()
	if a, c := m.Reclaim(); a != 1 || c != 0 {
		t.Fatalf("接管 %d、修账 %d，期望把还活着的 failed 接管成 1 条", a, c)
	}
	r, ok := m.Get("p1")
	if !ok {
		t.Fatal("failed 标签下的活进程没被接管")
	}
	if r.State != StateOrphan || r.PID != pid {
		t.Errorf("接管结果 = %s pid %d，期望 orphan pid %d", r.State, r.PID, pid)
	}
}

// TestStopKillsProcessDespiteFinishedLabel 是同一个事实的另一半：stop 必须看进程在不在，
// 不能因为标签写着 failed 就直接返回"已停"。此前本机实测：端口 8081 仍在监听，
// runlite 却报 failed 并 rc=0，用户以为服务停了。
func TestStopKillsProcessDespiteFinishedLabel(t *testing.T) {
	pid, created := spawnBlockingChild(t)
	m := NewManager(Options{RunsDir: t.TempDir(), DefaultGrace: 3 * time.Second})
	defer m.Close()

	r := &Run{RunInfo: RunInfo{ID: "slow", ProjectID: "p1", Name: "slow", State: StateFailed,
		PID: pid, PidCreated: created, StartedAt: time.Now()}} // doneCh 为空：按只能强停的接管对待
	m.mu.Lock()
	m.active["p1"] = r
	m.mu.Unlock()

	out, err := m.Stop("p1", false)
	if err != nil {
		t.Fatal("停一个 failed 标签下的活进程报错:", err)
	}
	if out.State != StateStopped {
		t.Errorf("状态 = %s，期望 stopped（说明根本没去结束进程）", out.State)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !netx.PidSame(pid, created) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("pid %d 仍在监听身份：stop 只改了标签，没结束进程", pid)
}

// TestMarkSlowKeepsStarting 钉住"启动慢不等于失败"：进程还在起的时候写成 failed，
// 后续 stop 会跳过它，list 也会报出一个假失败。就绪后转 running 还得把这条提示清掉。
func TestMarkSlowKeepsStarting(t *testing.T) {
	m := NewManager(Options{RunsDir: t.TempDir(), StartTimeout: 2 * time.Second})
	defer m.Close()
	r := &Run{RunInfo: RunInfo{ID: "slow", ProjectID: "p1", Name: "slow", State: StateStarting,
		StartedAt: time.Now()}}
	m.markSlow(r, nil)
	if r.State != StateStarting {
		t.Errorf("慢启动被判成 %s，期望保持 starting", r.State)
	}
	if r.Reason == "" {
		t.Error("保持 starting 却没留下原因，用户看不到为什么慢")
	}

	m.markRunning(r, 8081)
	if r.State != StateRunning || r.Reason != "" || r.Port != 8081 {
		t.Errorf("就绪后 = %s/端口 %d/%q，期望 running、端口 8081、原因清空", r.State, r.Port, r.Reason)
	}
}

// TestWaitPortClearedOnlyCountsListeners 钉住 stop 之后端口复核的口径。
// 本机实测：进程确实被结束了，stop 却报 "端口 8081 仍被 PID [] 占用" 并置 zombie——
// 那一下用的是 TCP 握手，撞上操作系统还没撤掉的监听套接字；而归属列表为空恰恰说明没人占。
func TestWaitPortClearedOnlyCountsListeners(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("起不了临时监听:", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if owners := waitPortCleared(port, 200*time.Millisecond); len(owners) == 0 {
		ln.Close()
		t.Skip("这台机器读不到监听套接字的归属，负对照无从谈起")
	}

	// 握手一次，让客户端侧留下 TIME_WAIT，再撤掉监听者：复核必须认"已释放"。
	c, err := net.Dial("tcp", ln.Addr().String())
	if err == nil {
		_ = c.Close()
	}
	if owners := waitPortCleared(port, 200*time.Millisecond); len(owners) == 0 {
		ln.Close()
		t.Fatalf("撤走监听者之前就该报出占用者，这里报空说明这道断言是空针（端口 %d）", port)
	}
	_ = ln.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		owners := waitPortCleared(port, 500*time.Millisecond)
		if len(owners) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("监听者已撤走，复核仍反复报占用：端口 %d 会被误判成 zombie（占用者 %v）", port, owners)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestStopNotRunningIsSentinel 钉住"没在跑"这个结论可被上层识别：service 靠它把重复的
// stop 变成幂等成功（并说清这次没做事），而不是当成一条错误。
func TestStopNotRunningIsSentinel(t *testing.T) {
	m := NewManager(Options{RunsDir: t.TempDir()})
	defer m.Close()
	if _, err := m.Stop("p-none", false); !errors.Is(err, ErrNotRunning) {
		t.Errorf("= %v，期望 ErrNotRunning", err)
	}
}

// TestWatchReadyOnlyCountsOwnTree 钉住端口归属。本机实测：`mvn package` 那次编译期间
// 机器上别的过程新开了一个监听端口，被 runlite 记成"这个项目在 5283 上就绪"——
// 只看"启动后新出现的端口"而不看归属，就会把无关进程当成服务。
// 反向也要成立：自己进程树里的端口必须还能被认出来，不然这个修复只是把探测关掉。
func TestWatchReadyOnlyCountsOwnTree(t *testing.T) {
	m := NewManager(Options{RunsDir: t.TempDir(), StartTimeout: 4 * time.Second,
		ScanInterval: 150 * time.Millisecond, PortScanGrace: 200 * time.Millisecond})
	defer m.Close()

	startChild := func(id string, env map[string]string) {
		t.Helper()
		_, err := m.Start(StartSpec{Project: &project.Project{ID: id, Name: id, Dir: t.TempDir(), Kind: project.KindExec,
			Active: "exec", Configs: []project.LaunchConfig{{ID: "exec", Task: os.Args[0] +
				" -test.run=^TestBlockForever$ -test.timeout=10m", Env: env}}}})
		if err != nil {
			t.Fatalf("拉起被测进程 %s 失败: %v", id, err)
		}
	}

	t.Cleanup(func() {
		for _, id := range []string{"blocker", "listener"} {
			_, _ = m.Stop(id, true)
		}
	})

	startChild("blocker", map[string]string{"RUNLITE_TEST_CHILD": "1"})
	r, ok := m.Get("blocker")
	if !ok || r.State != StateStarting {
		t.Fatalf("刚拉起 = %v，期望 starting", r)
	}

	// 外来端口是在 baseline 之后出现的：按"新出现"就认领的话，它会被记成 blocker 的端口
	foreign, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("起不了外来监听:", err)
	}
	foreignPort := foreign.Addr().(*net.TCPAddr).Port
	defer foreign.Close()

	time.Sleep(2 * time.Second)
	if cur, ok := m.Get("blocker"); ok && (cur.Port != 0 || cur.State == StateRunning) {
		t.Errorf("外来端口 %d 被记成本项目的: port=%d state=%s", foreignPort, cur.Port, cur.State)
	}
	_, _ = m.Stop("blocker", true)

	// 实心对照：子进程自己监听时，端口必须归它
	startChild("listener", map[string]string{"RUNLITE_TEST_CHILD": "1", "RUNLITE_TEST_LISTEN": "1"})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cur, ok := m.Get("listener")
		if ok && cur.State == StateRunning && cur.Port > 0 {
			owners := netx.PIDsOnPort(cur.Port)
			if len(owners) == 0 || !netx.TreePIDs(cur.PID)[owners[0]] {
				t.Fatalf("认领的端口 %d 归属 %v 不在 pid %d 的进程树里", cur.Port, owners, cur.PID)
			}
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	if cur, ok := m.Get("listener"); ok {
		t.Fatalf("子进程自己监听的端口没被认出来（对照失败）：state=%s port=%d", cur.State, cur.Port)
	}
	t.Fatal("对照失败：监听子进程的运行记录找不到")
}

func TestSplitArgsKeepsQuotedGroups(t *testing.T) {
	got := splitArgs(`java -cp "a b.jar" -Dx=1 Main`)
	want := []string{"java", "-cp", "a b.jar", "-Dx=1", "Main"}
	if len(got) != len(want) {
		t.Fatalf("= %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, 期望 %q", i, got[i], want[i])
		}
	}
}

func TestSanitizeAndTruncate(t *testing.T) {
	if got := sanitize(`a/b\c:d`); got != "a_b_c_d" {
		t.Errorf("sanitize = %q", got)
	}
	if got := truncate("  hello\nworld  ", 5); got != "hello…" {
		t.Errorf("truncate = %q", got)
	}
}

func boolPtr(b bool) *bool { return &b }

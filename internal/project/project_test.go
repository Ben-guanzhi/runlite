package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPutDoesNotDeadlock 盯住一个真实踩过的坑：saveLocked 在持锁状态下调用 Snapshot，
// 而 sync.Mutex 不可重入 —— Put 会永久卡死，表现为 daemon 对 /api/projects 无响应。
func TestPutDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	r, err := LoadRegistry(filepath.Join(dir, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := r.Put(&Project{ID: "a", Name: "a", Dir: "/a", Kind: KindMaven,
			Configs: []LaunchConfig{{ID: "c1", Task: "spring-boot:run"}}}); err != nil {
			t.Error(err)
		}
		if err := r.Put(&Project{ID: "b", Name: "b", Dir: "/b", Kind: KindNode,
			Configs: []LaunchConfig{{ID: "c1", Task: "run dev"}}}); err != nil {
			t.Error(err)
		}
		if got := len(r.Snapshot()); got != 2 {
			t.Errorf("Snapshot = %d, 期望 2", got)
		}
		if _, err := r.Remove("a"); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Put/Snapshot 路径出现死锁")
	}
}

func TestRegistryPersistsAndResolves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	p := &Project{ID: IDFor(`D:\srv\demo`), Name: "demo", Dir: "D:/srv/demo", Kind: KindMaven,
		Configs: []LaunchConfig{{ID: "run", Task: "spring-boot:run"}, {ID: "pkg", Task: "package"}}}
	p.Active = "pkg"
	if err := r.Put(p); err != nil {
		t.Fatal(err)
	}
	again, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Resolve("demo")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := got.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Task != "package" {
		t.Errorf("active 配置 = %q", cfg.Task)
	}
	if _, err := again.Resolve(p.ID[:6]); err != nil {
		t.Errorf("id 前缀解析失败: %v", err)
	}
	// 大小写不敏感的名称解析
	if _, err := again.Resolve("DEMO"); err != nil {
		t.Errorf("名称解析应忽略大小写: %v", err)
	}
}

// TestResolveByDirectory 钉住"给目录也能找到项目"。本机实测：注册的是仓库根下面的
// start 模块，用户对着仓库根 start 却被告知"未找到项目"——那目录明明是同一个项目。
func TestResolveByDirectory(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "app-module", "app-start")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRegistry(filepath.Join(t.TempDir(), "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Put(&Project{ID: IDFor(module), Name: "app", Dir: module, Kind: KindExec}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{module, root, filepath.Join(module, "..")} {
		got, err := r.Resolve(key)
		if err != nil {
			t.Errorf("用目录 %q 解析失败: %v", key, err)
			continue
		}
		if got.Name != "app" {
			t.Errorf("目录 %q 解析到 %q，期望 app", key, got.Name)
		}
	}

	// 反面对照：真实存在但没注册过的目录，不能瞎猜，也不能说成"未找到项目"这种没信息量的话
	unrelated := filepath.Join(root, "unrelated")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve(unrelated)
	if err == nil {
		t.Fatal("无关目录被解析成了项目")
	}
	if !strings.Contains(err.Error(), "没有已注册的项目") {
		t.Errorf("无关目录的报错 = %q，期望指明该目录没注册过项目", err)
	}
}

func TestDuplicateNameIsNotGuessed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	r, _ := LoadRegistry(path)
	_ = r.Put(&Project{ID: "x1", Name: "demo", Dir: "/x1"})
	_ = r.Put(&Project{ID: "x2", Name: "demo", Dir: "/x2"})
	if _, err := r.Resolve("demo"); err == nil {
		t.Fatal("同名项目必须报错，不能随便挑一个")
	}
}

func TestIDForIsPathCaseInsensitive(t *testing.T) {
	if IDFor(`D:\SRV\Demo`) != IDFor(`d:\srv\demo`) {
		t.Fatal("同一目录在 Windows 上因大小写不同生成了不同 id")
	}
}

func TestLoadRegistryToleratesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projects.json")
	if err := os.WriteFile(path, []byte("  "), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Snapshot()) != 0 {
		t.Fatal("空文件应视为空注册表")
	}
}

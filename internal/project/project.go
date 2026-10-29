// Package project 定义项目模型与本机注册表（projects.json）。
package project

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Kind string

const (
	KindMaven  Kind = "maven"
	KindGradle Kind = "gradle"
	KindJar    Kind = "jar"
	KindNode   Kind = "node"
	KindExec   Kind = "exec" // 自定义命令行，用于非标准工程与端到端验证
)

// LaunchConfig 是一次启动所需的全部参数。
type LaunchConfig struct {
	ID        string            `json:"id"`
	Label     string            `json:"label"`
	Task      string            `json:"task"` // "spring-boot:run" / "bootRun" / "-jar app.jar" / 原始命令行(KindExec)
	Profile   string            `json:"profile,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	JdkPath   string            `json:"jdkPath,omitempty"`
	Port      int               `json:"port,omitempty"`
	Readiness []string          `json:"readiness,omitempty"`
	StopGrace string            `json:"stopGrace,omitempty"`
	// MainClass 非空表示这是一条 direct-main 配置：Maven 解析 classpath 后，用单个 java JVM
	// 直接跑这个全限定类名，不再包一层 Maven JVM、也不 fork 应用 JVM。
	MainClass string `json:"mainClass,omitempty"`
	// SingleJVM 只对 spring-boot:run 有意义。nil 表示跟随默认（对 spring-boot:run 默认单 JVM）：
	// 插件 fork=false，应用直接跑在 Maven 这个 JVM 里，不再多 fork 一个 JVM —— 一个 Spring Boot
	// 项目从"mvn JVM + fork 的应用 JVM"两块堆，变成一块，这就是 IDEA 内存更省的同一件事。
	// 显式 *false 才恢复 fork，给那些非要隔离 JVM 的用户留后门。
	SingleJVM *bool `json:"singleJVM,omitempty"`
}

type Project struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Dir     string         `json:"dir"`
	Kind    Kind           `json:"kind"`
	Configs []LaunchConfig `json:"configs"`
	Active  string         `json:"active"`
	AddedAt time.Time      `json:"addedAt"`
	Notes   []string       `json:"notes,omitempty"`
}

func NormalizeDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func IDFor(dir string) string {
	key := strings.ToLower(filepath.ToSlash(filepath.Clean(dir)))
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// ActiveConfig 返回当前选中的启动配置，缺失时回落到第一个。
func (p *Project) ActiveConfig() (*LaunchConfig, error) {
	if len(p.Configs) == 0 {
		return nil, fmt.Errorf("项目 %s 没有可用的启动配置", p.Name)
	}
	for i := range p.Configs {
		if p.Configs[i].ID == p.Active {
			return &p.Configs[i], nil
		}
	}
	return &p.Configs[0], nil
}

func (p *Project) Config(id string) (*LaunchConfig, error) {
	for i := range p.Configs {
		if p.Configs[i].ID == id {
			return &p.Configs[i], nil
		}
	}
	return nil, fmt.Errorf("项目 %s 不存在启动配置 %q", p.Name, id)
}

type Registry struct {
	mu   sync.Mutex
	path string
	m    map[string]*Project
}

func LoadRegistry(path string) (*Registry, error) {
	r := &Registry{path: path, m: map[string]*Project{}}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return r, nil
	}
	var list []*Project
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	for _, p := range list {
		if p != nil && p.ID != "" {
			r.m[p.ID] = p
		}
	}
	return r, nil
}

func (r *Registry) saveLocked() error {
	list := r.snapshotLocked()
	raw, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (r *Registry) Put(p *Project) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[p.ID] = p
	return r.saveLocked()
}

func (r *Registry) Remove(id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; !ok {
		return false, nil
	}
	delete(r.m, id)
	return true, r.saveLocked()
}

func (r *Registry) Get(id string) (*Project, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.m[id]
	return p, ok
}

// Resolve 接受项目 id、id 前缀或名称（名称不唯一时报错，不猜）。
func (r *Registry) Resolve(key string) (*Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.m[key]; ok {
		return p, nil
	}
	var prefix []*Project
	var named []*Project
	for _, p := range r.m {
		if strings.HasPrefix(p.ID, key) {
			prefix = append(prefix, p)
		}
		if strings.EqualFold(p.Name, key) {
			named = append(named, p)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if len(named) > 1 {
		return nil, fmt.Errorf("名称 %q 对应多个项目，请改用 id：%s", key, joinIDs(named))
	}
	if len(prefix) == 1 {
		return prefix[0], nil
	}
	if len(prefix) > 1 {
		return nil, fmt.Errorf("id 前缀 %q 命中多个项目：%s", key, joinIDs(prefix))
	}

	// 目录也能当项目用。本机实测：用户对着仓库根 `D:\liugang\lgsy-app` 执行 start，
	// 而注册的是根下面的 start 模块——只认 id/名称就得到一句"未找到项目"，
	// 看不出那个目录其实就属于已经注册过的项目。
	if abs, err := filepath.Abs(key); err == nil {
		want := strings.ToLower(filepath.ToSlash(filepath.Clean(abs)))
		var dirs []*Project
		for _, p := range r.m {
			got := strings.ToLower(filepath.ToSlash(filepath.Clean(p.Dir)))
			if got == want || strings.HasPrefix(got, want+"/") || strings.HasPrefix(want, got+"/") {
				dirs = append(dirs, p)
			}
		}
		if len(dirs) == 1 {
			return dirs[0], nil
		}
		if len(dirs) > 1 {
			return nil, fmt.Errorf("目录 %q 关联到多个项目，请改用 id：%s", key, joinIDs(dirs))
		}
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			return nil, fmt.Errorf("%q 是个目录，但里面没有已注册的项目（先 runlite add %q）", key, key)
		}
	}

	return nil, fmt.Errorf("未找到项目 %q（先 runlite add <目录>）", key)
}

func joinIDs(ps []*Project) string {
	ids := make([]string, 0, len(ps))
	for _, p := range ps {
		ids = append(ids, p.ID+":"+p.Name)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}

func (r *Registry) Snapshot() []*Project {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

// snapshotLocked 是 Snapshot 的不加锁版本：saveLocked 已经在锁内，
// 再调 Snapshot 会死锁（sync.Mutex 不可重入）。
func (r *Registry) snapshotLocked() []*Project {
	out := make([]*Project, 0, len(r.m))
	for _, p := range r.m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

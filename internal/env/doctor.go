// Package env 探测本机已有的工具链（runlite 不内置任何运行时）。
package env

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

type Tool struct {
	Name    string `json:"name"`
	Bin     string `json:"bin,omitempty"`
	Home    string `json:"home,omitempty"`
	Version string `json:"version,omitempty"`
	Err     string `json:"err,omitempty"`
}

func (t Tool) OK() bool { return t.Err == "" && t.Bin != "" }

type Report struct {
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Tools     []Tool    `json:"tools"`
	Warnings  []string  `json:"warnings,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
	overrides map[string]string
}

func (r *Report) Tool(name string) (Tool, bool) {
	for _, t := range r.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

func (r *Report) JavaHome() string {
	if t, ok := r.Tool("java"); ok {
		return t.Home
	}
	return ""
}

func (r *Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "系统: %s/%s  探测时间: %s\n", r.OS, r.Arch, r.CheckedAt.Format("2006-01-02 15:04:05"))
	for _, t := range r.Tools {
		if t.OK() {
			fmt.Fprintf(&b, "  %-5s ✓ %-24s %s\n", t.Name, t.Version, t.Bin)
		} else {
			fmt.Fprintf(&b, "  %-5s ✗ %s\n", t.Name, t.Err)
			if h := hint(t.Name); h != "" {
				fmt.Fprintf(&b, "        怎么办: %s\n", h)
			}
		}
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  ! %s\n", w)
	}
	return b.String()
}

// hint 把"没装"翻译成能照着做的一句话。写这类文案最容易犯的错是给出一个不存在的
// 配置键（这里曾经是 jdkPath，实际键叫 toolPaths），所以每条都以 settings.json
// 的真实结构为准，并由 TestHintKeysMatchSettings 核对。
func hint(name string) string {
	switch name {
	case "java":
		return `装 JDK 17+，或在 settings.json 写 "toolPaths": {"java": "C:/jdk/bin/java.exe"}`
	case "mvn":
		return `装 Maven 3.6+（pom.xml 项目需要）；已构建好 jar 的话可绕过它：runlite add <目录> --cmd "java -jar target\xxx.jar"`
	case "gradle":
		return "工程里有 gradlew 时 runlite 用 wrapper，不需要装；否则装 Gradle 或用 toolPaths 指 gradle.bat"
	case "node":
		return `装 Node 18+；纯前端项目也可以用 toolPaths 指 node.exe`
	case "npm", "pnpm", "yarn", "bun":
		return name + " 只影响 package.json 项目的包管理器选择；缺了会退回 npm，装上或在 toolPaths 里指路径即可"
	}
	return ""
}

// Overrides 允许 settings.json 指定路径，key 取 java/maven/gradle/node/npm/pnpm/yarn/bun。
func Detect(overrides map[string]string) *Report {
	r := &Report{
		OS: runtime.GOOS, Arch: runtime.GOARCH, CheckedAt: time.Now(),
		overrides: overrides,
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	add := func(t Tool) {
		mu.Lock()
		r.Tools = append(r.Tools, t)
		mu.Unlock()
	}
	type spec struct {
		name    string
		bin     string
		version func(string) ([]string, string)
	}
	specs := []spec{
		{"java", "java", javaVersionArgs},
		{"mvn", "mvn", versionFlag("--version")},
		{"gradle", "gradle", versionFlag("--version")},
		{"node", "node", versionFlag("--version")},
		{"npm", "npm", versionFlag("--version")},
		{"pnpm", "pnpm", versionFlag("--version")},
		{"yarn", "yarn", versionFlag("--version")},
		{"bun", "bun", versionFlag("--version")},
	}
	for _, s := range specs {
		wg.Add(1)
		go func(s spec) {
			defer wg.Done()
			add(probe(s.name, s.bin, s.version, r.overrides))
		}(s)
	}
	wg.Wait()
	sort.SliceStable(r.Tools, func(i, j int) bool { return order(r.Tools[i].Name) < order(r.Tools[j].Name) })

	if java, ok := r.Tool("java"); !ok || !java.OK() {
		r.Warnings = append(r.Warnings, `未找到 java：Java/Spring Boot 项目无法启动，可在 settings.json 里写 "toolPaths": {"java": ...}`)
	}
	if mvn, ok := r.Tool("mvn"); ok && mvn.OK() && mvn.Version == "" {
		r.Warnings = append(r.Warnings, "mvn 可执行但版本号解析失败")
	}
	return r
}

// order 按 java > mvn > gradle > node > 包管理器 的展示顺序给工具编号。
// 用 switch 而非 map：老写法每次调用都分配一个 8 元素的 map，而 Detect 的
// sort.SliceStable 对 8 个元素要调 O(n log n) ≈ 24 次，白扔 24 次分配。
func order(name string) int {
	switch name {
	case "java":
		return 0
	case "mvn":
		return 1
	case "gradle":
		return 2
	case "node":
		return 3
	case "npm":
		return 4
	case "pnpm":
		return 5
	case "yarn":
		return 6
	case "bun":
		return 7
	}
	return 99
}

func probe(name, bin string, vf func(string) ([]string, string), overrides map[string]string) Tool {
	t := Tool{Name: name}
	if ov := overrides[name]; ov != "" {
		if st, err := os.Stat(ov); err == nil && !st.IsDir() {
			t.Bin = ov
		} else if st != nil && st.IsDir() {
			t.Err = fmt.Sprintf("settings.json 中 %s=%s 不是可执行文件", name, ov)
			return t
		} else {
			t.Err = fmt.Sprintf("settings.json 中 %s=%s 不存在", name, ov)
			return t
		}
	} else {
		p, err := exec.LookPath(bin)
		if err != nil {
			t.Err = "PATH 中未找到 " + bin
			return t
		}
		t.Bin = p
	}
	args, homeEnv := vf(name)
	out := runCmd(t.Bin, args, 8*time.Second)
	if out == "" {
		t.Err = "已找到但无法执行（" + t.Bin + "）"
		return t
	}
	t.Version = firstLine(out)
	if homeEnv != "" {
		if h := os.Getenv(homeEnv); h != "" {
			t.Home = h
		}
	}
	if name == "java" && t.Home == "" {
		t.Home = filepath.Dir(filepath.Dir(t.Bin)) // <jdk>/bin/java(.exe)
	}
	return t
}

func javaVersionArgs(string) ([]string, string) { return []string{"-version"}, "JAVA_HOME" }

func versionFlag(flag string) func(string) ([]string, string) {
	return func(string) ([]string, string) { return []string{flag}, "" }
}

// runCmd probes one tool's version with a hard timeout. The probe must not
// outlive its timeout: an abandoned mvn/gradle/node keeps running (and holds
// its daemon) long after the doctor moved on, so the context kills it.
func runCmd(bin string, args []string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "MAVEN_OPTS=", "GRADLE_OPTS=-Dorg.gradle.daemon=false")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "" // timed out: a half-finished probe is not a verdict
	}
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}

func firstLine(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

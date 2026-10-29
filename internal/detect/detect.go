// Package detect 把一个目录识别为可启动项目，并生成默认启动配置。
package detect

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"runlite/internal/project"
)

// Result 是检测输出；Project 字段不含 ID/AddedAt，由调用方补齐。
type Result struct {
	Project *project.Project
}

type buildInfo struct {
	kind     project.Kind
	name     string
	configs  []project.LaunchConfig
	notes    []string
	hasJava  bool
	hasPkgJS bool
}

// Detect 按 Java 构建文件 > JAR > package.json 的优先级识别目录。
func Detect(dir string) (*Result, error) {
	dir, err := project.NormalizeDir(dir)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", dir)
	}

	var info *buildInfo
	if has(dir, "pom.xml") {
		info, err = fromMaven(dir)
	} else if gradle := findGradleBuild(dir); gradle != "" {
		info, err = fromGradle(dir, gradle)
	} else if j := findJar(dir); j != "" {
		info, err = fromJar(dir, j)
	} else if has(dir, "package.json") {
		info, err = fromNode(dir)
	} else {
		return nil, fmt.Errorf("目录 %s 里没有找到 pom.xml、build.gradle(.kts)、*.jar 或 package.json，无法作为项目添加", dir)
	}
	if err != nil {
		return nil, err
	}
	if info.hasJava && info.hasPkgJS {
		info.notes = append(info.notes, "同目录存在 package.json，按 Java 项目处理（需要跑前端请另建 exec 项目）")
	}
	if len(info.configs) == 0 {
		return nil, fmt.Errorf("目录 %s 识别为 %s，但没能推导出启动命令，请用 runlite add --cmd 手工指定", dir, info.kind)
	}
	for i := range info.configs {
		if info.configs[i].ID == "" {
			info.configs[i].ID = string(info.kind) + "-" + strconv.Itoa(i+1)
		}
		if info.configs[i].Label == "" {
			info.configs[i].Label = info.configs[i].ID
		}
	}
	name := info.name
	if name == "" {
		name = filepath.Base(dir)
	}
	active := pickActiveConfig(info.configs)
	return &Result{Project: &project.Project{
		Name:    name,
		Dir:     filepath.ToSlash(dir),
		Kind:    info.kind,
		Configs: info.configs,
		Active:  active,
		Notes:   info.notes,
	}}, nil
}

// pickActiveConfig 从检测出的配置里挑默认（active）那一个。
//
// 默认是第一个配置；但对 Windows 上这个规模的 Spring Boot 项目，spring-boot:run 的
// 命令行会超过 Windows 的 CreateProcess 上限（本机 lgsy-app 的 classpath 有几百个 jar，
// 报 error=206，且是确定性失败），反而是检测到的 java-jar 配置（target/dist/**/app.jar
// 带着 lib/，能直接跑）才能用。所以当存在可运行的装配 jar 时，把 java-jar 提升为默认。
func pickActiveConfig(configs []project.LaunchConfig) string {
	if len(configs) == 0 {
		return ""
	}
	for _, c := range configs {
		t := strings.TrimSpace(c.Task)
		if strings.HasPrefix(t, "-jar ") {
			jar := strings.TrimSpace(strings.TrimPrefix(t, "-jar"))
			if jar != "" && hasLibSibling(jar) {
				return c.ID
			}
		}
	}
	return configs[0].ID
}

func has(dir, file string) bool {
	st, err := os.Stat(filepath.Join(dir, file))
	return err == nil && !st.IsDir()
}

// item 是一个候选 jar 及其修改时间，供 jarsIn/assemblyJars 等按新旧排序。
type item struct {
	path string
	mod  int64
}

func fromMaven(dir string) (*buildInfo, error) {
	pom, err := parsePom(filepath.Join(dir, "pom.xml"))
	if err != nil {
		return nil, err
	}
	info := &buildInfo{kind: project.KindMaven, name: pom.artifactId, hasJava: true}
	info.hasPkgJS = has(dir, "package.json")
	// 多模块 reactor：根 pom 只负责聚合，启动类在子模块里。只看根 pom 会把这种项目
	// 识别成"没有可跑配置"（本机实测 lgsy-app 就是这样，start 只会去 mvn package）。
	bootRel := ""
	bootDir := dir
	if !pom.springBoot {
		if rel, ok := findBootModule(dir, "", 0); ok {
			bootRel, bootDir = rel, filepath.Join(dir, filepath.FromSlash(rel))
		}
	}
	port := mavenServerPort(bootDir, "")
	if pom.springBoot || bootRel != "" {
		cfg := project.LaunchConfig{
			ID: "spring-boot-run", Label: "mvn spring-boot:run", Task: "spring-boot:run", Port: port,
		}
		// 单 JVM 是默认：应用跑进 Maven 这个 JVM，不 fork 第二个——内存省一个应用 JVM 的堆，
		// 进程树变成一个，kill 一次停整个服务，Windows 上也不再有 classpath 过长（error 206）。
		single := true
		cfg.SingleJVM = &single
		if bootRel != "" {
			cfg.Args = []string{"-pl", bootRel}
			cfg.Label = "mvn -pl " + bootRel + " spring-boot:run"
			info.notes = append(info.notes, "启动模块在 "+bootRel+"：-pl 只启动它，依赖模块取本地仓库（改了依赖请先 mvn install -DskipTests）")
		}
		info.configs = append(info.configs, cfg)
		// direct-main：Maven 只负责解析 classpath 和编译，应用用单个 java JVM 直接跑 main，
		// 不再包一层 Maven JVM、也不再 fork 应用 JVM——这是 IDEA 的启动方式，省一个 JVM 的堆。
		// 首次启动要编译一次（mvn process-classes），改了依赖后要重跑。
		if main := findMainClass(bootDir); main != "" {
			dcfg := project.LaunchConfig{
				ID: "direct-main", Label: "java " + main, Task: "direct-main", Port: port, MainClass: main,
			}
			if bootRel != "" {
				dcfg.Args = []string{"-pl", bootRel}
			}
			info.configs = append(info.configs, dcfg)
			info.notes = append(info.notes, "direct-main：单 JVM 直跑 "+main+"（Maven 解析 classpath，应用不进 Maven 也不额外 fork JVM）")
		}
	}
	info.configs = append(info.configs, project.LaunchConfig{
		ID: "package", Label: "mvn package -DskipTests", Task: "package", Args: []string{"-DskipTests"}, Port: port,
	})
	if !pom.springBoot && bootRel == "" {
		info.notes = append(info.notes, "pom.xml 未声明 spring-boot-maven-plugin，已默认给出 mvn package 配置")
	}
	if jar := firstBuiltJar(bootDir); jar != "" {
		info.configs = append(info.configs, project.LaunchConfig{
			ID: "java-jar", Label: "java -jar " + filepath.ToSlash(jar), Task: "-jar " + filepath.ToSlash(jar), Port: port,
		})
		info.notes = append(info.notes, "检测到已构建产物，可用 java -jar 配置跳过每次编译")
	}
	return info, nil
}

// findBootModule 顺着根 pom 的 <modules> 找出真正可启动的那个模块，返回它相对根目录
// 的斜杠路径。判据用两条：模块 pom 里出现 spring-boot 插件/parent，或模块源码里有
// @SpringBootApplication。第二条是必须的——本机 lgsy-app 的 boot 插件全在远端父 pom
// （dcode-parent）里，本地每个 pom 都搜不到那个字符串，只认插件就等于判定"整个项目不可启动"。
// 限深是为了不去扫 target/ 里解出来的 pom，也不在超大 monorepo 里全量遍历。
func findBootModule(dir, rel string, depth int) (string, bool) {
	if depth > 3 {
		return "", false
	}
	p, err := parsePom(filepath.Join(dir, "pom.xml"))
	if err != nil {
		return "", false
	}
	if depth > 0 && p.packaging != "pom" && (p.springBoot || hasSpringBootApplication(dir)) {
		return rel, true
	}
	for _, m := range p.modules {
		clean := filepath.ToSlash(filepath.Join(rel, m))
		if clean == rel {
			continue
		}
		switch path.Base(clean) {
		case "target", "node_modules", ".":
			continue
		}
		if r, ok := findBootModule(filepath.Join(dir, filepath.FromSlash(m)), clean, depth+1); ok {
			return r, true
		}
	}
	return "", false
}

// hasSpringBootApplication 在模块自己的 src/main/java 里找启动类；隐藏目录（本机有
// .kilo/worktrees 里的同名副本）不进，免得把别人的工作树当成启动模块。
func hasSpringBootApplication(dir string) bool {
	return findMainClass(dir) != ""
}

// findMainClass 在 src/main/java 里找带 @SpringBootApplication 的启动类，返回全限定名
// （包名.类名）。direct-main 配置靠它直接跑 main；包名从 package 声明取，类名用文件名。
func findMainClass(dir string) string {
	root := filepath.Join(dir, "src", "main", "java")
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return ""
	}
	found := ""
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if found != "" || !strings.HasSuffix(d.Name(), ".java") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(raw), "@SpringBootApplication") {
			return nil
		}
		if fqn := javaClassName(p, raw); fqn != "" {
			found = fqn
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// javaClassName 从源文件还原全限定类名：package 声明 + 文件名（去掉 .java）。
func javaClassName(srcPath string, raw []byte) string {
	pkg := ""
	if m := reJavaPackage.FindSubmatch(raw); m != nil {
		pkg = string(m[1])
	}
	base := strings.TrimSuffix(filepath.Base(srcPath), ".java")
	if pkg != "" {
		return pkg + "." + base
	}
	return base
}

func findGradleBuild(dir string) string {
	for _, f := range []string{"build.gradle.kts", "build.gradle"} {
		if has(dir, f) {
			return f
		}
	}
	return ""
}

func fromGradle(dir, buildFile string) (*buildInfo, error) {
	info := &buildInfo{kind: project.KindGradle, name: gradleRootName(dir), hasJava: true}
	info.hasPkgJS = has(dir, "package.json")
	body, _ := os.ReadFile(filepath.Join(dir, buildFile))
	isBoot := strings.Contains(string(body), "org.springframework.boot") || strings.Contains(string(body), "spring-boot")
	wrapper := has(dir, "gradlew.bat") || has(dir, "gradlew")
	port := mavenServerPort(dir, "")
	if isBoot {
		info.configs = append(info.configs, project.LaunchConfig{
			ID: "bootRun", Label: "gradlew bootRun", Task: "bootRun", Port: port,
		})
	}
	if !wrapper {
		info.notes = append(info.notes, "未发现 gradlew，将使用系统 PATH 中的 gradle（版本可能与工程要求不一致）")
	}
	if len(info.configs) == 0 {
		info.configs = append(info.configs, project.LaunchConfig{
			ID: "build", Label: "gradlew build", Task: "build", Port: port,
		})
		info.notes = append(info.notes, "未识别到 Spring Boot 插件，只提供 build 配置；需要运行请用 add --cmd 指定")
	}
	return info, nil
}

func fromJar(dir, jarPath string) (*buildInfo, error) {
	rel := filepath.ToSlash(strings.TrimPrefix(filepath.ToSlash(jarPath), filepath.ToSlash(dir)+"/"))
	return &buildInfo{
		kind: project.KindJar,
		name: strings.TrimSuffix(filepath.Base(jarPath), ".jar"),
		configs: []project.LaunchConfig{{
			ID: "java-jar", Label: "java -jar " + rel, Task: "-jar " + rel,
			Port: jarServerPort(dir),
		}},
	}, nil
}

func findJar(dir string) string {
	candidates := []string{filepath.Join(dir, "build", "libs"), filepath.Join(dir, "target"), dir}
	for _, c := range candidates {
		if jars := jarsIn(c); len(jars) > 0 {
			return jars[0]
		}
	}
	return ""
}

// firstBuiltJar 挑一个"可以直接 java -jar 跑"的 jar。
//
// 优先看装配目录（target/dist 这类）：jeecg-boot 的发布结构是 target/dist/<name>/app.jar
// 带着同级的 lib/ 和 resources/，manifest 里的 Class-Path 就是指着它们，这种 jar 用
// java -jar 就能起来。而 target/ 下那个裸 app.jar 的 manifest 也写着相对路径的
// Class-Path，旁边却没有 lib/，java -jar 会直接 ClassNotFoundError——本机 lgsy-app
// 就是这么错出来的，顺带也绕开了 spring-boot:run 在 Windows 上命令行太长（error 206）的坑。
func firstBuiltJar(dir string) string {
	// 装配目录优先级最高：jar 所在目录带着 lib/ 才算"真能跑"。
	for _, assembled := range assemblyJars(dir) {
		if hasLibSibling(assembled) {
			return assembled
		}
	}
	if jars := jarsIn(filepath.Join(dir, "target")); len(jars) > 0 {
		return jars[0]
	}
	if jars := jarsIn(filepath.Join(dir, "build", "libs")); len(jars) > 0 {
		return jars[0]
	}
	return ""
}

// assemblyJars 返回 target/dist 与 build/dist 下的所有 jar（按修改时间倒序）。
func assemblyJars(dir string) []string {
	var out []item
	for _, base := range []string{"target", "build"} {
		dist := filepath.Join(dir, base, "dist")
		entries, err := os.ReadDir(dist)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			out = append(out, collectJars(filepath.Join(dist, e.Name()))...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod > out[j].mod })
	res := make([]string, 0, len(out))
	for _, f := range out {
		res = append(res, f.path)
	}
	return res
}

// hasLibSibling 判断 jar 的同级目录里有没有 lib/——有，说明这是装配好的可运行产物。
func hasLibSibling(jar string) bool {
	st, err := os.Stat(filepath.Join(filepath.Dir(jar), "lib"))
	return err == nil && st.IsDir()
}

// collectJars 是 jarsIn 的内核：列出 dir 下可执行 jar 的 item 列表。
func collectJars(dir string) []item {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []item
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(n), ".jar") {
			continue
		}
		lower := strings.ToLower(n)
		if strings.HasSuffix(lower, "-sources.jar") || strings.HasSuffix(lower, "-javadoc.jar") ||
			strings.HasSuffix(lower, "-plain.jar") || strings.Contains(lower, ".original") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		found = append(found, item{filepath.Join(dir, n), info.ModTime().UnixNano()})
	}
	return found
}

// jarsIn 返回按修改时间倒序的可执行 jar（排除 sources/javadoc/war 等派生物）。
func jarsIn(dir string) []string {
	found := collectJars(dir)
	sort.Slice(found, func(i, j int) bool { return found[i].mod > found[j].mod })
	out := make([]string, 0, len(found))
	for _, f := range found {
		out = append(out, f.path)
	}
	return out
}

// ---- package.json ----

type pkgJSON struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Main         string            `json:"main"`
	Scripts      map[string]string `json:"scripts"`
	DevDeps      map[string]string `json:"devDependencies"`
	Dependencies map[string]string `json:"dependencies"`
}

func fromNode(dir string) (*buildInfo, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var pkg pkgJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("package.json 解析失败: %w", err)
	}
	pm := nodePackageManager(dir)
	script := firstScript(pkg.Scripts, "dev", "start", "serve")
	info := &buildInfo{kind: project.KindNode, name: pkg.Name}
	if script != "" {
		info.configs = append(info.configs, project.LaunchConfig{
			ID: "node-" + script, Label: pm + " run " + script, Task: "run " + script,
			Port: nodePort(pkg, dir),
		})
	} else if pkg.Main != "" {
		info.configs = append(info.configs, project.LaunchConfig{
			ID: "node-main", Label: "node " + pkg.Main, Task: pkg.Main, Port: nodePort(pkg, dir),
		})
	}
	info.notes = append(info.notes, "包管理器判定为 "+pm)
	return info, nil
}

func nodePackageManager(dir string) string {
	switch {
	case has(dir, "pnpm-lock.yaml"):
		return "pnpm"
	case has(dir, "yarn.lock"):
		return "yarn"
	case has(dir, "bun.lockb"), has(dir, "bun.lock"):
		return "bun"
	default:
		return "npm"
	}
}

func firstScript(scripts map[string]string, order ...string) string {
	for _, k := range order {
		if v, ok := scripts[k]; ok && strings.TrimSpace(v) != "" {
			return k
		}
	}
	return ""
}

func nodePort(pkg pkgJSON, dir string) int {
	for _, k := range []string{"dev", "start", "serve"} {
		if p := portFromText(pkg.Scripts[k]); p > 0 {
			return p
		}
	}
	if p := mavenServerPort(dir, ""); p > 0 {
		return p
	}
	return 0
}

// ---- 端口与名称读取（不引入 YAML 依赖，按行扫描） ----

var rePort = regexp.MustCompile(`(?i)(?:--port|--server\.port|-p)[\s=]+(\d{2,5})`)
var reYamlPort = regexp.MustCompile(`^[ \t]+port:\s*['"]?(\d{2,5})`)
var reYamlServer = regexp.MustCompile(`^\s*server:\s*$`)
var rePropsPort = regexp.MustCompile(`(?m)^\s*server\.port\s*[=:]\s*(\d{1,5})`)

// javaClassName / gradleRootName 都跑在 Detect 的项目遍历里，一次 add 可能触发
// 很多次；把正则提到包级避免每次调用都重编译一次。
var reJavaPackage = regexp.MustCompile(`(?m)^\s*package\s+([\w.]+)\s*;`)
var reGradleRoot = regexp.MustCompile(`rootProject\.name\s*=\s*["']([^"']+)["']`)

// mavenServerPort 读取 src/main/resources 下的 application[.yml|yaml|properties]。
// profile 非空时优先读 application-<profile>.*（后者覆盖前者）。
func mavenServerPort(dir, profile string) int {
	res := filepath.Join(dir, "src", "main", "resources")
	names := []string{"application.yml", "application.yaml", "application.properties"}
	if profile == "" {
		// jeecg 这类工程把 server.port 放在 application-dev.yml，基础文件里只有
		// spring.profiles.active。不顺着它读，添加项目时报不出端口，用户只能手工 -port。
		profile = springActiveProfile(filepath.Join(res, "application.yml"))
	}
	if profile != "" {
		names = append([]string{
			"application-" + profile + ".yml",
			"application-" + profile + ".yaml",
			"application-" + profile + ".properties",
		}, names...)
	}
	for _, n := range names {
		if p := portInFile(filepath.Join(res, n)); p > 0 {
			return p
		}
	}
	return 0
}

var reActiveProfile = regexp.MustCompile(`(?m)^[ \t]*active:\s*['"]?([A-Za-z0-9_,\-.]+)`)

// springActiveProfile 取 spring.profiles.active 的第一个 profile 名。
func springActiveProfile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	m := reActiveProfile.FindStringSubmatch(strings.ReplaceAll(string(raw), "\r", ""))
	if m == nil {
		return ""
	}
	return strings.Split(m[1], ",")[0]
}

func jarServerPort(dir string) int { return mavenServerPort(dir, "") }

func portInFile(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return portFromText(string(raw))
}

// portFromText 依次尝试：YAML 的 server: / port: 嵌套 → properties 的 server.port= →
// 命令行里的 --port/-p。顺序不能反：宽松正则会误抓 mail.port、grpc.port 这类同名字段。
func portFromText(s string) int {
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	inServer := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if inServer {
			indent := len(ln) - len(strings.TrimLeft(ln, " \t"))
			if trimmed != "" && indent == 0 {
				inServer = false
			} else if m := reYamlPort.FindStringSubmatch(ln); m != nil {
				return normalizePort(m[1])
			}
		}
		if reYamlServer.MatchString(ln) {
			inServer = true
		}
	}
	if m := rePropsPort.FindStringSubmatch(s); m != nil {
		return normalizePort(m[1])
	}
	if m := rePort.FindStringSubmatch(s); m != nil {
		return normalizePort(m[1])
	}
	return 0
}

func normalizePort(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n >= 65536 {
		return 0
	}
	return n
}

func gradleRootName(dir string) string {
	for _, n := range []string{"settings.gradle.kts", "settings.gradle"} {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		if m := reGradleRoot.FindSubmatch(raw); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// ---- pom.xml ----

type pom struct {
	artifactId string
	packaging  string
	modules    []string
	springBoot bool
}

type xmlNode struct {
	XMLName  xml.Name
	Content  string     `xml:",chardata"`
	Children []*xmlNode `xml:",any"`
}

func parsePom(path string) (*pom, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root xmlNode
	if err := xml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("pom.xml 解析失败: %w", err)
	}
	out := &pom{}
	if root.XMLName.Local != "project" {
		return nil, fmt.Errorf("%s 根节点不是 <project>", path)
	}
	for _, c := range root.Children {
		if c.XMLName.Local == "artifactId" && out.artifactId == "" {
			out.artifactId = strings.TrimSpace(c.Content)
		}
		if c.XMLName.Local == "packaging" {
			out.packaging = strings.TrimSpace(c.Content)
		}
		if c.XMLName.Local == "modules" {
			for _, m := range c.Children {
				if m.XMLName.Local == "module" {
					if name := strings.TrimSpace(m.Content); name != "" {
						out.modules = append(out.modules, name)
					}
				}
			}
		}
	}
	// spring-boot-starter-parent 或 spring-boot-maven-plugin 任一命中即认为是可运行 Spring Boot 工程
	if strings.Contains(string(raw), "spring-boot-maven-plugin") || strings.Contains(string(raw), "spring-boot-starter-parent") {
		out.springBoot = true
	}
	if out.artifactId == "" {
		out.artifactId = filepath.Base(filepath.Dir(path))
	}
	return out, nil
}

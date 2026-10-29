package proc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"runlite/internal/project"
)

// PreparedLaunch 是一次"单 JVM 直跑 main"的成果：已经把 classpath 解析好、把 Java 参数
// 写进了 argfile，Start 只要拿它 exec 即可。区别于 buildArgv（纯拼命令行，不碰 I/O），
// 这一步要跑一次 mvn，所以必须在 Start 拿全局锁之前做完。
type PreparedLaunch struct {
	Name string   // 执行体：java
	Args []string // 形如 ["@<runs>/xxx.argfile"]
}

// plArg 从 cfg.Args 里取 -pl 后面的模块相对路径；没有就返回空（单模块工程）。
func plArg(args []string) string {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-pl" {
			return args[i+1]
		}
	}
	return ""
}

// writeDirectMainArgfile 把 java 的命令行写进一个 @argfile（Java 9+），
// 用它来彻底绕开 Windows CreateProcess 的命令行长度上限（error 206）。
//
// argfile 格式：一行一个参数；classpath 用双引号包成一个 token，引号内的反斜杠与
// 空格都是字面量，所以 Windows 路径（C:\Users\...）和带空格的 .m2 目录都安全。
func writeDirectMainArgfile(path, classesDir, cp, mainClass, profile string, extra []string) error {
	var sb strings.Builder
	jarg := func(s string) string { return "\"" + s + "\"" }
	fullCP := classesDir + string(os.PathListSeparator) + cp
	sb.WriteString("-classpath\n")
	sb.WriteString(jarg(fullCP) + "\n")
	sb.WriteString(mainClass + "\n")
	if profile != "" {
		sb.WriteString("--spring.profiles.active=" + profile + "\n")
	}
	for _, a := range extra {
		sb.WriteString(a + "\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o600)
}

// mvnClasspath 跑一次 Maven：先编译（process-classes），再用 dependency:build-classpath
// 把运行期依赖的完整 classpath 写到 cpFile。多模块 reactor 会带上 -am，让构建把同 reactor
// 里被依赖的模块也做出来，否则解析出的 classpath 会缺内部的模块。
func (m *Manager) mvnClasspath(cfg *project.LaunchConfig, dir, cpFile string) error {
	args := append([]string{}, cfg.Args...)
	if plArg(cfg.Args) != "" {
		args = append(args, "-am")
	}
	args = append(args, "process-classes", "dependency:build-classpath", "-Dmdep.outputFile="+cpFile)

	cmd := exec.Command("mvn", args...)
	cmd.Dir = dir
	cmd.Env = buildEnv(project.KindMaven, cfg, m.report())
	decorate(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mvn 解析 classpath 失败: %w\n%s", err, truncate(string(out), 400))
	}
	return nil
}

// prepareDirectMain 是单 JVM 直跑的准备工作：解析 classpath → 写 argfile → 交出 argv。
// 这一步要跑 mvn（可能几秒到几十秒），调用方必须保证没有持着 Manager 的全局锁。
func (m *Manager) prepareDirectMain(cfg *project.LaunchConfig, dir string, extra []string) (*PreparedLaunch, error) {
	if err := os.MkdirAll(m.opt.RunsDir, 0o700); err != nil {
		return nil, err
	}
	cpFile := filepath.Join(m.opt.RunsDir, sanitize(cfg.ID)+"-classpath.txt")
	if err := m.mvnClasspath(cfg, dir, cpFile); err != nil {
		return nil, err
	}
	cpRaw, err := os.ReadFile(cpFile)
	if err != nil {
		return nil, fmt.Errorf("读 classpath 文件失败: %w", err)
	}
	cp := strings.TrimSpace(string(cpRaw))
	if cp == "" {
		return nil, errors.New("mvn 解析出的 classpath 为空，无法启动")
	}
	// 依赖 classpath 之外，还要把本模块自己编译出来的 target/classes 加进去：
	// dependency:build-classpath 只列依赖 jar，不含应用自身的类。
	modRel := plArg(cfg.Args)
	classesDir := filepath.Join(modRel, "target", "classes")

	argFile := filepath.Join(m.opt.RunsDir, sanitize(cfg.ID)+"-"+time.Now().Format("20060102-150405")+".argfile")
	if err := writeDirectMainArgfile(argFile, classesDir, cp, cfg.MainClass, cfg.Profile, extra); err != nil {
		return nil, err
	}
	return &PreparedLaunch{Name: "java", Args: []string{"@" + argFile}}, nil
}

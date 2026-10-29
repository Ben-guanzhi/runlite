package proc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"runlite/internal/project"
)

// TestWriteDirectMainArgfile 盯住单 JVM 直跑的 @argfile 内容：classpath 必须用双引号包成
// 一个 token（否则 Windows 路径里的空格、反斜杠会把 java 的参数解析拆坏），main 类和
// profile 各占一行。这个文件是 java 真正执行的命令行，写错了应用起不来。
func TestWriteDirectMainArgfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.argfile")
	classesDir := `target/classes`
	cp := `C:\Users\11518\.m2\repo\a\b.jar;C:\Users\11518\.m2\repo c\x.jar`
	mainClass := `com.example.lgsy.LgsyApplication`
	if err := writeDirectMainArgfile(path, classesDir, cp, mainClass, "dev", []string{"--server.port=18082"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	// classpath 是双引号包起来的一个 token，且含 ; 分隔与带空格的路径
	if !strings.Contains(got, "-classpath\n\"target/classes;"+cp+"\"\n") {
		t.Errorf("argfile 的 classpath 行不对:\n%s", got)
	}
	if !strings.Contains(got, mainClass+"\n") {
		t.Errorf("缺 main class 行:\n%s", got)
	}
	if !strings.Contains(got, "--spring.profiles.active=dev\n") {
		t.Errorf("缺 profile 行:\n%s", got)
	}
	if !strings.Contains(got, "--server.port=18082\n") {
		t.Errorf("缺附加参数行:\n%s", got)
	}
}

// TestWriteDirectMainArgfileNoProfile 没有 profile 时不该出现 spring.profiles 行。
func TestWriteDirectMainArgfileNoProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.argfile")
	if err := writeDirectMainArgfile(path, "target/classes", "a.jar", "com.x.App", "", nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "spring.profiles") {
		t.Errorf("没有 profile 却写了 spring.profiles 行:\n%s", raw)
	}
}

// TestPlArg 盯住从 cfg.Args 里取 -pl 后面的模块路径；没有则返回空。
func TestPlArg(t *testing.T) {
	if got := plArg([]string{"-pl", "mod/a"}); got != "mod/a" {
		t.Errorf("plArg = %q, 期望 mod/a", got)
	}
	if got := plArg([]string{"clean", "install"}); got != "" {
		t.Errorf("plArg = %q, 期望空", got)
	}
	if got := plArg(nil); got != "" {
		t.Errorf("plArg(nil) = %q, 期望空", got)
	}
}

// TestBuildArgvRejectsDirectMain 盯住：direct-main 配置不能走纯命令行的 buildArgv，
// 必须经 prepareDirectMain 解析（跑 mvn 拿 classpath、写 argfile）——否则会把
// Task("direct-main") 当成一个 maven goal 去 exec，报一堆奇怪的错。
func TestBuildArgvRejectsDirectMain(t *testing.T) {
	cfg := &project.LaunchConfig{ID: "direct-main", Task: "direct-main", MainClass: "com.x.App"}
	if _, _, err := buildArgv(project.KindMaven, cfg, ".", nil); err == nil {
		t.Fatal("direct-main 配置不该被 buildArgv 直接处理")
	}
}

// TestSingleJVM 盯住 singleJVM 判定：只对 maven 的 spring-boot:run 生效，SingleJVM 显式
// *false 就恢复 fork。
func TestSingleJVM(t *testing.T) {
	if !singleJVM(project.KindMaven, &project.LaunchConfig{}, "spring-boot:run") {
		t.Error("maven spring-boot:run 默认应单 JVM")
	}
	if singleJVM(project.KindMaven, &project.LaunchConfig{}, "package") {
		t.Error("非 spring-boot:run 的 goal 不该加 fork=false")
	}
	f := false
	if singleJVM(project.KindMaven, &project.LaunchConfig{SingleJVM: &f}, "spring-boot:run") {
		t.Error("SingleJVM=false 应恢复 fork")
	}
	if singleJVM(project.KindGradle, &project.LaunchConfig{}, "spring-boot:run") {
		t.Error("gradle 不走 fork=false")
	}
}

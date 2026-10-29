package detect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"runlite/internal/project"
)

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectMavenSpringBoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <parent>
    <groupId>org.springframework.boot</groupId>
    <artifactId>spring-boot-starter-parent</artifactId>
    <version>3.2.0</version>
  </parent>
  <artifactId>demo-service</artifactId>
  <build><plugins><plugin>
    <groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId>
  </plugin></plugins></build>
</project>`)
	write(t, dir, "src/main/resources/application.yml", "server:\n  port: 18081\nspring:\n  application:\n    name: demo\n")
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Project
	if p.Kind != "maven" {
		t.Fatalf("kind = %s, 期望 maven", p.Kind)
	}
	if p.Name != "demo-service" {
		t.Errorf("name = %q, 期望 demo-service（必须跳过 <parent> 里的 artifactId）", p.Name)
	}
	cfg, err := p.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Task != "spring-boot:run" {
		t.Errorf("task = %q, 期望 spring-boot:run", cfg.Task)
	}
	if cfg.Port != 18081 {
		t.Errorf("port = %d, 期望 18081（YAML 嵌套 server.port）", cfg.Port)
	}
}

func TestDetectJavaWinsOverPackageJSON(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<project><artifactId>mixed</artifactId></project>`)
	write(t, dir, "package.json", `{"name":"frontend","scripts":{"dev":"vite"}}`)
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project.Kind != "maven" {
		t.Fatalf("kind = %s, 期望 maven（Java 优先规则）", got.Project.Kind)
	}
	if !containsNote(got.Project.Notes, "按 Java 项目处理") {
		t.Errorf("notes 未提示混合目录: %v", got.Project.Notes)
	}
}

func TestDetectGradleWrapperAndPropertiesPort(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "build.gradle", `plugins { id 'org.springframework.boot' version '3.2.0' }`)
	write(t, dir, "settings.gradle", `rootProject.name = 'hg-gateway'`)
	write(t, dir, "gradlew.bat", "@echo off\n")
	write(t, dir, "src/main/resources/application.properties", "# c\nserver.port=9999\n")
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project.Name != "hg-gateway" {
		t.Errorf("name = %q", got.Project.Name)
	}
	cfg, _ := got.Project.ActiveConfig()
	if cfg.Task != "bootRun" || cfg.Port != 9999 {
		t.Errorf("cfg = %+v, 期望 bootRun/9999", cfg)
	}
}

func TestDetectNodePnpmDevScript(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"web","scripts":{"build":"vite build","dev":"vite --port 5173"}}`)
	write(t, dir, "pnpm-lock.yaml", "")
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := got.Project.ActiveConfig()
	if cfg.Task != "run dev" {
		t.Errorf("task = %q, 期望 run dev", cfg.Task)
	}
	if cfg.Port != 5173 {
		t.Errorf("port = %d, 期望 5173", cfg.Port)
	}
	if !containsNote(got.Project.Notes, "pnpm") {
		t.Errorf("notes 未记录包管理器: %v", got.Project.Notes)
	}
}

func TestDetectJarSkipsSourcesArtifacts(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "target/app-1.0.0-sources.jar", "x")
	write(t, dir, "target/app-1.0.0.jar", "x")
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Project.Kind != "jar" {
		t.Fatalf("kind = %s", got.Project.Kind)
	}
	cfg, _ := got.Project.ActiveConfig()
	if cfg.Task != "-jar target/app-1.0.0.jar" {
		t.Errorf("task = %q", cfg.Task)
	}
}

func TestDetectRejectsUnknownDir(t *testing.T) {
	_, err := Detect(t.TempDir())
	if err == nil {
		t.Fatal("空目录应报错，不能静默注册不可用项目")
	}
}

func TestDetectProfileOverridesBasePort(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<project><artifactId>x</artifactId>
	<build><plugins><plugin><artifactId>spring-boot-maven-plugin</artifactId></plugin></plugins></build></project>`)
	write(t, dir, "src/main/resources/application.yml", "server:\n  port: 8080\n")
	write(t, dir, "src/main/resources/application-dev.yml", "server:\n  port: 8081\n")
	if got := mavenServerPort(dir, "dev"); got != 8081 {
		t.Errorf("profile 端口 = %d, 期望 8081", got)
	}
	if got := mavenServerPort(dir, ""); got != 8080 {
		t.Errorf("默认端口 = %d, 期望 8080", got)
	}
}

func TestPortFromTextIgnoresCommentsAndLookalikes(t *testing.T) {
	if p := portFromText("# server:\n#   port: 1234\n"); p != 0 {
		t.Errorf("注释里的端口被读成 %d", p)
	}
	if p := portFromText("server:\n  port: 8080\nmanagement:\n  port: 9090\n"); p != 8080 {
		t.Errorf("= %d, 期望 8080", p)
	}
	if p := portFromText("mail:\n  port: 25\nserver:\n  port: 8080\n"); p != 8080 {
		t.Errorf("= %d, 期望 8080（不能被 mail.port 抢先）", p)
	}
	if p := portFromText("server.port : 8999\n"); p != 8999 {
		t.Errorf("= %d, 期望 8999（YAML 冒号写法）", p)
	}
	if p := portFromText("vite --port 5173 --host"); p != 5173 {
		t.Errorf("= %d, 期望 5173", p)
	}
}

// TestDetectMavenPortFromActiveProfile 盯住本机 lgsy-app 的实际布局：
// application.yml 只有 spring.profiles.active: dev，server.port 在 application-dev.yml。
// 不顺着 active 去读，添加项目时报不出端口，用户只能每次手工 --port。
func TestDetectMavenPortFromActiveProfile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>demo</artifactId>
  <build><plugins><plugin>
    <groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId>
  </plugin></plugins></build>
</project>`)
	write(t, dir, "src/main/resources/application.yml", "spring:\n  profiles:\n    active: dev\n")
	write(t, dir, "src/main/resources/application-dev.yml", "server:\n  port: 18083\n")
	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := got.Project.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 18083 {
		t.Fatalf("port = %d, 期望 18083（从 spring.profiles.active 指向的 application-dev.yml 读出）", cfg.Port)
	}

	// 反面对照：active 指向一个不存在的 profile 时不能凭空报出端口，
	// 否则这个改动只是让所有项目都拿到一个假数字。
	write(t, dir, "src/main/resources/application.yml", "spring:\n  profiles:\n    active: nosuch\n")
	got2, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := got2.Project.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Port != 0 {
		t.Errorf("port = %d, 期望 0（application-nosuch.yml 不存在，基础文件里也没有 server.port）", cfg2.Port)
	}
}

func containsNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestDetectMultiModuleBoot 钉住真实形态的 reactor（本机 D:\liugang\lgsy-app 就是这样）：
// 根 pom 只 <modules> 聚合，启动模块藏在两级目录里。只看根 pom 时 runlite 认为它
// "没有可跑配置"，start 于是去 mvn package——用户要的"把服务起起来"从来没发生过。
func TestDetectMultiModuleBoot(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app</artifactId><packaging>pom</packaging>
  <modules><module>jeecg-boot-msg-po</module><module>lgsy-app-module</module></modules>
</project>`)
	write(t, dir, "jeecg-boot-msg-po/pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>jeecg-boot-msg-po</artifactId></project>`)
	write(t, dir, "lgsy-app-module/pom.xml", `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app-module</artifactId><packaging>pom</packaging>
  <modules><module>lgsy-app-start</module></modules>
</project>`)
	write(t, dir, "lgsy-app-module/lgsy-app-start/pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app-start</artifactId>
  <build><plugins><plugin>
    <groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId>
  </plugin></plugins></build>
</project>`)
	write(t, dir, "lgsy-app-module/lgsy-app-start/src/main/resources/application.yml", "server:\n  port: 18082\n")

	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := got.Project.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ID != "spring-boot-run" {
		t.Fatalf("默认配置 = %q, 期望 spring-boot-run（当前配置: %+v）", cfg.ID, got.Project.Configs)
	}
	if cfg.Task != "spring-boot:run" {
		t.Errorf("task = %q, 期望 spring-boot:run", cfg.Task)
	}
	if strings.Join(cfg.Args, " ") != "-pl lgsy-app-module/lgsy-app-start" {
		t.Errorf("args = %v, 期望只选中启动模块（否则 mvn 会去跑根目录，起不来服务）", cfg.Args)
	}
	if cfg.Port != 18082 {
		t.Errorf("port = %d, 期望 18082（端口在读启动模块的 application.yml，不是根目录）", cfg.Port)
	}
	if !containsNote(got.Project.Notes, "lgsy-app-module/lgsy-app-start") {
		t.Errorf("notes = %q, 期望说明启动模块位置", got.Project.Notes)
	}
	// 反面对照：这条 -pl 路径不能被所有项目共用。纯聚合、模块里也没有 boot 插件时，
	// 必须回到"只有 package"的旧结论，不然每个 maven 项目都会得到一个假的可跑配置。
	write(t, dir, "lgsy-app-module/lgsy-app-start/pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app-start</artifactId></project>`)
	got2, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg2, err := got2.Project.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.Task == "spring-boot:run" {
		t.Errorf("模块里已删掉 boot 插件，仍然给出 spring-boot:run：args=%v", cfg2.Args)
	}
	if !containsNote(got2.Project.Notes, "未声明 spring-boot-maven-plugin") {
		t.Errorf("notes = %q, 期望回到没有可跑配置的提示", got2.Project.Notes)
	}
}

// TestDetectPrefersJavaJarWhenAssembledJarExists 钉住默认配置的选择：
// 当检测到可运行的装配 jar（target/dist/**/app.jar 带着 lib/）时，默认配置必须是
// java-jar 而不是 spring-boot:run——后者在 Windows 上对这种大项目必然报 CreateProcess
// error=206（命令行长到爆），GUI 点「启动」就永远失败。有现成的可跑 jar 就用它。
func TestDetectPrefersJavaJarWhenAssembledJarExists(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app</artifactId><packaging>pom</packaging>
  <modules><module>lgsy-app-module</module></modules>
</project>`)
	write(t, dir, "lgsy-app-module/pom.xml", `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app-module</artifactId><packaging>pom</packaging>
  <modules><module>lgsy-app-start</module></modules>
</project>`)
	write(t, dir, "lgsy-app-module/lgsy-app-start/pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>lgsy-app-start</artifactId>
  <build><plugins><plugin>
    <groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId>
  </plugin></plugins></build>
</project>`)
	write(t, dir, "lgsy-app-module/lgsy-app-start/src/main/resources/application.yml", "server:\n  port: 8080\n")
	// 装配产物：target/dist/app/app.jar + 同级 lib/。
	write(t, dir, "lgsy-app-module/lgsy-app-start/target/dist/app/app.jar", "dummy")
	write(t, dir, "lgsy-app-module/lgsy-app-start/target/dist/app/lib/dep.jar", "dep")

	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	active, err := got.Project.ActiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != "java-jar" {
		t.Fatalf("默认配置 = %q, 期望 java-jar（有可运行装配 jar 时 GUI 启动才不撞 Windows 206）; configs=%+v", active.ID, got.Project.Configs)
	}
}

// TestFirstBuiltJarPrefersAssembled 钉住"挑能直接 java -jar 跑的那个 jar"：
// jeecg-boot 会把装配好的产物放在 target/dist/<name>/app.jar，同级带 lib/ 和 resources/，
// 它才是能跑的；target/ 下那个裸 app.jar 的 manifest 也指着 lib/，旁边却没有，跑不动。
// 之前 firstBuiltJar 只扫 target/ 顶层，捡回那个裸 jar，GUI 的「启动」就点了一个起不来的东西。
func TestFirstBuiltJarPrefersAssembled(t *testing.T) {
	dir := t.TempDir()
	// 裸 jar：没有同级 lib/，不可直接运行。
	write(t, dir, "target/app.jar", "dummy")
	// 装配产物：target/dist/app/app.jar + 同级 lib/ + resources/，可运行。
	write(t, dir, "target/dist/app/app.jar", "dummy")
	write(t, dir, "target/dist/app/lib/any.jar", "dep")
	write(t, dir, "target/dist/app/resources/application.yml", "x")

	got := firstBuiltJar(dir)
	want := filepath.Join(dir, "target", "dist", "app", "app.jar")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("firstBuiltJar = %q, 期望装配产物 %q", got, want)
	}
}

// TestFirstBuiltJarFallsBackToPlain 没有装配目录时退回 target/ 顶层的 jar。
func TestFirstBuiltJarFallsBackToPlain(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "target/app.jar", "dummy")
	if got := firstBuiltJar(dir); !strings.HasSuffix(filepath.ToSlash(got), "target/app.jar") {
		t.Errorf("无装配目录时该退回 target/app.jar，实得 %q", got)
	}
}

// TestFindMainClass 盯住 direct-main 的启动类识别：从 src/main/java 里按 @SpringBootApplication
// 找 main 类，返回全限定名；没有 src 或没有注解就不给，绝不瞎猜。
func TestFindMainClass(t *testing.T) {
	dir := t.TempDir()
	// 还没有源码：不该有启动类
	if got := findMainClass(dir); got != "" {
		t.Fatalf("没有 src 时 findMainClass = %q，期望空", got)
	}
	write(t, dir, "src/main/java/com/example/lgsy/LgsyApplication.java", `package com.example.lgsy;

import org.springframework.boot.autoconfigure.SpringBootApplication;

@SpringBootApplication
public class LgsyApplication {
    public static void main(String[] args) {}
}
`)
	if got := findMainClass(dir); got != "com.example.lgsy.LgsyApplication" {
		t.Errorf("findMainClass = %q, 期望 com.example.lgsy.LgsyApplication", got)
	}
	// 无包名也能还原类名
	write(t, dir, "src/main/java/BareApp.java", "@SpringBootApplication public class BareApp {}")
	if got := findMainClass(dir); got != "BareApp" {
		t.Errorf("无包名时 findMainClass = %q, 期望 BareApp", got)
	}
}

// TestDetectDirectMainConfig 盯住 detect 会给 Spring Boot 工程生成 direct-main 配置：
// MainClass 来自源码扫描，Task 是 direct-main，多模块还要带上 -pl。且默认配置不受影响
// （direct-main 是候选，不是默认）。
func TestDetectDirectMainConfig(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "pom.xml", `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <artifactId>demo</artifactId>
  <build><plugins><plugin>
    <groupId>org.springframework.boot</groupId><artifactId>spring-boot-maven-plugin</artifactId>
  </plugin></plugins></build>
</project>`)
	write(t, dir, "src/main/resources/application.yml", "server:\n  port: 19090\n")
	write(t, dir, "src/main/java/com/example/demo/DemoApplication.java", `package com.example.demo;
@SpringBootApplication
public class DemoApplication { public static void main(String[] args){} }`)

	got, err := Detect(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Project
	var dm *project.LaunchConfig
	for i := range p.Configs {
		if p.Configs[i].ID == "direct-main" {
			dm = &p.Configs[i]
			break
		}
	}
	if dm == nil {
		t.Fatalf("没有生成 direct-main 配置，configs=%+v", p.Configs)
	}
	if dm.MainClass != "com.example.demo.DemoApplication" {
		t.Errorf("direct-main MainClass = %q", dm.MainClass)
	}
	if dm.Task != "direct-main" {
		t.Errorf("direct-main task = %q, 期望 direct-main", dm.Task)
	}
	if dm.Port != 19090 {
		t.Errorf("direct-main port = %d, 期望 19090", dm.Port)
	}
	// 默认配置仍是 spring-boot-run（direct-main 是候选，不抢默认）
	active, _ := p.ActiveConfig()
	if active.ID != "spring-boot-run" {
		t.Errorf("默认配置 = %q, 期望 spring-boot-run（direct-main 是候选）", active.ID)
	}
}

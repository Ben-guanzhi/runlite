package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"runlite/internal/config"
)

func TestMissingToolAdviceIsActionable(t *testing.T) {
	r := &Report{
		OS: "windows", Arch: "amd64", CheckedAt: time.Unix(0, 0),
		Tools: []Tool{
			{Name: "java", Err: "PATH 中未找到 java"},
			{Name: "mvn", Err: "PATH 中未找到 mvn"},
			{Name: "node", Bin: `C:\node\node.exe`, Version: "v22.22.3"},
		},
	}
	out := r.String()

	// 没装的工具必须各带一条"怎么办"；装上的不要凑热闹
	if n := strings.Count(out, "怎么办"); n != 2 {
		t.Errorf("建议条数 = %d, 期望 2（只给没装的工具）\n%s", n, out)
	}
	if !strings.Contains(out, `runlite add`) {
		t.Error("mvn 缺失时没给出绕过 Maven 的可执行出路，用户只能干等装 Maven")
	}

	// 负对照：键名写错的旧文案不能复活。这台机器上真照做过一次，settings.json 里
	// 根本不存在 jdkPath 这个键，用户照做等于白做。
	if strings.Contains(out, "jdkPath") {
		t.Errorf("建议里出现了不存在的配置键 jdkPath:\n%s", out)
	}
	for _, name := range []string{"java", "mvn"} {
		if !strings.Contains(out, name) {
			t.Errorf("输出里没有 %s 那一行", name)
		}
	}
}

// TestAdviceKeyRoundTripsThroughSettings 钉住"建议里的键名"与"实际解析的键名"是同一个。
// 光看 hint 文案看不出键名对不对，必须让真实配置文件走一遍加载。
func TestAdviceKeyRoundTripsThroughSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"toolPaths":{"java":"C:/jdk/bin/java.exe"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	set, err := config.LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := set.JdkPaths["java"]; got != "C:/jdk/bin/java.exe" {
		t.Fatalf("settings.json 的 toolPaths 没被读出来: %+v", set.JdkPaths)
	}
	if h := hint("java"); !strings.Contains(h, `"toolPaths"`) {
		t.Errorf("java 的建议没写真实键名 toolPaths: %q", h)
	}

	// 反面对照：换成旧键名必须读不到，否则上面那条断言是空针。
	if err := os.WriteFile(path, []byte(`{"jdkPath":"C:/jdk/bin/java.exe"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	set2, err := config.LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(set2.JdkPaths) != 0 {
		t.Errorf("旧键名 jdkPath 也被吃掉了（%+v），说明 toolPaths 这条针没有判别力", set2.JdkPaths)
	}
}

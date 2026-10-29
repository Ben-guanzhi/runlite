package netx

import (
	"runtime"
	"testing"
)

// 真实 `netstat -ano -p tcp` 片段（Windows 中文/英文表头一致，行格式相同）
const sample = `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       928
  TCP    0.0.0.0:445            0.0.0.0:0              LISTENING       4
  TCP    0.0.0.0:18080          0.0.0.0:0              LISTENING       25460
  TCP    127.0.0.1:54321        127.0.0.1:54322        ESTABLISHED     1234
  TCP    [::]:135               [::]:0                 LISTENING       928
  TCP    [::]:47613             [::]:0                 LISTENING       25272
  TCP    [::1]:18080            [::1]:55000            TIME_WAIT       0
  UDP    0.0.0.0:5353           *:*                                    4
`

func TestParseNetstatTCP(t *testing.T) {
	got := parseNetstatTCP(sample)
	// 135 同时有 IPv4/IPv6 两条监听，是真实存在的两个 socket，必须都保留
	want := []int{135, 135, 445, 18080, 47613}
	if len(got) != len(want) {
		t.Fatalf("解析出 %d 条: %+v", len(got), got)
	}
	for i, w := range want {
		if got[i].Port != w {
			t.Errorf("第 %d 条端口 = %d, 期望 %d", i, got[i].Port, w)
		}
	}
	// 关键负例：ESTABLISHED / TIME_WAIT / UDP 都不能被当成监听
	for _, l := range got {
		if l.PID == 0 || l.PID == 1234 {
			t.Errorf("混进了非监听条目: %+v", l)
		}
	}
	if got[3].PID != 25460 {
		t.Errorf("18080 的 PID = %d, 期望 25460", got[3].PID)
	}
}

func TestPortOfIPv6AndEdgeCases(t *testing.T) {
	if p, ok := portOf("[::]:47613"); !ok || p != 47613 {
		t.Errorf("IPv6 解析 = %d/%v", p, ok)
	}
	if _, ok := portOf("0.0.0.0:0"); ok {
		t.Error("远端 0.0.0.0:0 不应被当作端口")
	}
	if _, ok := portOf("127.0.0.1:"); ok {
		t.Error("空端口不应通过")
	}
}

func TestNewPortsDiff(t *testing.T) {
	before := map[int]int{135: 1, 8080: 2}
	after := map[int]int{135: 1, 8080: 2, 18080: 9}
	diff := NewPorts(before, after)
	if len(diff) != 1 || diff[0] != 18080 {
		t.Errorf("差集 = %v, 期望 [18080]", diff)
	}
}

// TestListenersAgainstRealHost 只在 Windows 上跑：确认解析器对真实 netstat 输出有效。
func TestListenersAgainstRealHost(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("netstat 解析路径仅 Windows")
	}
	ls, err := Listeners()
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) == 0 {
		t.Fatal("本机一个监听端口都没有，解析器或命令有问题")
	}
	for _, l := range ls {
		if l.Port <= 0 || l.PID <= 0 {
			t.Fatalf("解析出非法条目: %+v", l)
		}
	}
}

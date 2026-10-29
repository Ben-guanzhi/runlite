package daemon

import "testing"

// TestSameHome 钉住"同一个目录别被说成两个"：RUNLITE_HOME 里正反斜杠、结尾多一个分隔符、
// 大小写不一样都是常见写法，判错会把用户引去停一个根本没冲突的 daemon。
func TestSameHome(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{`C:\Users\me\.runlite`, `C:/Users/me/.runlite`, true},
		{`D:/tmp/runlite-home`, `D:/tmp/runlite-home/`, true},
		{`d:\tmp\runlite-home`, `D:/tmp/runlite-home`, true},
		{`D:/tmp/runlite-home`, `D:/tmp/other-home`, false},
		{`D:/tmp/runlite-home`, `D:/tmp/runlite-home-old`, false},
	}
	for _, c := range cases {
		if got := sameHome(c.a, c.b); got != c.want {
			t.Errorf("sameHome(%q, %q) = %v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

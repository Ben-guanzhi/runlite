package logs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestReadFileRoundTrip 盯住 daemon 重启后仍能读到日志这条路：
// 落盘格式必须能还原出与内存一致的正文、时间戳、stderr 标记和行号。
func TestReadFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	l, err := Create(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(false, "开始")
	l.Append(true, "警告")
	l.Append(false, "  缩进正文")
	l.Append(false, "结束")
	inMem := l.Tail(0, 0)
	l.Close()

	got, err := ReadFile(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(inMem) {
		t.Fatalf("读到 %d 行，内存里是 %d 行", len(got), len(inMem))
	}
	for i := range got {
		if got[i].Seq != inMem[i].Seq || got[i].Text != inMem[i].Text || got[i].Err != inMem[i].Err {
			t.Errorf("第 %d 行不一致：文件 %+v vs 内存 %+v", i, got[i], inMem[i])
		}
		if got[i].TS != inMem[i].TS {
			t.Errorf("第 %d 行时间戳不一致：文件 %q vs 内存 %q", i, got[i].TS, inMem[i].TS)
		}
	}
	if got[1].Text != "警告" || !got[1].Err {
		t.Errorf("stderr 标记没还原：%+v", got[1])
	}
	if got[2].Text != "  缩进正文" {
		t.Errorf("正文自带的缩进被吃掉了：%q", got[2].Text)
	}
}

// TestAppendDecodesGbk 盯住管道里的 GBK：转码发生在 Append，所以内存和落盘文件
// 存的都是 UTF-8，读哪一条路都不会花屏。
func TestAppendDecodesGbk(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("GBK 还原只在 Windows 上有意义")
	}
	path := filepath.Join(t.TempDir(), "gbk.log")
	l, err := Create(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	l.Append(true, "\xd5\xe2\xca\xc7\xd2\xbb\xcc\xf5\xc2\xeb\xc1\xf7")
	got := l.Tail(0, 0)
	l.Close()
	if len(got) != 1 || got[0].Text != "这是一条码流" {
		t.Fatalf("内存里没还原：%+v", got)
	}
	fromFile, err := ReadFile(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile) != 1 || fromFile[0].Text != "这是一条码流" {
		t.Errorf("文件里没还原：%+v", fromFile)
	}
}

func TestReadFileTailAndSince(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	l, err := Create(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		l.Append(false, itoa(i))
	}
	l.Close()

	tail, err := ReadFile(path, 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 6 {
		t.Fatalf("尾部 5 行外加一条缺口提示，共 6 行，实际 %+v", tail)
	}
	if tail[0].Seq != 45 || tail[1].Text != "45" || tail[5].Text != "49" {
		t.Errorf("尾部内容不对：%+v", tail)
	}
	inc, err := ReadFile(path, 0, 47)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc) != 3 || inc[0].Seq != 48 || inc[0].Text != "47" || inc[2].Text != "49" {
		t.Errorf("增量拉取不对：%+v", inc)
	}
	none, err := ReadFile(path, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("已经读到最后一行，不该再返回内容：%+v", none)
	}
}

func TestReadFileKeepsForeignLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.log")
	raw := "12:34:56.789 正常一行\n没有时间戳的一行\n12:34:57.001 [stderr] 错误一行\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("应读到 3 行：%+v", got)
	}
	if got[0].TS != "12:34:56.789" || got[0].Text != "正常一行" || got[0].Seq != 1 {
		t.Errorf("第一行解析不对：%+v", got[0])
	}
	if got[1].Text != "没有时间戳的一行" || got[1].TS != "" {
		t.Errorf("对不上格式的旧行应原样保留：%+v", got[1])
	}
	if got[2].Text != "错误一行" || !got[2].Err {
		t.Errorf("第三行解析不对：%+v", got[2])
	}
}

// TestReleaseIdleFreesRingKeepsDiskRead 盯住内存优化那条路：已结束的日志释放环形缓冲后，
// Tail 必须透明地退回磁盘读，内容一行不少——否则 UI 会看到历史日志"凭空消失"，
// 用户会以为 daemon 把日志弄丢了。这层兜底是"释放内存"这个优化能不能落地的前提。
func TestReleaseIdleFreesRingKeepsDiskRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	l, err := Create(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		l.Append(false, itoa(i))
	}
	before := l.Tail(0, 0) // 活跃期读过：lastRead 被刷新
	l.Close()

	// 刚结束且刚读过：不该被当作空闲释放（用户可能正在收尾）
	if l.ReleaseIdle(time.Hour) {
		t.Fatal("刚读过的已结束日志不该在空闲期之前释放")
	}

	// 超过空闲门槛后才释放
	if !l.ReleaseIdle(0) {
		t.Fatal("空闲期到点却没释放环形缓冲")
	}
	if l.ring != nil {
		t.Fatal("ReleaseIdle 返回 true 却没清空 ring")
	}

	after, err := ReadFile(path, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterTail := l.Tail(0, 0) // ring 已释放，应走磁盘路径
	if len(afterTail) != len(before) {
		t.Fatalf("释放后 Tail 读到 %d 行，释放前是 %d 行", len(afterTail), len(before))
	}
	if len(after) != len(before) {
		t.Fatalf("磁盘读到 %d 行，内存是 %d 行", len(after), len(before))
	}
	for i := range before {
		if afterTail[i].Seq != before[i].Seq || afterTail[i].Text != before[i].Text {
			t.Errorf("第 %d 行：释放后 %+v vs 释放前 %+v", i, afterTail[i], before[i])
		}
	}
}

// TestBusReleaseIdle 盯住 Bus 层的批量释放：活跃日志不该被放（它要高频 Tail），
// 结束且空闲的才放。这是 daemon 周期任务真正调用的那层。
func TestBusReleaseIdle(t *testing.T) {
	b := NewBus(10)
	live, _ := Create(filepath.Join(t.TempDir(), "live.log"), 4)
	done, _ := Create(filepath.Join(t.TempDir(), "done.log"), 4)
	defer live.Close()
	defer done.Close()
	done.Append(false, "x")
	done.Tail(0, 0) // 刷新 lastRead，之后再 Close
	done.Close()
	b.Put("live", live)
	b.Put("done", done)

	if n := b.ReleaseIdle(0); n != 1 {
		t.Fatalf("释放 %d 条，期望只放已结束的 1 条", n)
	}
	if live.ring == nil {
		t.Fatal("活跃日志的环形缓冲不该被释放")
	}
	if done.ring != nil {
		t.Fatal("已结束且空闲的日志没释放环形缓冲")
	}
	if got, ok := b.Get("done"); !ok || got == nil {
		t.Fatal("释放后日志对象仍应在 Bus 里（只是内存退回磁盘）")
	}
}

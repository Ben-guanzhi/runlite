package logs

import (
	"testing"
)

// ---- stripANSI ----

func BenchmarkStripANSI_None(b *testing.B) {
	s := "hello world this is a clean line with no escapes"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = stripANSI(s)
	}
}

func BenchmarkStripANSI_Many(b *testing.B) {
	s := "\x1b[31mhello\x1b[0m \x1b[32mworld\x1b[0m \x1b[1;34mfoo\x1b[0m \x1b[0mbar\x1b[0m"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = stripANSI(s)
	}
}

func BenchmarkStripANSI_Long(b *testing.B) {
	// Simulate a long JVM stack trace with color codes
	s := "Exception in thread \"main\" java.lang.NullPointerException: Cannot invoke"
	s += " method on a null object reference\n\tat com.example.Foo.bar(Foo.java:42)\n"
	s += "\tat com.example.Baz.run(Baz.java:10)\n" + "\x1b[31mERROR\x1b[0m " + s + "\x1b[31mERROR\x1b[0m " + s
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = stripANSI(s)
	}
}

// ---- Log.Append ----

func benchmarkAppend(b *testing.B, text string) {
	path := b.TempDir() + "/test.log"
	l, err := Create(path, 2000)
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Append(false, text)
	}
}

func BenchmarkAppend_CleanLine(b *testing.B) {
	benchmarkAppend(b, "2026-10-23 14:30:00 INFO  main  Application started successfully")
}

func BenchmarkAppend_ANSI(b *testing.B) {
	benchmarkAppend(b, "\x1b[32mINFO\x1b[0m  main  \x1b[31mERROR\x1b[0m something happened")
}

func BenchmarkAppend_LongLine(b *testing.B) {
	// Simulate a long stack trace line
	text := "java.lang.RuntimeException: com.example.ServiceException: downstream timeout"
	text += " at com.example.Main.main(Main.java:10)"
	text += " Caused by: java.net.SocketTimeoutException: connect timed out"
	text += " at sun.nio.ch.SocketChannelImpl.checkConnect(Native Method)"
	text += " at java.net.Socket.implConnect(Socket.java:662)"
	text += " at java.net.Socket.connect(Socket.java:617)"
	benchmarkAppend(b, text)
}

// ---- Log.Tail ----

func BenchmarkTail_Full(b *testing.B) {
	path := b.TempDir() + "/test.log"
	l, _ := Create(path, 2000)
	defer l.Close()
	for i := 0; i < 2000; i++ {
		l.Append(false, "line "+itoa(i))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.Tail(0, 0)
	}
}

func BenchmarkTail_SmallSince(b *testing.B) {
	path := b.TempDir() + "/test.log"
	l, _ := Create(path, 2000)
	defer l.Close()
	for i := 0; i < 2000; i++ {
		l.Append(false, "line "+itoa(i))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.Tail(0, 1900)
	}
}

func BenchmarkTail_Limited(b *testing.B) {
	path := b.TempDir() + "/test.log"
	l, _ := Create(path, 2000)
	defer l.Close()
	for i := 0; i < 2000; i++ {
		l.Append(false, "line "+itoa(i))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.Tail(100, 0)
	}
}

// ---- fmtFprintln ----

func BenchmarkFmtFprintln_Stderr(b *testing.B) {
	path := b.TempDir() + "/test.log"
	l, _ := Create(path, 100)
	defer l.Close()
	ln := Line{Seq: 1, TS: "14:30:00.000", Err: true, Text: "this is an error line"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = fmtFprintln(l.w, ln)
	}
	l.w.Flush()
}

func BenchmarkFmtFprintln_Stdout(b *testing.B) {
	path := b.TempDir() + "/test.log"
	l, _ := Create(path, 100)
	defer l.Close()
	ln := Line{Seq: 1, TS: "14:30:00.000", Err: false, Text: "this is a normal line"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = fmtFprintln(l.w, ln)
	}
	l.w.Flush()
}

// ---- now ----

func BenchmarkNow_Format(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = now()
	}
}

func BenchmarkNow_Cached(b *testing.B) {
	l, _ := Create(b.TempDir()+"/test.log", 100)
	defer l.Close()
	l.mu.Lock()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = l.nowCached()
	}
	l.mu.Unlock()
}

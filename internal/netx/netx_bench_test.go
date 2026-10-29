package netx

import (
	"testing"
)

func BenchmarkDecodeConsole_UTF8(b *testing.B) {
	s := "hello world this is already utf8"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DecodeConsole(s)
	}
}

func BenchmarkDecodeConsole_GBK(b *testing.B) {
	// GBK 编码的中文：这是一条码流
	s := "\xd5\xe2\xca\xc7\xd2\xbb\xcc\xf5\xc2\xeb\xc1\xf7"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DecodeConsole(s)
	}
}

func BenchmarkDecodeConsole_Mixed(b *testing.B) {
	// 混合：GBK 中文 + ASCII 标点
	s := "\xc4\xad\xd3\xa2\xca\xd6\xc5\xc5\x3a\x20" +
		"\xb2\xbb\xc7\xf3\xcf\xf1\xbb\xee\xbd\xd3\xbf\xda\xb2\xbb\xc7\xf3\xcf\xf1"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DecodeConsole(s)
	}
}

func BenchmarkDecodeConsole_Empty(b *testing.B) {
	s := ""
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DecodeConsole(s)
	}
}

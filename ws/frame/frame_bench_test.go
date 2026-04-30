package frame

import (
	"bytes"
	"testing"

	"github.com/lufeijun/goTools/ws/buf"
)

func BenchmarkWriteFrame(b *testing.B) {
	f := NewTextFrame(make([]byte, 128))
	var w bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Reset()
		_ = WriteFrame(&w, f)
	}
}

func BenchmarkWriteFrameTo(b *testing.B) {
	f := NewTextFrame(make([]byte, 128))
	pool := buf.NewPool(512, 4096, 65536)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bb := pool.Get(256)
		_ = WriteFrameTo(bb, f)
		bb.Release()
	}
}

func BenchmarkReadFrame(b *testing.B) {
	f := NewTextFrame(make([]byte, 128))
	var w bytes.Buffer
	_ = WriteFrame(&w, f)
	data := w.Bytes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = ReadFrame(bytes.NewReader(data))
	}
}

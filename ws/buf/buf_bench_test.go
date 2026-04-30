package buf

import (
	"testing"
)

func BenchmarkByteBuf_Write(b *testing.B) {
	b.ReportAllocs()
	payload := make([]byte, 128)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bb := NewByteBuf(128)
		bb.Write(payload)
		bb.Release()
	}
}

func BenchmarkByteBuf_ReadBytes(b *testing.B) {
	b.ReportAllocs()
	payload := make([]byte, 128)
	bb := NewByteBuf(128)
	bb.Write(payload)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bb.ReadBytes(64)
		bb.SetReaderIndex(0)
	}
	bb.Release()
}

func BenchmarkByteBuf_PeekSkip(b *testing.B) {
	b.ReportAllocs()
	payload := make([]byte, 128)
	bb := NewByteBuf(128)
	bb.Write(payload)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bb.Peek(64)
		bb.Skip(64)
		bb.SetReaderIndex(0)
	}
	bb.Release()
}

func BenchmarkByteBuf_Slice(b *testing.B) {
	b.ReportAllocs()
	payload := make([]byte, 128)
	bb := NewByteBuf(128)
	bb.Write(payload)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := bb.Slice(0, 64)
		s.Release()
	}
	bb.Release()
}

func BenchmarkByteBuf_PoolGetPut(b *testing.B) {
	pool := NewPool(512, 4096, 65536)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bb := pool.Get(256)
		bb.Write(make([]byte, 200))
		bb.Release()
	}
}

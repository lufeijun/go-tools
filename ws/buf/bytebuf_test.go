package buf

import (
	"bytes"
	"testing"
)

func TestByteBuf_ReadWrite(t *testing.T) {
	b := NewByteBuf(64)
	defer b.Release()

	b.Write([]byte("hello"))
	if b.ReadableBytes() != 5 {
		t.Errorf("ReadableBytes = %d, want 5", b.ReadableBytes())
	}

	got := b.ReadBytes(3)
	if !bytes.Equal(got, []byte("hel")) {
		t.Errorf("ReadBytes = %q, want %q", got, "hel")
	}
	if b.ReadableBytes() != 2 {
		t.Errorf("ReadableBytes after read = %d, want 2", b.ReadableBytes())
	}
}

func TestByteBuf_Slice(t *testing.T) {
	b := NewByteBuf(64)
	defer b.Release()

	b.Write([]byte("hello world"))
	s := b.Slice(2, 5)
	defer s.Release()

	if !bytes.Equal(s.ReadAll(), []byte("llo w")) {
		t.Errorf("Slice = %q, want %q", s.ReadAll(), "llo w")
	}
	if s.RefCount() != 1 {
		t.Errorf("Slice RefCount = %d, want 1", s.RefCount())
	}
}

func TestByteBuf_ReferenceCount(t *testing.T) {
	b := NewByteBuf(64)
	if b.RefCount() != 1 {
		t.Errorf("initial RefCount = %d, want 1", b.RefCount())
	}

	b.Retain()
	if b.RefCount() != 2 {
		t.Errorf("after Retain RefCount = %d, want 2", b.RefCount())
	}

	b.Release()
	if b.RefCount() != 1 {
		t.Errorf("after Release RefCount = %d, want 1", b.RefCount())
	}

	b.Release()
	// after second Release, the buf should be returned to pool
}

func TestByteBuf_DoubleFreePanic(t *testing.T) {
	b := NewByteBuf(64)
	b.Release()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double Release")
		}
	}()
	b.Release()
}

func TestByteBuf_Peek(t *testing.T) {
	b := NewByteBuf(64)
	defer b.Release()

	b.Write([]byte("abc"))
	if !bytes.Equal(b.Peek(2), []byte("ab")) {
		t.Errorf("Peek = %q, want %q", b.Peek(2), "ab")
	}
	if b.ReadableBytes() != 3 {
		t.Errorf("ReadableBytes after Peek = %d, want 3", b.ReadableBytes())
	}
}

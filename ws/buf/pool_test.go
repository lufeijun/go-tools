package buf

import "testing"

func TestPool_GetPut_Small(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b := p.Get(100)
	if cap(b.Bytes()) != 512 {
		t.Errorf("cap = %d, want 512", cap(b.Bytes()))
	}
	b.Release()
}

func TestPool_GetPut_Default(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b := p.Get(2000)
	if cap(b.Bytes()) != 4096 {
		t.Errorf("cap = %d, want 4096", cap(b.Bytes()))
	}
	b.Release()
}

func TestPool_GetPut_Large(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b := p.Get(100000)
	if cap(b.Bytes()) < 100000 {
		t.Errorf("cap = %d, want >= 100000", cap(b.Bytes()))
	}
	b.Release()
}

func TestPool_Reuse(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b1 := p.Get(100)
	b1.Write([]byte("hello"))
	b1.Release()

	b2 := p.Get(100)
	if b2.ReadableBytes() != 0 {
		t.Errorf("reused buf should be empty, got %d readable", b2.ReadableBytes())
	}
	b2.Release()
}

package buf

import "sync"

// NewPool creates a tiered ByteBuf pool.
// smallSize, defaultSize, largeSize define the tier boundaries.
func NewPool(smallSize, defaultSize, largeSize int) Pool {
	return &bufPool{
		smallSize:   smallSize,
		defaultSize: defaultSize,
		largeSize:   largeSize,
	}
}

type bufPool struct {
	small sync.Pool // capacity <= smallSize
	def   sync.Pool // capacity <= defaultSize
	large sync.Pool // capacity <= largeSize

	smallSize   int
	defaultSize int
	largeSize   int
}

func (p *bufPool) Get(capacity int) ByteBuf {
	var b *byteBuf
	switch {
	case capacity <= p.smallSize:
		if v := p.small.Get(); v != nil {
			b = v.(*byteBuf)
		} else {
			b = &byteBuf{data: make([]byte, 0, p.smallSize)}
		}
	case capacity <= p.defaultSize:
		if v := p.def.Get(); v != nil {
			b = v.(*byteBuf)
		} else {
			b = &byteBuf{data: make([]byte, 0, p.defaultSize)}
		}
	case capacity <= p.largeSize:
		if v := p.large.Get(); v != nil {
			b = v.(*byteBuf)
		} else {
			b = &byteBuf{data: make([]byte, 0, p.largeSize)}
		}
	default:
		b = &byteBuf{data: make([]byte, 0, capacity)}
	}
	b.readerIndex = 0
	b.writerIndex = 0
	b.refCount = 1
	b.pool = p
	return b
}

func (p *bufPool) Put(b ByteBuf) {
	bb, ok := b.(*byteBuf)
	if !ok {
		return
	}
	c := cap(bb.data)
	switch {
	case c <= p.smallSize:
		p.small.Put(bb)
	case c <= p.defaultSize:
		p.def.Put(bb)
	case c <= p.largeSize:
		p.large.Put(bb)
	}
}

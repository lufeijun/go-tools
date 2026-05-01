package pipeline

import (
	"sync"
	"sync/atomic"
)

// NewPipeline creates a new ChannelPipeline.
func NewPipeline() ChannelPipeline {
	p := &defaultPipeline{
		ctxs: make(map[string]*handlerContext),
	}
	p.head = &handlerContext{pipeline: p, name: "head"}
	p.tail = &handlerContext{pipeline: p, name: "tail"}
	p.head.next = p.tail
	p.tail.prev = p.head
	return p
}

func (p *defaultPipeline) rebuild() {
	var lastInbound *handlerContext
	for ctx := p.head.next; ctx != p.tail; ctx = ctx.next {
		if _, ok := ctx.handler.(InboundHandler); ok {
			if lastInbound != nil {
				lastInbound.nextInbound.Store(ctx)
			} else {
				p.head.nextInbound.Store(ctx)
			}
			lastInbound = ctx
		}
	}
	if lastInbound != nil {
		lastInbound.nextInbound.Store(nil)
	} else {
		p.head.nextInbound.Store(nil)
	}

	var lastOutbound *handlerContext
	for ctx := p.head.next; ctx != p.tail; ctx = ctx.next {
		ctx.prevOutbound.Store(lastOutbound)
		if _, ok := ctx.handler.(OutboundHandler); ok {
			lastOutbound = ctx
		}
	}
	p.tail.prevOutbound.Store(lastOutbound)
}

type defaultPipeline struct {
	mu   sync.RWMutex // protects ctxs map and linked list during setup; event traversal is lock-free
	head *handlerContext
	tail *handlerContext
	ctxs map[string]*handlerContext
}

type handlerContext struct {
	pipeline     *defaultPipeline
	name         string
	handler      ChannelHandler
	prev         *handlerContext
	next         *handlerContext
	nextInbound  atomic.Pointer[handlerContext] // next InboundHandler in chain (head→tail)
	prevOutbound atomic.Pointer[handlerContext] // previous OutboundHandler in chain (tail→head)
}

func (c *handlerContext) Pipeline() ChannelPipeline       { return c.pipeline }
func (c *handlerContext) FireChannelRead(msg interface{}) { c.invokeChannelRead(msg) }
func (c *handlerContext) FireChannelWrite(msg interface{}) {
	c.invokeChannelWrite(msg)
}
func (c *handlerContext) FireChannelActive()            { c.invokeChannelActive() }
func (c *handlerContext) FireChannelInactive()          { c.invokeChannelInactive() }
func (c *handlerContext) FireExceptionCaught(err error) { c.invokeExceptionCaught(err) }
func (c *handlerContext) Write(msg interface{})         { c.invokeChannelWrite(msg) }
func (c *handlerContext) Flush()                        {}

func (c *handlerContext) invokeChannelRead(msg interface{}) {
	next := c.nextInbound.Load()
	if next != nil {
		next.handler.(InboundHandler).ChannelRead(next, msg)
	}
}

func (c *handlerContext) invokeChannelWrite(msg interface{}) {
	prev := c.prevOutbound.Load()
	if prev != nil {
		prev.handler.(OutboundHandler).Write(prev, msg)
	}
}

func (c *handlerContext) invokeChannelActive() {
	next := c.nextInbound.Load()
	if next != nil {
		next.handler.(InboundHandler).ChannelActive(next)
	}
}

func (c *handlerContext) invokeChannelInactive() {
	next := c.nextInbound.Load()
	if next != nil {
		next.handler.(InboundHandler).ChannelInactive(next)
	}
}

func (c *handlerContext) invokeExceptionCaught(err error) {
	next := c.nextInbound.Load()
	if next != nil {
		next.handler.(InboundHandler).ExceptionCaught(next, err)
	}
}

func (p *defaultPipeline) AddFirst(name string, handler ChannelHandler) ChannelPipeline {
	if handler == nil {
		panic("pipeline: handler is nil")
	}
	if name == "" {
		panic("pipeline: handler name is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.ctxs[name]; exists {
		panic("pipeline: duplicate handler name: " + name)
	}
	ctx := &handlerContext{pipeline: p, name: name, handler: handler}
	p.insertAfter(p.head, ctx)
	p.ctxs[name] = ctx
	p.rebuild()
	return p
}

func (p *defaultPipeline) AddLast(name string, handler ChannelHandler) ChannelPipeline {
	if handler == nil {
		panic("pipeline: handler is nil")
	}
	if name == "" {
		panic("pipeline: handler name is empty")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.ctxs[name]; exists {
		panic("pipeline: duplicate handler name: " + name)
	}
	ctx := &handlerContext{pipeline: p, name: name, handler: handler}
	p.insertBefore(p.tail, ctx)
	p.ctxs[name] = ctx
	p.rebuild()
	return p
}

func (p *defaultPipeline) Remove(name string) ChannelPipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, ok := p.ctxs[name]
	if !ok {
		return p
	}
	delete(p.ctxs, name)
	ctx.prev.next = ctx.next
	ctx.next.prev = ctx.prev
	p.rebuild()
	return p
}

func (p *defaultPipeline) insertAfter(after, ctx *handlerContext) {
	ctx.prev = after
	ctx.next = after.next
	after.next.prev = ctx
	after.next = ctx
}

func (p *defaultPipeline) insertBefore(before, ctx *handlerContext) {
	ctx.next = before
	ctx.prev = before.prev
	before.prev.next = ctx
	before.prev = ctx
}

func (p *defaultPipeline) FireChannelRead(msg interface{}) {
	p.head.invokeChannelRead(msg)
}

func (p *defaultPipeline) FireChannelWrite(msg interface{}) {
	p.tail.invokeChannelWrite(msg)
}

func (p *defaultPipeline) FireChannelActive() {
	p.head.invokeChannelActive()
}

func (p *defaultPipeline) FireChannelInactive() {
	p.head.invokeChannelInactive()
}

func (p *defaultPipeline) FireExceptionCaught(err error) {
	p.head.invokeExceptionCaught(err)
}

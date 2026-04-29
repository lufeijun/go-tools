package pipeline

import "sync"

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

type defaultPipeline struct {
	mu   sync.RWMutex
	head *handlerContext
	tail *handlerContext
	ctxs map[string]*handlerContext
}

type handlerContext struct {
	pipeline *defaultPipeline
	name     string
	handler  ChannelHandler
	prev     *handlerContext
	next     *handlerContext
}

func (c *handlerContext) Pipeline() ChannelPipeline          { return c.pipeline }
func (c *handlerContext) FireChannelRead(msg interface{})    { c.invokeChannelRead(msg) }
func (c *handlerContext) FireChannelWrite(msg interface{})   { c.invokeChannelWrite(msg) }
func (c *handlerContext) FireChannelActive()                 { c.invokeChannelActive() }
func (c *handlerContext) FireChannelInactive()               { c.invokeChannelInactive() }
func (c *handlerContext) Write(msg interface{})              { c.invokeChannelWrite(msg) }
func (c *handlerContext) Flush()                             {}

func (c *handlerContext) invokeChannelRead(msg interface{}) {
	c.pipeline.mu.RLock()
	defer c.pipeline.mu.RUnlock()
	next := c.findNextInbound()
	if next != nil {
		next.handler.(InboundHandler).ChannelRead(next, msg)
	}
}

func (c *handlerContext) invokeChannelWrite(msg interface{}) {
	c.pipeline.mu.RLock()
	defer c.pipeline.mu.RUnlock()
	prev := c.findPrevOutbound()
	if prev != nil {
		prev.handler.(OutboundHandler).Write(prev, msg)
	}
}

func (c *handlerContext) invokeChannelActive() {
	c.pipeline.mu.RLock()
	defer c.pipeline.mu.RUnlock()
	next := c.findNextInbound()
	if next != nil {
		next.handler.(InboundHandler).ChannelActive(next)
	}
}

func (c *handlerContext) invokeChannelInactive() {
	c.pipeline.mu.RLock()
	defer c.pipeline.mu.RUnlock()
	next := c.findNextInbound()
	if next != nil {
		next.handler.(InboundHandler).ChannelInactive(next)
	}
}

func (c *handlerContext) findNextInbound() *handlerContext {
	for ctx := c.next; ctx != nil; ctx = ctx.next {
		if _, ok := ctx.handler.(InboundHandler); ok {
			return ctx
		}
	}
	return nil
}

func (c *handlerContext) findPrevOutbound() *handlerContext {
	for ctx := c.prev; ctx != nil; ctx = ctx.prev {
		if _, ok := ctx.handler.(OutboundHandler); ok {
			return ctx
		}
	}
	return nil
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

func (p *defaultPipeline) FireExceptionCaught(err error) { /* TODO */ }

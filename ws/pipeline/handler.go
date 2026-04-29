package pipeline

// ChannelPipeline is the handler chain for a connection.
type ChannelPipeline interface {
	AddFirst(name string, handler ChannelHandler) ChannelPipeline
	AddLast(name string, handler ChannelHandler) ChannelPipeline
	Remove(name string) ChannelPipeline
	FireChannelRead(msg interface{})
	FireChannelWrite(msg interface{})
	FireChannelActive()
	FireChannelInactive()
	FireExceptionCaught(err error)
}

// ChannelHandler is the base handler interface.
type ChannelHandler interface {
	Name() string
}

// InboundHandler handles inbound data (reads).
type InboundHandler interface {
	ChannelHandler
	ChannelRead(ctx Context, msg interface{})
	ChannelActive(ctx Context)
	ChannelInactive(ctx Context)
	ExceptionCaught(ctx Context, err error)
}

// OutboundHandler handles outbound data (writes).
type OutboundHandler interface {
	ChannelHandler
	Write(ctx Context, msg interface{})
	Flush(ctx Context)
}

// Context is the handler's execution context within a pipeline.
type Context interface {
	Pipeline() ChannelPipeline
	FireChannelRead(msg interface{})
	FireChannelWrite(msg interface{})
	FireChannelActive()
	FireChannelInactive()
	Write(msg interface{})
	Flush()
}

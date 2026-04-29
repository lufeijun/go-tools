package conn

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

// epollConn is an event-driven Conn backed by a raw fd.
type epollConn struct {
	id        uint64
	fd        int
	isClient  bool
	pipeline  pipeline.ChannelPipeline
	active    int32
	closeOnce sync.Once
	el        interface{} // actual type: eventloop.EventLoop
}

func newEpollConn(fd int, isClient bool, id uint64) *epollConn {
	return &epollConn{
		id:       id,
		fd:       fd,
		isClient: isClient,
		pipeline: pipeline.NewPipeline(),
		active:   1,
	}
}

func (c *epollConn) ID() uint64                         { return c.id }
func (c *epollConn) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *epollConn) FD() int                            { return c.fd }
func (c *epollConn) IsClient() bool                     { return c.isClient }
func (c *epollConn) Active() bool                       { return atomic.LoadInt32(&c.active) == 1 }
func (c *epollConn) RemoteAddr() net.Addr               { return nil } // TODO: resolve from fd
func (c *epollConn) LocalAddr() net.Addr                { return nil } // TODO: resolve from fd

func (c *epollConn) Read(b buf.ByteBuf) error {
	if !c.Active() {
		return ErrConnClosed
	}
	// TODO: non-blocking read using syscall.Read (V2.1)
	return nil
}

func (c *epollConn) Write(b buf.ByteBuf) error {
	if !c.Active() {
		return ErrConnClosed
	}
	// TODO: non-blocking write using syscall.Write (V2.1)
	return nil
}

func (c *epollConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.active, 0)
		// TODO: deregister from eventloop, close fd
	})
	return err
}

func (c *epollConn) OnEvent(events uint32) {
	// TODO: handle read/write events (V2.1)
}

func (c *epollConn) SetEventLoop(el interface{}) {
	c.el = el
}

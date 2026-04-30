package conn

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/eventloop"
	"github.com/lufeijun/goTools/ws/pipeline"
	"golang.org/x/sys/unix"
)

// epollConn is an event-driven Conn backed by a raw fd.
// It performs non-blocking I/O and is intended to be used with an EventLoop.
type epollConn struct {
	id        uint64
	fd        int
	isClient  bool
	pipeline  pipeline.ChannelPipeline
	active    int32
	closeOnce sync.Once
	el        eventloop.EventLoop

	readBuf  *bytes.Buffer
	readMu   sync.Mutex
	maxFrame int

	writeMu  sync.Mutex
	writeBuf []buf.ByteBuf
	writeOff int
}

func newEpollConn(fd int, isClient bool, id uint64) *epollConn {
	unix.SetNonblock(fd, true)
	return &epollConn{
		id:       id,
		fd:       fd,
		isClient: isClient,
		pipeline: pipeline.NewPipeline(),
		active:   1,
		readBuf:  bytes.NewBuffer(nil),
		maxFrame: 64 * 1024 * 1024,
	}
}

func (c *epollConn) ID() uint64                         { return c.id }
func (c *epollConn) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *epollConn) FD() int                            { return c.fd }
func (c *epollConn) IsClient() bool                     { return c.isClient }
func (c *epollConn) Active() bool                       { return atomic.LoadInt32(&c.active) == 1 }
func (c *epollConn) RemoteAddr() net.Addr               { return nil } // TODO: resolve from fd
func (c *epollConn) LocalAddr() net.Addr                { return nil } // TODO: resolve from fd

func (c *epollConn) SetEventLoop(el interface{}) {
	if v, ok := el.(eventloop.EventLoop); ok {
		c.el = v
	}
}

// Read copies available data from the internal read buffer into b.
// In an event-driven model the data is already buffered by OnEvent,
// so this does not block on the fd.
func (c *epollConn) Read(b buf.ByteBuf) error {
	if !c.Active() {
		return ErrConnClosed
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if c.readBuf.Len() > 0 {
		b.Write(c.readBuf.Bytes())
		c.readBuf.Reset()
	}
	return nil
}

// Write queues data for sending. If the fd is currently writable,
// data is flushed immediately; otherwise it is buffered and EPOLLOUT
// is registered so the remaining bytes are sent on the next write event.
func (c *epollConn) Write(b buf.ByteBuf) error {
	if !c.Active() {
		return ErrConnClosed
	}
	c.writeMu.Lock()
	c.writeBuf = append(c.writeBuf, b)
	c.writeMu.Unlock()
	c.flushWrite()
	return nil
}

// flushWrite attempts to write buffered data in a non-blocking loop.
// It stops when everything is sent or when EAGAIN is encountered.
func (c *epollConn) flushWrite() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	for len(c.writeBuf) > 0 {
		bb := c.writeBuf[0]
		data := bb.Bytes()
		if c.writeOff < len(data) {
			n, err := unix.Write(c.fd, data[c.writeOff:])
			if n > 0 {
				c.writeOff += n
			}
			if err != nil {
				if err == unix.EAGAIN {
					c.registerWriteEvent()
					return
				}
				c.closeLocked()
				return
			}
		}
		if c.writeOff >= len(data) {
			bb.Release()
			c.writeBuf = c.writeBuf[1:]
			c.writeOff = 0
		}
	}

	// All data flushed – remove EPOLLOUT if registered.
	if c.el != nil {
		_ = c.el.Mod(c.fd, eventloop.EventRead)
	}
}

func (c *epollConn) registerWriteEvent() {
	if c.el != nil {
		_ = c.el.Mod(c.fd, eventloop.EventRead|eventloop.EventWrite)
	}
}

// Close deregisters the fd from the event loop and closes it.
func (c *epollConn) Close() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.closeLocked()
	return nil
}

func (c *epollConn) closeLocked() {
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.active, 0)
		if c.el != nil {
			_ = c.el.Deregister(c.fd)
		}
		for _, bb := range c.writeBuf {
			bb.Release()
		}
		c.writeBuf = nil
		unix.Close(c.fd)
	})
}

// OnEvent is called by the EventLoop when the fd becomes ready.
func (c *epollConn) OnEvent(events uint32) {
	if events&eventloop.EventRead != 0 {
		c.handleReadEvent()
	}
	if events&eventloop.EventWrite != 0 {
		c.handleWriteEvent()
	}
	if events&(eventloop.EventError|eventloop.EventHup) != 0 {
		c.Close()
	}
}

func (c *epollConn) handleReadEvent() {
	tmp := make([]byte, 4096)
	for {
		n, err := unix.Read(c.fd, tmp)
		if n > 0 {
			c.readMu.Lock()
			c.readBuf.Write(tmp[:n])
			c.readMu.Unlock()
		}
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return
			}
			c.Close()
			return
		}
		if n == 0 {
			c.Close()
			return
		}
	}
}

func (c *epollConn) handleWriteEvent() {
	c.flushWrite()
}

// eventHandlerAdapter adapts epollConn to eventloop.EventHandler.
type eventHandlerAdapter struct {
	c *epollConn
}

func (a *eventHandlerAdapter) OnEvent(fd int, events uint32) {
	a.c.OnEvent(events)
}

package conn

import (
	"bytes"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/eventloop"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/pipeline"
	"golang.org/x/sys/unix"
)

// EpollConn is an event-driven Conn backed by a raw fd.
// It performs non-blocking I/O and is intended to be used with an EventLoop.
type EpollConn struct {
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

	frameParser *frame.IncrementalParser
	onFrame     func(frame.Frame)
	onClose     func()
}

func NewEpollConn(fd int, isClient bool, id uint64) *EpollConn {
	unix.SetNonblock(fd, true)
	return &EpollConn{
		id:          id,
		fd:          fd,
		isClient:    isClient,
		pipeline:    pipeline.NewPipeline(),
		active:      1,
		readBuf:     bytes.NewBuffer(nil),
		maxFrame:    64 * 1024 * 1024,
		frameParser: frame.NewIncrementalParser(64 * 1024 * 1024),
	}
}

func (c *EpollConn) ID() uint64                         { return c.id }
func (c *EpollConn) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *EpollConn) FD() int                            { return c.fd }
func (c *EpollConn) IsClient() bool                     { return c.isClient }
func (c *EpollConn) Active() bool                       { return atomic.LoadInt32(&c.active) == 1 }
func (c *EpollConn) RemoteAddr() net.Addr               { return nil } // TODO: resolve from fd
func (c *EpollConn) LocalAddr() net.Addr                { return nil } // TODO: resolve from fd

// SetOnFrame sets the callback function that will be invoked when a complete frame is received.
func (c *EpollConn) SetOnFrame(fn func(frame.Frame)) {
	c.onFrame = fn
}

// SetOnClose sets the callback function that will be invoked when the connection is closed.
func (c *EpollConn) SetOnClose(fn func()) {
	c.onClose = fn
}

// SetMaxFrameSize sets the maximum allowed frame size and recreates the frame parser.
func (c *EpollConn) SetMaxFrameSize(n int) {
	c.maxFrame = n
	c.frameParser = frame.NewIncrementalParser(n)
}

func (c *EpollConn) SetEventLoop(el interface{}) {
	if v, ok := el.(eventloop.EventLoop); ok {
		c.el = v
	}
}

// Read copies available data from the internal read buffer into b.
// In an event-driven model the data is already buffered by OnEvent,
// so this does not block on the fd.
func (c *EpollConn) Read(b buf.ByteBuf) error {
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
func (c *EpollConn) Write(b buf.ByteBuf) error {
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
func (c *EpollConn) flushWrite() {
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

func (c *EpollConn) registerWriteEvent() {
	if c.el != nil {
		_ = c.el.Mod(c.fd, eventloop.EventRead|eventloop.EventWrite)
	}
}

// Close deregisters the fd from the event loop and closes it.
func (c *EpollConn) Close() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.closeLocked()
	return nil
}

func (c *EpollConn) closeLocked() {
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
		if c.onClose != nil {
			c.onClose()
		}
	})
}

// OnEvent is called by the EventLoop when the fd becomes ready.
func (c *EpollConn) OnEvent(events uint32) {
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

func (c *EpollConn) handleReadEvent() {
	tmp := make([]byte, 4096)
	for {
		n, err := unix.Read(c.fd, tmp)
		if n > 0 {
			if c.onFrame != nil && c.frameParser != nil {
				// Use frame parser if onFrame callback is set
				frames := c.frameParser.Feed(tmp[:n])
				for _, f := range frames {
					c.onFrame(f)
				}
				if c.frameParser.Err() != nil {
					// Parse error, close connection
					c.Close()
					return
				}
			} else {
				// Fallback to read buffer behavior
				c.readMu.Lock()
				c.readBuf.Write(tmp[:n])
				c.readMu.Unlock()
			}
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

func (c *EpollConn) handleWriteEvent() {
	c.flushWrite()
}

// EventHandlerAdapter adapts EpollConn to eventloop.EventHandler.
type EventHandlerAdapter struct {
	Conn *EpollConn
}

func (a *EventHandlerAdapter) OnEvent(fd int, events uint32) {
	a.Conn.OnEvent(events)
}

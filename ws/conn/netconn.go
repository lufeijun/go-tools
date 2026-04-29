package conn

import (
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type netConn struct {
	id        uint64
	conn      net.Conn
	isClient  bool
	pipeline  pipeline.ChannelPipeline
	active    int32
	closeOnce sync.Once
}

func NewNetConn(nc net.Conn, isClient bool, id uint64) *netConn {
	c := &netConn{
		id:       id,
		conn:     nc,
		isClient: isClient,
		pipeline: pipeline.NewPipeline(),
		active:   1,
	}
	return c
}

func (c *netConn) ID() uint64                    { return c.id }
func (c *netConn) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *netConn) RemoteAddr() net.Addr          { return c.conn.RemoteAddr() }
func (c *netConn) LocalAddr() net.Addr           { return c.conn.LocalAddr() }
func (c *netConn) IsClient() bool                { return c.isClient }
func (c *netConn) Active() bool                  { return atomic.LoadInt32(&c.active) == 1 }

func (c *netConn) Read(b buf.ByteBuf) error {
	if !c.Active() {
		return io.EOF
	}
	// Read into a temp buffer then write to ByteBuf
	tmp := make([]byte, 4096)
	n, err := c.conn.Read(tmp)
	if n > 0 {
		b.Write(tmp[:n])
	}
	return err
}

func (c *netConn) Write(b buf.ByteBuf) error {
	if !c.Active() {
		return io.ErrClosedPipe
	}
	_, err := c.conn.Write(b.ReadAll())
	return err
}

func (c *netConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.active, 0)
		err = c.conn.Close()
	})
	return err
}

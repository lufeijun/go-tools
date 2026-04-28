package conn

import (
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lufeijun/goTools/ws/frame"
)

type Message struct {
	Type   frame.Opcode
	Data   []byte
	Status uint16
}

type Conn interface {
	ReadChan() <-chan Message
	WriteChan() chan<- Message
	Close() error
	RemoteAddr() net.Addr
	LocalAddr() net.Addr
	ID() uint64
}

var connIDSeq uint64

func nextConnID() uint64 {
	return atomic.AddUint64(&connIDSeq, 1)
}

const defaultChanSize = 256

type goroutineConn struct {
	id        uint64
	conn      net.Conn
	isClient  bool
	readChan  chan Message
	writeChan chan Message
	closeChan chan struct{}
	closeOnce sync.Once
}

func newGoroutineConn(nc net.Conn, isClient bool) *goroutineConn {
	if tc, ok := nc.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
		tc.SetReadDeadline(time.Time{})
	}
	c := &goroutineConn{
		id:        nextConnID(),
		conn:      nc,
		isClient:  isClient,
		readChan:  make(chan Message, defaultChanSize),
		writeChan: make(chan Message, defaultChanSize),
		closeChan: make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

func (c *goroutineConn) ReadChan() <-chan Message  { return c.readChan }
func (c *goroutineConn) WriteChan() chan<- Message  { return c.writeChan }
func (c *goroutineConn) ID() uint64                 { return c.id }
func (c *goroutineConn) RemoteAddr() net.Addr       { return c.conn.RemoteAddr() }
func (c *goroutineConn) LocalAddr() net.Addr        { return c.conn.LocalAddr() }

func (c *goroutineConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		select {
		case c.writeChan <- Message{Type: frame.OpcodeClose, Status: 1000}:
		default:
		}
		close(c.closeChan)
		err = c.conn.Close()
	})
	return err
}

func (c *goroutineConn) readLoop() {
	defer close(c.readChan)
	for {
		select {
		case <-c.closeChan:
			return
		default:
		}

		f, err := frame.ReadFrame(c.conn)
		if err != nil {
			return
		}

		msg := Message{Type: f.Opcode, Data: f.Payload}
		if f.Opcode == frame.OpcodeClose && len(f.Payload) >= 2 {
			msg.Status = uint16(f.Payload[0])<<8 | uint16(f.Payload[1])
		}

		switch f.Opcode {
		case frame.OpcodePing:
			select {
			case c.writeChan <- Message{Type: frame.OpcodePong, Data: f.Payload}:
			default:
			}
			select {
			case c.readChan <- msg:
			case <-c.closeChan:
				return
			}

		case frame.OpcodeClose:
			select {
			case c.writeChan <- Message{Type: frame.OpcodeClose, Status: msg.Status}:
			default:
			}
			select {
			case c.readChan <- msg:
			case <-c.closeChan:
			}
			return

		default:
			select {
			case c.readChan <- msg:
			case <-c.closeChan:
				return
			}
		}
	}
}

func (c *goroutineConn) writeLoop() {
	for {
		select {
		case msg, ok := <-c.writeChan:
			if !ok {
				return
			}

			var f frame.Frame
			switch msg.Type {
			case frame.OpcodeText:
				f = frame.NewTextFrame(msg.Data)
			case frame.OpcodeBinary:
				f = frame.NewBinaryFrame(msg.Data)
			case frame.OpcodePing:
				f = frame.NewPingFrame(msg.Data)
			case frame.OpcodePong:
				f = frame.NewPongFrame(msg.Data)
			case frame.OpcodeClose:
				f = frame.NewCloseFrame(msg.Status, string(msg.Data))
			default:
				continue
			}

			f.Masked = c.isClient
			if c.isClient {
				f.MaskKey = frame.GenerateMaskKey()
			}

			if err := frame.WriteFrame(c.conn, f); err != nil {
				return
			}

		case <-c.closeChan:
			return
		}
	}
}

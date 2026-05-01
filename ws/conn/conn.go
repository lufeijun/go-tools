package conn

import (
	"errors"
	"net"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/pipeline"
)

var ErrConnClosed = errors.New("conn: connection closed")

// Message is the high-level message structure.
type Message struct {
	Type   byte   // Opcode
	Data   []byte
	Status uint16 // For Close frames
}

// Conn is the WebSocket connection abstraction.
type Conn interface {
	ID() uint64
	Pipeline() pipeline.ChannelPipeline
	Read(b buf.ByteBuf) error
	Write(b buf.ByteBuf) error
	RemoteAddr() net.Addr
	LocalAddr() net.Addr
	IsClient() bool
	Close() error
	Active() bool
}

// EventDrivenConn is the event-driven Conn extension.
type EventDrivenConn interface {
	Conn
	FD() int
	OnEvent(events uint32)
	SetEventLoop(el interface{})
	SetOnFrame(fn func(frame.Frame))
}

var connIDSeq uint64

// NextConnID returns the next unique connection ID.
func NextConnID() uint64 {
	return atomic.AddUint64(&connIDSeq, 1)
}

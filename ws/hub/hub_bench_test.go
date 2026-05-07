package hub

import (
	"net"
	"testing"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
	"github.com/lufeijun/goTools/ws/session"
)

type benchSession struct {
	id   uint64
	pip  pipeline.ChannelPipeline
	msgs int
}

func newBenchSession(id uint64) *benchSession {
	return &benchSession{id: id, pip: pipeline.NewPipeline()}
}

func (s *benchSession) Conn() conn.Conn                 { return &benchConn{id: s.id, pip: s.pip} }
func (s *benchSession) State() session.State            { return session.StateConnected }
func (s *benchSession) StateChan() <-chan session.State { return nil }
func (s *benchSession) SetState(st session.State)          {}
func (s *benchSession) SetConn(conn.Conn)                  {}
func (s *benchSession) SetHeartbeater(session.Heartbeater) {}
func (s *benchSession) Close() error                       { return nil }

type benchConn struct {
	id  uint64
	pip pipeline.ChannelPipeline
}

func (c *benchConn) ID() uint64                         { return c.id }
func (c *benchConn) Pipeline() pipeline.ChannelPipeline { return c.pip }
func (c *benchConn) Read(b buf.ByteBuf) error           { return nil }
func (c *benchConn) Write(b buf.ByteBuf) error          { return nil }
func (c *benchConn) RemoteAddr() net.Addr               { return nil }
func (c *benchConn) LocalAddr() net.Addr                { return nil }
func (c *benchConn) IsClient() bool                     { return false }
func (c *benchConn) Close() error                       { return nil }
func (c *benchConn) Active() bool                       { return true }

func BenchmarkHub_Broadcast_1K(b *testing.B) {
	h := NewHub(32)
	for i := 0; i < 1000; i++ {
		h.Register(newBenchSession(uint64(i)))
	}
	msg := conn.Message{Type: 0x1, Data: []byte("hello")}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Broadcast(msg)
	}
}

func BenchmarkHub_Broadcast_10K(b *testing.B) {
	h := NewHub(32)
	for i := 0; i < 10000; i++ {
		h.Register(newBenchSession(uint64(i)))
	}
	msg := conn.Message{Type: 0x1, Data: []byte("hello")}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Broadcast(msg)
	}
}

package hub

import (
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
	"github.com/lufeijun/goTools/ws/session"
)

type mockHubConn struct {
	id  uint64
	pip pipeline.ChannelPipeline
}

func newMockHubConn(id uint64) *mockHubConn {
	return &mockHubConn{id: id, pip: pipeline.NewPipeline()}
}

func (m *mockHubConn) ID() uint64                         { return m.id }
func (m *mockHubConn) Pipeline() pipeline.ChannelPipeline { return m.pip }
func (m *mockHubConn) Read(b buf.ByteBuf) error           { return nil }
func (m *mockHubConn) Write(b buf.ByteBuf) error          { return nil }
func (m *mockHubConn) RemoteAddr() net.Addr               { return nil }
func (m *mockHubConn) LocalAddr() net.Addr                { return nil }
func (m *mockHubConn) IsClient() bool                     { return false }
func (m *mockHubConn) Close() error                       { return nil }
func (m *mockHubConn) Active() bool                       { return true }

func newTestSession(id uint64) session.Session {
	return session.NewSession(newMockHubConn(id), session.Config{})
}

func TestShardedHub_RegisterCount(t *testing.T) {
	h := NewHub(4)
	s1 := newTestSession(1)
	s2 := newTestSession(2)

	h.Register(s1)
	h.Register(s2)

	if h.Count() != 2 {
		t.Errorf("Count = %d, want 2", h.Count())
	}
}

func TestShardedHub_Unregister(t *testing.T) {
	h := NewHub(4)
	s := newTestSession(1)
	h.Register(s)
	h.Unregister(1)

	if h.Count() != 0 {
		t.Errorf("Count = %d, want 0", h.Count())
	}
}

func TestShardedHub_Get(t *testing.T) {
	h := NewHub(4)
	s := newTestSession(42)
	h.Register(s)

	got := h.Get(42)
	if got == nil {
		t.Fatal("Get(42) = nil")
	}
	if got.Conn().ID() != 42 {
		t.Errorf("ID = %d, want 42", got.Conn().ID())
	}

	if h.Get(999) != nil {
		t.Error("Get(999) should be nil")
	}
}

func TestShardedHub_Broadcast(t *testing.T) {
	h := NewHub(4)
	s1 := newTestSession(1)
	s2 := newTestSession(2)
	h.Register(s1)
	h.Register(s2)

	msg := conn.Message{Type: 0x1, Data: []byte("broadcast")}
	h.Broadcast(msg)
	// Non-blocking broadcast, just verify no panic
	time.Sleep(10 * time.Millisecond)
}

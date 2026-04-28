package ws

import (
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

// newTestSession creates a Session for testing using internal package access.
// This is only used within the ws package tests; external users use *ws.Session.

type mockHubConn struct {
	readChan  chan conn.Message
	writeChan chan conn.Message
	id        uint64
}

func newMockHubConn(id uint64) *mockHubConn {
	return &mockHubConn{
		readChan:  make(chan conn.Message, 64),
		writeChan: make(chan conn.Message, 64),
		id:        id,
	}
}

func (m *mockHubConn) ReadChan() <-chan conn.Message  { return m.readChan }
func (m *mockHubConn) WriteChan() chan<- conn.Message  { return m.writeChan }
func (m *mockHubConn) Close() error                    { return nil }
func (m *mockHubConn) RemoteAddr() net.Addr            { return nil }
func (m *mockHubConn) LocalAddr() net.Addr             { return nil }
func (m *mockHubConn) ID() uint64                      { return m.id }

func newTestSession(id uint64) *session.Session {
	mc := newMockHubConn(id)
	return session.NewSession(mc, session.SessionConfig{})
}

func TestHub_RegisterAndCount(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s := newTestSession(1)
	h.Register(s)
	time.Sleep(50 * time.Millisecond)

	if count := h.Count(); count != 1 {
		t.Errorf("Count = %d, want 1", count)
	}
}

func TestHub_Unregister(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s := newTestSession(1)
	h.Register(s)
	time.Sleep(50 * time.Millisecond)

	h.Unregister(1)
	time.Sleep(50 * time.Millisecond)

	if count := h.Count(); count != 0 {
		t.Errorf("Count = %d, want 0", count)
	}
}

func TestHub_Get(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s := newTestSession(1)
	h.Register(s)
	time.Sleep(50 * time.Millisecond)

	got := h.Get(1)
	if got == nil {
		t.Error("Get(1) = nil, want session")
	}

	got = h.Get(999)
	if got != nil {
		t.Error("Get(999) should be nil")
	}
}

func TestHub_Broadcast(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s1 := newTestSession(1)
	s2 := newTestSession(2)
	h.Register(s1)
	h.Register(s2)
	time.Sleep(50 * time.Millisecond)

	msg := conn.Message{Type: 0x1, Data: []byte("broadcast")}
	h.Broadcast(msg)
	time.Sleep(50 * time.Millisecond)
}

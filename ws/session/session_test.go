package session

import (
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type mockConn struct {
	id      uint64
	pip     pipeline.ChannelPipeline
	readBuf []byte
}

func newMockConn(id uint64) *mockConn {
	return &mockConn{
		id:  id,
		pip: pipeline.NewPipeline(),
	}
}

func (m *mockConn) ID() uint64                         { return m.id }
func (m *mockConn) Pipeline() pipeline.ChannelPipeline { return m.pip }
func (m *mockConn) Read(b buf.ByteBuf) error           { return nil }
func (m *mockConn) Write(b buf.ByteBuf) error          { return nil }
func (m *mockConn) RemoteAddr() net.Addr               { return nil }
func (m *mockConn) LocalAddr() net.Addr                { return nil }
func (m *mockConn) IsClient() bool                     { return false }
func (m *mockConn) Close() error                       { return nil }
func (m *mockConn) Active() bool                       { return true }

func TestSession_StateTransitions(t *testing.T) {
	mc := newMockConn(1)
	s := NewSession(mc, Config{
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	if s.State() != StateDisconnected {
		t.Errorf("initial state = %d, want Disconnected", s.State())
	}

	s.SetState(StateConnecting)
	if s.State() != StateConnecting {
		t.Errorf("state = %d, want Connecting", s.State())
	}

	<-s.StateChan() // drain StateConnecting event

	s.SetState(StateConnected)
	select {
	case st := <-s.StateChan():
		if st != StateConnected {
			t.Errorf("StateChan = %d, want Connected", st)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for state")
	}
}

func TestSession_Close(t *testing.T) {
	mc := newMockConn(1)
	s := NewSession(mc, Config{})
	s.SetState(StateConnected)
	s.Close()
	if s.State() != StateClosed {
		t.Errorf("state = %d, want Closed", s.State())
	}
}

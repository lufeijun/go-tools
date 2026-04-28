package session

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/internal/conn"
)

type mockConn struct {
	readChan  chan conn.Message
	writeChan chan conn.Message
}

func newMockConn() *mockConn {
	return &mockConn{
		readChan:  make(chan conn.Message, 64),
		writeChan: make(chan conn.Message, 64),
	}
}

func (m *mockConn) ReadChan() <-chan conn.Message { return m.readChan }
func (m *mockConn) WriteChan() chan<- conn.Message { return m.writeChan }
func (m *mockConn) Close() error                   { return nil }
func (m *mockConn) RemoteAddr() net.Addr           { return nil }
func (m *mockConn) LocalAddr() net.Addr            { return nil }
func (m *mockConn) ID() uint64                     { return 1 }

func TestSession_StateTransitions(t *testing.T) {
	mc := newMockConn()
	s := NewSession(mc, SessionConfig{
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

	s.SetState(StateConnected)
	if s.State() != StateConnected {
		t.Errorf("state = %d, want Connected", s.State())
	}

	// Drain all prior state changes from the buffered channel.
	for len(s.stateChan) > 0 {
		<-s.stateChan
	}

	// Now set a new state and verify it arrives.
	s.SetState(StateReconnecting)
	select {
	case st := <-s.StateChan():
		if st != StateReconnecting {
			t.Errorf("StateChan = %d, want Reconnecting", st)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for state change")
	}
}

func TestHeartbeat_PingSent(t *testing.T) {
	mc := newMockConn()
	hb := NewPerConnHeartbeater(100*time.Millisecond, 5*time.Second)
	s := NewSession(mc, SessionConfig{
		PingInterval: 100 * time.Millisecond,
		PongTimeout:  5 * time.Second,
	})
	s.SetHeartbeater(hb)
	s.SetState(StateConnected)
	hb.Start(mc)

	select {
	case msg := <-mc.writeChan:
		if msg.Type != frame.OpcodePing {
			t.Errorf("got type %d, want Ping", msg.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ping")
	}

	hb.Stop()
}

func TestReconnect_MaxRetries(t *testing.T) {
	mc := newMockConn()
	cfg := SessionConfig{
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		ReconnectInterval: 100 * time.Millisecond,
		MaxReconnect:      3,
	}
	s := NewSession(mc, cfg)
	s.SetState(StateConnected)
	s.SetState(StateDisconnected)

	// Drain stale state changes from earlier SetState calls.
	for len(s.stateChan) > 0 {
		<-s.stateChan
	}

	rc := NewReconnector(cfg.ReconnectInterval, cfg.MaxReconnect, func() (conn.Conn, error) {
		return nil, errors.New("connection refused")
	})

	go rc.Start(s)

	// Wait until we see StateClosed (reconnect exhausts all retries).
	for {
		select {
		case st := <-s.StateChan():
			if st == StateClosed {
				return // success
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for reconnect exhaustion")
		}
	}
}

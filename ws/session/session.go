package session

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/lufeijun/goTools/ws/conn"
)

// State represents the connection lifecycle state.
type State int

const (
	StateDisconnected State = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Config is the session configuration.
type Config struct {
	PingInterval      time.Duration
	PongTimeout       time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      int
}

// Session is the WebSocket session abstraction.
type Session interface {
	Conn() conn.Conn
	State() State
	StateChan() <-chan State
	SetState(State)
	SetConn(conn.Conn)
	SetHeartbeater(Heartbeater)
	Close() error
}

// defaultSession is the standard Session implementation.
type defaultSession struct {
	conn   conn.Conn
	config Config
	state  State
	closed int32
	hb     Heartbeater

	mu   sync.RWMutex
	subs map[chan State]struct{}
}

// NewSession creates a new Session.
func NewSession(c conn.Conn, cfg Config) Session {
	return &defaultSession{
		conn: c,
		config: cfg,
		subs: make(map[chan State]struct{}),
	}
}

func (s *defaultSession) Conn() conn.Conn { return s.conn }

func (s *defaultSession) State() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

func (s *defaultSession) SetConn(c conn.Conn)  { s.conn = c }
func (s *defaultSession) SetHeartbeater(hb Heartbeater) { s.hb = hb }

// StateChan returns a new channel that receives state changes.
// Each call creates a fresh subscriber; messages are broadcast to all subscribers.
func (s *defaultSession) StateChan() <-chan State {
	ch := make(chan State, 16)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	// Deliver current state to the new subscriber so it doesn't miss the latest.
	if s.state != StateDisconnected {
		select {
		case ch <- s.state:
		default:
		}
	}
	s.mu.Unlock()
	return ch
}

func (s *defaultSession) SetState(st State) {
	if atomic.LoadInt32(&s.closed) == 1 {
		return
	}
	s.mu.Lock()
	s.state = st
	for ch := range s.subs {
		select {
		case ch <- st:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *defaultSession) Close() error {
	if atomic.CompareAndSwapInt32(&s.closed, 0, 1) {
		if s.hb != nil {
			s.hb.Stop()
		}
		s.mu.Lock()
		s.state = StateClosed
		for ch := range s.subs {
			select {
			case ch <- StateClosed:
			default:
			}
			close(ch)
		}
		s.subs = make(map[chan State]struct{})
		s.mu.Unlock()
	}
	return s.conn.Close()
}

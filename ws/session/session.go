package session

import (
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
	Close() error
}

// defaultSession is the standard Session implementation.
type defaultSession struct {
	conn      conn.Conn
	config    Config
	stateChan chan State
	state     State
	closed    int32
}

// NewSession creates a new Session.
func NewSession(c conn.Conn, cfg Config) Session {
	return &defaultSession{
		conn:      c,
		config:    cfg,
		stateChan: make(chan State, 16),
		state:     StateDisconnected,
	}
}

func (s *defaultSession) Conn() conn.Conn         { return s.conn }
func (s *defaultSession) State() State            { return s.state }
func (s *defaultSession) StateChan() <-chan State { return s.stateChan }

func (s *defaultSession) SetState(st State) {
	if atomic.LoadInt32(&s.closed) == 1 {
		return
	}
	s.state = st
	select {
	case s.stateChan <- st:
	default:
	}
}

func (s *defaultSession) Close() error {
	if atomic.CompareAndSwapInt32(&s.closed, 0, 1) {
		s.state = StateClosed
		select {
		case s.stateChan <- StateClosed:
		default:
		}
		close(s.stateChan)
	}
	return s.conn.Close()
}

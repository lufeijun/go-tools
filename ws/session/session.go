package session

import (
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
	s.state = st
	select {
	case s.stateChan <- st:
	default:
	}
}

func (s *defaultSession) Close() error {
	s.SetState(StateClosed)
	return s.conn.Close()
}

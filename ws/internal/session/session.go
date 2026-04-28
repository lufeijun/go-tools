package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
)

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

type SessionConfig struct {
	PingInterval      time.Duration
	PongTimeout       time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      int
}

type Session struct {
	connection  conn.Conn
	heartbeater Heartbeater
	config      SessionConfig

	stateChan chan State
	state     State

	readChan  <-chan conn.Message
	writeChan chan<- conn.Message
}

func NewSession(c conn.Conn, cfg SessionConfig) *Session {
	return &Session{
		connection: c,
		config:     cfg,
		stateChan:  make(chan State, 16),
		state:      StateDisconnected,
		readChan:   c.ReadChan(),
		writeChan:  c.WriteChan(),
	}
}

func (s *Session) State() State                  { return s.state }
func (s *Session) StateChan() <-chan State       { return s.stateChan }
func (s *Session) ReadChan() <-chan conn.Message  { return s.readChan }
func (s *Session) WriteChan() chan<- conn.Message { return s.writeChan }
func (s *Session) Conn() conn.Conn               { return s.connection }

func (s *Session) SetState(st State) {
	s.state = st
	select {
	case s.stateChan <- st:
	default:
	}
}

func (s *Session) SetHeartbeater(hb Heartbeater) {
	s.heartbeater = hb
}

func (s *Session) Heartbeater() Heartbeater {
	return s.heartbeater
}

func (s *Session) Close() error {
	if s.heartbeater != nil {
		s.heartbeater.Stop()
	}
	s.SetState(StateClosed)
	return s.connection.Close()
}

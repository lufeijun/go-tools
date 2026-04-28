package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
)

type Reconnector struct {
	interval   time.Duration
	maxRetries int
	dial       func() (conn.Conn, error)
	stopChan   chan struct{}
}

func NewReconnector(interval time.Duration, maxRetries int, dial func() (conn.Conn, error)) *Reconnector {
	return &Reconnector{
		interval:   interval,
		maxRetries: maxRetries,
		dial:       dial,
		stopChan:   make(chan struct{}),
	}
}

func (r *Reconnector) Start(s *Session) {
	s.SetState(StateReconnecting)

	for i := 0; i < r.maxRetries; i++ {
		select {
		case <-r.stopChan:
			return
		case <-time.After(r.interval):
		}

		s.SetState(StateConnecting)
		c, err := r.dial()
		if err != nil {
			s.SetState(StateReconnecting)
			continue
		}

		s.connection = c
		s.readChan = c.ReadChan()
		s.writeChan = c.WriteChan()
		s.SetState(StateConnected)
		return
	}

	s.SetState(StateClosed)
}

func (r *Reconnector) Stop() {
	select {
	case r.stopChan <- struct{}{}:
	default:
	}
}

package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/conn"
)

// Reconnector is the auto-reconnect interface (client-side).
type Reconnector interface {
	Start(s Session)
	Stop()
}

// reconnector implements auto-reconnect logic.
type reconnector struct {
	interval   time.Duration
	maxRetries int
	dial       func() (conn.Conn, error)
	stopChan   chan struct{}
}

// NewReconnector creates a reconnector.
func NewReconnector(interval time.Duration, maxRetries int, dial func() (conn.Conn, error)) Reconnector {
	return &reconnector{
		interval:   interval,
		maxRetries: maxRetries,
		dial:       dial,
		stopChan:   make(chan struct{}),
	}
}

func (r *reconnector) Start(s Session) {
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

		// TODO: update session conn, read/write channels (V2.1)
		_ = c
		s.SetState(StateConnected)
		return
	}
	s.SetState(StateClosed)
}

func (r *reconnector) Stop() {
	select {
	case r.stopChan <- struct{}{}:
	default:
	}
}

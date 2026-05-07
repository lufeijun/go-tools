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

// reconnector implements auto-reconnect with exponential backoff.
type reconnector struct {
	interval    time.Duration
	maxInterval time.Duration
	maxRetries  int
	dial        func() (conn.Conn, error)
	stopChan    chan struct{}
}

const maxBackoffInterval = 60 * time.Second

// NewReconnector creates a reconnector with exponential backoff.
// interval is the initial retry interval (doubled each attempt up to 60s).
// maxRetries caps the number of reconnect attempts.
func NewReconnector(interval time.Duration, maxRetries int, dial func() (conn.Conn, error)) Reconnector {
	return &reconnector{
		interval:    interval,
		maxInterval: maxBackoffInterval,
		maxRetries:  maxRetries,
		dial:        dial,
		stopChan:    make(chan struct{}),
	}
}

func (r *reconnector) Start(s Session) {
	s.SetState(StateReconnecting)
	delay := r.interval
	for i := 0; i < r.maxRetries; i++ {
		timer := time.NewTimer(delay)
		select {
		case <-r.stopChan:
			timer.Stop()
			return
		case <-timer.C:
		}

		s.SetState(StateConnecting)
		c, err := r.dial()
		if err != nil {
			s.SetState(StateReconnecting)
			// Exponential backoff, capped at maxInterval.
			delay *= 2
			if delay > r.maxInterval {
				delay = r.maxInterval
			}
			continue
		}

		s.SetConn(c)
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

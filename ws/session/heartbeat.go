package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/conn"
)

// Heartbeater is the heartbeat interface.
type Heartbeater interface {
	Start(s Session)
	Stop()
	SetOnTimeout(fn func())
}

// perConnHeartbeater sends ping frames at regular intervals.
type perConnHeartbeater struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	stopChan     chan struct{}
	onTimeout    func()
}

// NewPerConnHeartbeater creates a per-connection heartbeater.
func NewPerConnHeartbeater(pingInterval, pongTimeout time.Duration) Heartbeater {
	return &perConnHeartbeater{
		pingInterval: pingInterval,
		pongTimeout:  pongTimeout,
		stopChan:     make(chan struct{}),
	}
}

func (h *perConnHeartbeater) Start(s Session) {
	h.stopChan = make(chan struct{})
	go h.run(s)
}

func (h *perConnHeartbeater) Stop() {
	select {
	case h.stopChan <- struct{}{}:
	default:
	}
}

func (h *perConnHeartbeater) SetOnTimeout(fn func()) {
	h.onTimeout = fn
}

func (h *perConnHeartbeater) run(s Session) {
	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if s.Conn() != nil && s.Conn().Pipeline() != nil {
				s.Conn().Pipeline().FireChannelWrite(&conn.Message{Type: 0x9})
			}
		case <-h.stopChan:
			return
		}
	}
}

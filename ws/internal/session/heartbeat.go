package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/internal/conn"
)

// Heartbeater sends periodic Ping frames to keep the connection alive.
// V1: only sends Ping; Pong timeout detection is deferred to V2 (time wheel).
type Heartbeater interface {
	Start(c conn.Conn)
	Stop()
	SetOnTimeout(fn func())
}

type perConnHeartbeater struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	stopChan     chan struct{}
	onTimeout    func()
}

func NewPerConnHeartbeater(pingInterval, pongTimeout time.Duration) *perConnHeartbeater {
	return &perConnHeartbeater{
		pingInterval: pingInterval,
		pongTimeout:  pongTimeout,
		stopChan:     make(chan struct{}),
	}
}

func (h *perConnHeartbeater) Start(c conn.Conn) {
	h.stopChan = make(chan struct{})
	go h.run(c)
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

// run sends Ping frames at the configured interval.
// It does NOT consume from ReadChan — that channel is owned by the user
// via Session.ReadChan(). If the Ping write fails (connection dead), the
// heartbeat goroutine exits. Pong timeout detection is a V2 concern.
func (h *perConnHeartbeater) run(c conn.Conn) {
	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			select {
			case c.WriteChan() <- conn.Message{Type: frame.OpcodePing, Data: []byte{}}:
			case <-h.stopChan:
				return
			}

		case <-h.stopChan:
			return
		}
	}
}

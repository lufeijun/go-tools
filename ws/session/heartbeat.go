package session

import (
	"sync"
	"time"

	"github.com/lufeijun/goTools/ws/conn"
)

// defaultTimingWheel is the package-level timing wheel shared by all
// perConnHeartbeaters.  It is lazily started on first use.
var defaultTimingWheel = NewTimingWheel(time.Second, 128)
var twOnce sync.Once

func ensureTimingWheel() {
	twOnce.Do(func() {
		defaultTimingWheel.Start()
	})
}

// Heartbeater is the heartbeat interface.
type Heartbeater interface {
	Start(s Session)
	Stop()
	Reset(s Session) // cancels the pending ping and re-schedules it
	SetOnTimeout(fn func())
}

// perConnHeartbeater sends ping frames at regular intervals using the
// shared timing wheel instead of a dedicated goroutine per connection.
type perConnHeartbeater struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	onTimeout    func()

	tw       *TimingWheel
	pingTask int64
	stopOnce sync.Once
}

// NewPerConnHeartbeater creates a per-connection heartbeater.
func NewPerConnHeartbeater(pingInterval, pongTimeout time.Duration) Heartbeater {
	ensureTimingWheel()
	return &perConnHeartbeater{
		pingInterval: pingInterval,
		pongTimeout:  pongTimeout,
		tw:           defaultTimingWheel,
	}
}

func (h *perConnHeartbeater) Start(s Session) {
	h.schedulePing(s)
}

func (h *perConnHeartbeater) Stop() {
	h.stopOnce.Do(func() {
		if h.pingTask != 0 {
			h.tw.Cancel(h.pingTask)
		}
	})
}

func (h *perConnHeartbeater) Reset(s Session) {
	if h.pingTask != 0 {
		h.tw.Cancel(h.pingTask)
	}
	h.schedulePing(s)
}

func (h *perConnHeartbeater) SetOnTimeout(fn func()) {
	h.onTimeout = fn
}

func (h *perConnHeartbeater) schedulePing(s Session) {
	if h.pingInterval <= 0 {
		return
	}
	h.pingTask = h.tw.Add(h.pingInterval, func() {
		// Guard against stopped heartbeater or closed session.
		if s.Conn() == nil || s.Conn().Pipeline() == nil {
			return
		}
		s.Conn().Pipeline().FireChannelWrite(&conn.Message{Type: 0x9})
		// Re-schedule the next ping.
		h.schedulePing(s)
	})
}

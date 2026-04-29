package eventloop

// EventHandler is the callback interface for fd events.
type EventHandler interface {
	OnEvent(fd int, events uint32)
}

// EventLoop manages a set of file descriptors and dispatches events.
type EventLoop interface {
	Register(fd int, handler EventHandler) error
	Deregister(fd int) error
	Wake()
	Run() error
	Stop() error
}

// Poller is the low-level OS-specific polling abstraction.
type Poller interface {
	Open() error
	Close() error
	Add(fd int, events uint32) error
	Mod(fd int, events uint32) error
	Del(fd int) error
	Wait(timeoutMs int) ([]Event, error)
}

// Event represents a single fd event.
type Event struct {
	FD     int
	Events uint32
}

const (
	EventRead  uint32 = 1 << iota
	EventWrite
	EventError
	EventHup
)

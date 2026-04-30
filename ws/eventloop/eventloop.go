package eventloop

import (
	"errors"
	"sync"
	"sync/atomic"
)

// EventHandler is the callback interface for fd events.
type EventHandler interface {
	OnEvent(fd int, events uint32)
}

// EventLoop manages a set of file descriptors and dispatches events.
type EventLoop interface {
	Register(fd int, handler EventHandler) error
	Deregister(fd int) error
	Mod(fd int, events uint32) error
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

type defaultEventLoop struct {
	poller   Poller
	handlers map[int]EventHandler
	mu       sync.RWMutex
	running  int32
	stopCh   chan struct{}
}

// NewEventLoop creates a new EventLoop backed by the given Poller.
func NewEventLoop(p Poller) EventLoop {
	return &defaultEventLoop{
		poller:   p,
		handlers: make(map[int]EventHandler),
		stopCh:   make(chan struct{}),
	}
}

// Register adds fd to the poller and associates it with a handler.
func (el *defaultEventLoop) Register(fd int, handler EventHandler) error {
	el.mu.Lock()
	defer el.mu.Unlock()
	el.handlers[fd] = handler
	return el.poller.Add(fd, EventRead)
}

// Deregister removes fd from the poller.
func (el *defaultEventLoop) Deregister(fd int) error {
	el.mu.Lock()
	defer el.mu.Unlock()
	delete(el.handlers, fd)
	return el.poller.Del(fd)
}

// Mod updates the events mask for an already-registered fd.
func (el *defaultEventLoop) Mod(fd int, events uint32) error {
	return el.poller.Mod(fd, events)
}

// Wake interrupts the poller wait (not yet implemented).
func (el *defaultEventLoop) Wake() {
	// TODO: implement wake
}

// Run starts the event loop. It is start-once.
func (el *defaultEventLoop) Run() error {
	if !atomic.CompareAndSwapInt32(&el.running, 0, 1) {
		return errors.New("already running")
	}
	defer atomic.StoreInt32(&el.running, 0)

	if err := el.poller.Open(); err != nil {
		return err
	}

	for {
		select {
		case <-el.stopCh:
			return nil
		default:
		}

		events, err := el.poller.Wait(100)
		if err != nil {
			return err
		}

		for _, e := range events {
			el.mu.RLock()
			h, ok := el.handlers[e.FD]
			el.mu.RUnlock()
			if ok {
				h.OnEvent(e.FD, e.Events)
			}
		}
	}
}

// Stop signals the event loop to exit.
func (el *defaultEventLoop) Stop() error {
	if !atomic.CompareAndSwapInt32(&el.running, 1, 0) {
		return errors.New("not running")
	}
	close(el.stopCh)
	return nil
}

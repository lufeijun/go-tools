package eventloop

import (
	"errors"
	"runtime"
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

// workerPool is a fixed-size goroutine pool for dispatching handler events.
type workerPool struct {
	taskCh chan func()
	stopCh chan struct{}
}

func newWorkerPool(workers int) *workerPool {
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	p := &workerPool{
		taskCh: make(chan func(), 1024),
		stopCh: make(chan struct{}),
	}
	for i := 0; i < workers; i++ {
		go p.run()
	}
	return p
}

func (p *workerPool) run() {
	for {
		select {
		case fn := <-p.taskCh:
			fn()
		case <-p.stopCh:
			return
		}
	}
}

func (p *workerPool) submit(fn func()) {
	select {
	case p.taskCh <- fn:
	case <-p.stopCh:
	}
}

func (p *workerPool) stop() {
	close(p.stopCh)
}

type defaultEventLoop struct {
	poller      Poller
	handlersVal atomic.Value // stores map[int]EventHandler
	running     int32
	stopCh      chan struct{}
	pool        *workerPool
}

// NewEventLoop creates a new EventLoop backed by the given Poller.
// It uses a goroutine pool sized to GOMAXPROCS for async handler dispatch.
func NewEventLoop(p Poller) EventLoop {
	return NewEventLoopWithPool(p, runtime.GOMAXPROCS(0))
}

// NewEventLoopWithPool creates a new EventLoop with a configurable worker pool size.
// A workers value <= 0 defaults to GOMAXPROCS.
func NewEventLoopWithPool(p Poller, workers int) EventLoop {
	el := &defaultEventLoop{
		poller: p,
		stopCh: make(chan struct{}),
		pool:   newWorkerPool(workers),
	}
	el.handlersVal.Store(make(map[int]EventHandler))
	return el
}

func (el *defaultEventLoop) loadHandlers() map[int]EventHandler {
	return el.handlersVal.Load().(map[int]EventHandler)
}

func (el *defaultEventLoop) storeHandler(fd int, handler EventHandler) {
	old := el.loadHandlers()
	newMap := make(map[int]EventHandler, len(old)+1)
	for k, v := range old {
		newMap[k] = v
	}
	newMap[fd] = handler
	el.handlersVal.Store(newMap)
}

func (el *defaultEventLoop) deleteHandler(fd int) {
	old := el.loadHandlers()
	newMap := make(map[int]EventHandler, len(old))
	for k, v := range old {
		if k != fd {
			newMap[k] = v
		}
	}
	el.handlersVal.Store(newMap)
}

// Register adds fd to the poller and associates it with a handler.
func (el *defaultEventLoop) Register(fd int, handler EventHandler) error {
	el.storeHandler(fd, handler)
	return el.poller.Add(fd, EventRead)
}

// Deregister removes fd from the poller.
func (el *defaultEventLoop) Deregister(fd int) error {
	el.deleteHandler(fd)
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
			h, ok := el.loadHandlers()[e.FD]
			if ok {
				el.pool.submit(func() {
					h.OnEvent(e.FD, e.Events)
				})
			}
		}
	}
}

// Stop signals the event loop to exit and cleans up all registered fds.
func (el *defaultEventLoop) Stop() error {
	if !atomic.CompareAndSwapInt32(&el.running, 1, 0) {
		return errors.New("not running")
	}
	close(el.stopCh)
	el.pool.stop()

	// Deregister all fds and close the poller.
	handlers := el.loadHandlers()
	fds := make([]int, 0, len(handlers))
	for fd := range handlers {
		fds = append(fds, fd)
	}
	el.handlersVal.Store(make(map[int]EventHandler))

	for _, fd := range fds {
		_ = el.poller.Del(fd)
	}
	_ = el.poller.Close()
	return nil
}

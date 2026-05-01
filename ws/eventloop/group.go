package eventloop

import "sync/atomic"

// EventLoopGroup manages a pool of EventLoops and distributes connections across them.
type EventLoopGroup interface {
	Start() error
	Stop() error
	Next() EventLoop
	Count() int
}

// NewEventLoopGroup creates a group of EventLoops with the given size.
// newPoller is called once per EventLoop to create its Poller.
func NewEventLoopGroup(workers int, newPoller func() Poller) EventLoopGroup {
	if workers <= 0 {
		workers = 1
	}
	loops := make([]EventLoop, workers)
	for i := range loops {
		p := newPoller()
		loops[i] = NewEventLoopWithPool(p, 1)
	}
	return &roundRobinEventLoopGroup{
		loops:  loops,
		nextFd: 0,
	}
}

type roundRobinEventLoopGroup struct {
	loops  []EventLoop
	nextFd uint64
}

func (g *roundRobinEventLoopGroup) Start() error {
	for _, el := range g.loops {
		go el.Run()
	}
	return nil
}

func (g *roundRobinEventLoopGroup) Stop() error {
	for _, el := range g.loops {
		_ = el.Stop()
	}
	return nil
}

func (g *roundRobinEventLoopGroup) Next() EventLoop {
	n := atomic.AddUint64(&g.nextFd, 1)
	return g.loops[(n-1)%uint64(len(g.loops))]
}

func (g *roundRobinEventLoopGroup) Count() int {
	return len(g.loops)
}

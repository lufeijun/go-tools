package eventloop

import "testing"

func TestRoundRobinEventLoopGroup_Next(t *testing.T) {
	g := NewEventLoopGroup(3, newTestPoller)
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	el0 := g.Next()
	el1 := g.Next()
	el2 := g.Next()
	el3 := g.Next()

	if el0 == nil || el1 == nil || el2 == nil {
		t.Fatal("Next returned nil")
	}
	if el3 != el0 {
		t.Error("round-robin did not wrap around")
	}
	if g.Count() != 3 {
		t.Errorf("Count = %d, want 3", g.Count())
	}
}

func TestRoundRobinEventLoopGroup_SingleWorker(t *testing.T) {
	g := NewEventLoopGroup(1, newTestPoller)
	if err := g.Start(); err != nil {
		t.Fatal(err)
	}
	defer g.Stop()

	el0 := g.Next()
	el1 := g.Next()
	if el0 != el1 {
		t.Error("single-worker group should always return same loop")
	}
}

type testPoller struct{}

func newTestPoller() Poller { return &testPoller{} }

func (p *testPoller) Open() error                         { return nil }
func (p *testPoller) Close() error                        { return nil }
func (p *testPoller) Add(fd int, events uint32) error     { return nil }
func (p *testPoller) Mod(fd int, events uint32) error     { return nil }
func (p *testPoller) Del(fd int) error                    { return nil }
func (p *testPoller) Wait(timeoutMs int) ([]Event, error) { return nil, nil }

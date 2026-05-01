package eventloop

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockHandler struct {
	called atomic.Bool
	fd     atomic.Int64
	evts   atomic.Uint32
}

func (m *mockHandler) OnEvent(fd int, events uint32) {
	m.called.Store(true)
	m.fd.Store(int64(fd))
	m.evts.Store(events)
}

func (m *mockHandler) wasCalled() bool    { return m.called.Load() }
func (m *mockHandler) lastFd() int        { return int(m.fd.Load()) }
func (m *mockHandler) lastEvents() uint32 { return m.evts.Load() }

func TestEventConstants(t *testing.T) {
	if EventRead != 1 {
		t.Errorf("EventRead = %d, want 1", EventRead)
	}
	if EventWrite != 2 {
		t.Errorf("EventWrite = %d, want 2", EventWrite)
	}
	if EventError != 4 {
		t.Errorf("EventError = %d, want 4", EventError)
	}
	if EventHup != 8 {
		t.Errorf("EventHup = %d, want 8", EventHup)
	}
}

func TestEvent_String(t *testing.T) {
	e := Event{FD: 3, Events: EventRead | EventWrite}
	if e.FD != 3 {
		t.Errorf("FD = %d, want 3", e.FD)
	}
}

type mockPoller struct {
	mu          sync.Mutex
	openCalled  bool
	closeCalled bool
	added       map[int]uint32
	deleted     map[int]bool
	events      []Event
	waitErr     error
	waitCount   int
}

func (m *mockPoller) Open() error {
	m.openCalled = true
	return nil
}

func (m *mockPoller) Close() error {
	m.closeCalled = true
	return nil
}

func (m *mockPoller) Add(fd int, events uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.added == nil {
		m.added = make(map[int]uint32)
	}
	m.added[fd] = events
	return nil
}

func (m *mockPoller) Mod(fd int, events uint32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.added == nil {
		m.added = make(map[int]uint32)
	}
	m.added[fd] = events
	return nil
}

func (m *mockPoller) Del(fd int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleted == nil {
		m.deleted = make(map[int]bool)
	}
	m.deleted[fd] = true
	delete(m.added, fd)
	return nil
}

func (m *mockPoller) Wait(timeoutMs int) ([]Event, error) {
	m.mu.Lock()
	m.waitCount++
	if m.waitErr != nil {
		m.mu.Unlock()
		return nil, m.waitErr
	}
	evts := m.events
	m.events = nil
	m.mu.Unlock()
	return evts, nil
}

func TestDefaultEventLoop_RegisterDeregister(t *testing.T) {
	mp := &mockPoller{}
	el := NewEventLoop(mp)

	h := &mockHandler{}
	if err := el.Register(1, h); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	mp.mu.Lock()
	if _, ok := mp.added[1]; !ok {
		t.Error("expected fd 1 to be added to poller")
	}
	mp.mu.Unlock()

	if err := el.Deregister(1); err != nil {
		t.Fatalf("Deregister failed: %v", err)
	}

	mp.mu.Lock()
	if _, ok := mp.added[1]; ok {
		t.Error("expected fd 1 to be removed from poller")
	}
	mp.mu.Unlock()
}

func TestDefaultEventLoop_Dispatch(t *testing.T) {
	mp := &mockPoller{
		events: []Event{{FD: 1, Events: EventRead}},
	}
	el := NewEventLoop(mp)

	h := &mockHandler{}
	if err := el.Register(1, h); err != nil {
		t.Fatalf("Register failed: %v", err)
	}

	go func() {
		if err := el.Run(); err != nil {
			t.Logf("Run returned: %v", err)
		}
	}()

	for i := 0; i < 50; i++ {
		if h.wasCalled() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !h.wasCalled() {
		t.Error("expected handler to be called")
	}
	if h.lastFd() != 1 {
		t.Errorf("handler fd = %d, want 1", h.lastFd())
	}
	if h.lastEvents() != EventRead {
		t.Errorf("handler events = %d, want %d", h.lastEvents(), EventRead)
	}

	if err := el.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
}

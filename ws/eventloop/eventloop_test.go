package eventloop

import (
	"testing"
)

type mockHandler struct {
	called bool
	fd     int
	evts   uint32
}

func (m *mockHandler) OnEvent(fd int, events uint32) {
	m.called = true
	m.fd = fd
	m.evts = events
}

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

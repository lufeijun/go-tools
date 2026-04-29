package pipeline

import (
	"testing"
)

type recorderInbound struct {
	testHandler
	reads    []interface{}
	active   bool
	inactive bool
}

func (h *recorderInbound) ChannelRead(ctx Context, msg interface{}) {
	h.reads = append(h.reads, msg)
	ctx.FireChannelRead(msg)
}
func (h *recorderInbound) ChannelActive(ctx Context)   { h.active = true }
func (h *recorderInbound) ChannelInactive(ctx Context) { h.inactive = true }
func (h *recorderInbound) ExceptionCaught(ctx Context, err error) {}

type recorderOutbound struct {
	testHandler
	writes []interface{}
}

func (h *recorderOutbound) Write(ctx Context, msg interface{}) {
	h.writes = append(h.writes, msg)
	ctx.Write(msg)
}
func (h *recorderOutbound) Flush(ctx Context) {}

func TestDefaultPipeline_InboundChain(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	h2 := &recorderInbound{testHandler: testHandler{name: "h2"}}
	p.AddLast("h1", h1)
	p.AddLast("h2", h2)

	p.FireChannelRead("hello")

	if len(h1.reads) != 1 || h1.reads[0] != "hello" {
		t.Errorf("h1.reads = %v, want [hello]", h1.reads)
	}
	if len(h2.reads) != 1 || h2.reads[0] != "hello" {
		t.Errorf("h2.reads = %v, want [hello]", h2.reads)
	}
}

func TestDefaultPipeline_OutboundChain(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderOutbound{testHandler: testHandler{name: "h1"}}
	h2 := &recorderOutbound{testHandler: testHandler{name: "h2"}}
	p.AddLast("h1", h1)
	p.AddLast("h2", h2)

	// Outbound: from tail to head
	p.FireChannelWrite("world")

	if len(h2.writes) != 1 || h2.writes[0] != "world" {
		t.Errorf("h2.writes = %v, want [world]", h2.writes)
	}
	if len(h1.writes) != 1 || h1.writes[0] != "world" {
		t.Errorf("h1.writes = %v, want [world]", h1.writes)
	}
}

func TestDefaultPipeline_Remove(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	h2 := &recorderInbound{testHandler: testHandler{name: "h2"}}
	p.AddLast("h1", h1)
	p.AddLast("h2", h2)
	p.Remove("h1")

	p.FireChannelRead("msg")
	if len(h1.reads) != 0 {
		t.Error("h1 should have been removed")
	}
	if len(h2.reads) != 1 {
		t.Error("h2 should have received msg")
	}
}

func TestDefaultPipeline_ActiveInactive(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	p.AddLast("h1", h1)
	p.FireChannelActive()
	if !h1.active {
		t.Error("h1 should be active")
	}
	p.FireChannelInactive()
	if !h1.inactive {
		t.Error("h1 should be inactive")
	}
}

func TestDefaultPipeline_DuplicateName(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	p.AddLast("h1", h1)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for duplicate handler name")
		}
	}()
	p.AddLast("h1", h1)
}

package pipeline

import "testing"

type testHandler struct {
	name string
}

func (h *testHandler) Name() string { return h.name }

type testInbound struct {
	testHandler
	readCalled bool
}

func (h *testInbound) ChannelRead(ctx Context, msg interface{}) { h.readCalled = true }
func (h *testInbound) ChannelActive(ctx Context)                {}
func (h *testInbound) ChannelInactive(ctx Context)              {}
func (h *testInbound) ExceptionCaught(ctx Context, err error)   {}

type testOutbound struct {
	testHandler
	writeCalled bool
}

func (h *testOutbound) Write(ctx Context, msg interface{}) { h.writeCalled = true }
func (h *testOutbound) Flush(ctx Context)                  {}

func TestHandlerInterfaces(t *testing.T) {
	in := &testInbound{testHandler: testHandler{name: "in"}}
	out := &testOutbound{testHandler: testHandler{name: "out"}}

	if in.Name() != "in" {
		t.Errorf("inbound name = %q, want in", in.Name())
	}
	if out.Name() != "out" {
		t.Errorf("outbound name = %q, want out", out.Name())
	}
	var _ InboundHandler = in
	var _ OutboundHandler = out
}

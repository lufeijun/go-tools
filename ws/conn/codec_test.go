package conn

import (
	"bytes"
	"testing"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type captureContext struct {
	pipeline pipeline.ChannelPipeline
	written  interface{}
}

func (c *captureContext) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *captureContext) FireChannelRead(msg interface{})     {}
func (c *captureContext) FireChannelWrite(msg interface{})    {}
func (c *captureContext) FireChannelActive()                  {}
func (c *captureContext) FireChannelInactive()                {}
func (c *captureContext) FireExceptionCaught(err error)       {}
func (c *captureContext) Write(msg interface{})               { c.written = msg }
func (c *captureContext) Flush()                              {}

func TestFrameCodec_EncodesTextMessage(t *testing.T) {
	codec := &FrameCodec{IsClient: false}
	ctx := &captureContext{}
	msg := &Message{Type: byte(frame.OpcodeText), Data: []byte("Hello")}
	codec.Write(ctx, msg)
	bb, ok := ctx.written.(buf.ByteBuf)
	if !ok {
		t.Fatalf("expected ByteBuf, got %T", ctx.written)
	}
	defer bb.Release()
	raw := bb.ReadAll()
	f, err := frame.ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != frame.OpcodeText {
		t.Errorf("Opcode = %d, want Text", f.Opcode)
	}
	if string(f.Payload) != "Hello" {
		t.Errorf("Payload = %q, want Hello", string(f.Payload))
	}
}

func TestFrameCodec_EncodesClientMasked(t *testing.T) {
	codec := &FrameCodec{IsClient: true}
	ctx := &captureContext{}
	msg := &Message{Type: byte(frame.OpcodeBinary), Data: []byte{0x01, 0x02}}
	codec.Write(ctx, msg)
	bb, ok := ctx.written.(buf.ByteBuf)
	if !ok {
		t.Fatalf("expected ByteBuf, got %T", ctx.written)
	}
	defer bb.Release()
	f, err := frame.ReadFrame(bytes.NewReader(bb.ReadAll()))
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != frame.OpcodeBinary {
		t.Errorf("Opcode = %d, want Binary", f.Opcode)
	}
	if !bytes.Equal(f.Payload, []byte{0x01, 0x02}) {
		t.Errorf("Payload = %v, want [1 2]", f.Payload)
	}
}

func TestFrameCodec_PassthroughNonMessage(t *testing.T) {
	codec := &FrameCodec{IsClient: false}
	ctx := &captureContext{}
	codec.Write(ctx, "not-a-message")
	if ctx.written != nil {
		t.Errorf("expected nil (fire, not write), got %v", ctx.written)
	}
}

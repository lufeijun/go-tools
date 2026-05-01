package conn

import (
	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/pipeline"
)

// FrameCodec is an OutboundHandler that encodes *Message into WebSocket frames.
type FrameCodec struct {
	IsClient bool
	Pool     buf.Pool // optional; if nil a temporary ByteBuf is allocated
}

func (fc *FrameCodec) Name() string { return "frameCodec" }

func (fc *FrameCodec) Write(ctx pipeline.Context, msg interface{}) {
	m, ok := msg.(*Message)
	if !ok {
		ctx.FireChannelWrite(msg)
		return
	}
	f := frame.Frame{
		FIN:     true,
		Opcode:  frame.Opcode(m.Type),
		Payload: m.Data,
		Masked:  fc.IsClient,
	}
	if fc.IsClient {
		f.MaskKey = frame.GenerateMaskKey()
	}
	size := 14 + len(m.Data)
	var bb buf.ByteBuf
	if fc.Pool != nil {
		bb = fc.Pool.Get(size)
	} else {
		bb = buf.NewByteBuf(size)
	}
	_ = frame.WriteFrameTo(bb, f)
	ctx.Write(bb)
}

func (fc *FrameCodec) Flush(ctx pipeline.Context) {}

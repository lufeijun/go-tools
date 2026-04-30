package conn

import (
	"io"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/pipeline"
)

// FrameCodec is an OutboundHandler that encodes *Message into WebSocket frames.
type FrameCodec struct {
	Writer   io.Writer
	IsClient bool
	Pool     buf.Pool // optional; if nil a temporary ByteBuf is allocated
}

func (fc *FrameCodec) Name() string { return "frameCodec" }

func (fc *FrameCodec) Write(ctx pipeline.Context, msg interface{}) {
	if m, ok := msg.(*Message); ok {
		f := frame.Frame{
			FIN:     true,
			Opcode:  frame.Opcode(m.Type),
			Payload: m.Data,
			Masked:  fc.IsClient,
		}
		if fc.IsClient {
			f.MaskKey = frame.GenerateMaskKey()
		}

		var bb buf.ByteBuf
		if fc.Pool != nil {
			bb = fc.Pool.Get(14 + len(m.Data))
		} else {
			bb = buf.NewByteBuf(14 + len(m.Data))
		}
		_ = frame.WriteFrameTo(bb, f)
		_, _ = fc.Writer.Write(bb.ReadAll())
		bb.Release()
	}
	ctx.FireChannelWrite(msg)
}

func (fc *FrameCodec) Flush(ctx pipeline.Context) {}

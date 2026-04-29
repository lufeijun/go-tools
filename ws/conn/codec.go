package conn

import (
	"io"

	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/pipeline"
)

// FrameCodec is an OutboundHandler that encodes *Message into WebSocket frames.
type FrameCodec struct {
	Writer   io.Writer
	IsClient bool
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
		_ = frame.WriteFrame(fc.Writer, f)
	}
	ctx.FireChannelWrite(msg)
}

func (fc *FrameCodec) Flush(ctx pipeline.Context) {}

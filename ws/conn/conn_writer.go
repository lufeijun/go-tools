package conn

import (
	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type ConnWriter struct {
	Conn Conn
}

func (cw *ConnWriter) Name() string { return "headWriter" }

func (cw *ConnWriter) Write(ctx pipeline.Context, msg interface{}) {
	if bb, ok := msg.(buf.ByteBuf); ok {
		_ = cw.Conn.Write(bb)
		return
	}
	ctx.FireChannelWrite(msg)
}

func (cw *ConnWriter) Flush(ctx pipeline.Context) {}

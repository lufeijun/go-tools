package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
	"github.com/lufeijun/goTools/ws/server"
	"github.com/lufeijun/goTools/ws/session"
)

// EchoHandler 是一个 InboundHandler，收到文本消息后加上服务端时间并写回客户端
type EchoHandler struct{}

func (h *EchoHandler) Name() string { return "echo" }

func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
	m, ok := msg.(*conn.Message)
	if !ok || m.Type != byte(0x1) { // 0x1 = Text frame
		ctx.FireChannelRead(msg)
		return
	}

	clientMsg := string(m.Data)
	serverTime := time.Now().Format("2006-01-02 15:04:05.000")
	reply := fmt.Sprintf("服务端时间 %s | 客户端内容: %s", serverTime, clientMsg)

	log.Printf("收到消息: %s\n", clientMsg)
	log.Printf("返回消息: %s\n", reply)

	// 写回客户端；codec handler 会把 *conn.Message 编码成 WebSocket frame
	ctx.Write(&conn.Message{Type: m.Type, Data: []byte(reply)})
	ctx.FireChannelRead(msg)
}

func (h *EchoHandler) ChannelActive(ctx pipeline.Context) {
	fmt.Println("客户端已连接")
	ctx.FireChannelActive()
}

func (h *EchoHandler) ChannelInactive(ctx pipeline.Context) {
	fmt.Println("客户端已断开")
	ctx.FireChannelInactive()
}

func (h *EchoHandler) ExceptionCaught(ctx pipeline.Context, err error) {
	log.Println("异常:", err)
}

func main() {
	cfg := ws.Config{
		Addr:         ":8080",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	}

	srv, err := server.NewServer(cfg)
	if err != nil {
		log.Fatal("创建服务端失败:", err)
	}

	// 每 5 秒打印一次当前连接数
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			count := srv.Hub().Count()
			if count > 0 {
				log.Printf("当前在线连接数: %d\n", count)
			}
		}
	}()

	// 注册连接建立回调：给每个新连接的 Pipeline 添加 EchoHandler
	srv.OnConnect(func(sess session.Session) {
		sess.Conn().Pipeline().AddLast("echo", &EchoHandler{})
	})

	log.Println("服务端启动，监听 :8080 ...")
	go func() {
		if err := srv.Start(); err != nil {
			log.Fatal("服务端启动失败:", err)
		}
	}()

	// 等待中断信号进行优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("正在关闭服务端...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(); err != nil {
		log.Println("关闭失败:", err)
	}
	_ = ctx
	fmt.Println("服务端已停止")
}

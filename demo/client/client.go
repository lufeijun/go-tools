package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/client"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
	"github.com/lufeijun/goTools/ws/session"
)

// PrintHandler 是一个 InboundHandler，负责打印服务端返回的消息
type PrintHandler struct{}

func (h *PrintHandler) Name() string { return "print" }

func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
	m, ok := msg.(*conn.Message)
	if !ok || m.Type != byte(0x1) { // 0x1 = Text frame
		ctx.FireChannelRead(msg)
		return
	}
	fmt.Printf("\n【收到服务端回复】%s\n", string(m.Data))
	fmt.Print("请输入消息: ")
	ctx.FireChannelRead(msg)
}

func (h *PrintHandler) ChannelActive(ctx pipeline.Context) {
	fmt.Println("已连接到服务端")
	ctx.FireChannelActive()
}

func (h *PrintHandler) ChannelInactive(ctx pipeline.Context) {
	fmt.Println("与服务端断开连接")
	ctx.FireChannelInactive()
}

func (h *PrintHandler) ExceptionCaught(ctx pipeline.Context, err error) {
	log.Println("异常:", err)
}

func main() {
	cfg := ws.Config{
		Addr:         "ws://localhost:8080/",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	}

	c, err := client.NewClient(cfg)
	if err != nil {
		log.Fatal("创建客户端失败:", err)
	}

	// 在单独的 goroutine 中监听连接状态变化
	go func() {
		sess := c.Session()
		if sess == nil {
			return
		}
		for st := range sess.StateChan() {
			switch st {
			case session.StateConnecting:
				fmt.Println("【状态】正在连接...")
			case session.StateConnected:
				fmt.Println("【状态】已连接")
			case session.StateDisconnected:
				fmt.Println("【状态】已断开")
			case session.StateClosed:
				fmt.Println("【状态】连接已关闭")
				return
			}
		}
	}()

	if err := c.Connect(); err != nil {
		log.Fatal("连接失败:", err)
	}

	// 注册消息打印 Handler
	c.Session().Conn().Pipeline().AddLast("print", &PrintHandler{})

	fmt.Println("已连接到服务端，输入消息并按回车发送（输入 exit 退出）")
	fmt.Print("请输入消息: ")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		text := scanner.Text()
		if text == "exit" {
			fmt.Println("退出客户端")
			c.Close()
			return
		}
		if text == "" {
			fmt.Print("请输入消息: ")
			continue
		}

		sendTime := time.Now().Format("2006-01-02 15:04:05.000")
		msg := fmt.Sprintf("客户端发送时间 %s | 内容: %s", sendTime, text)

		fmt.Printf("【发送】%s\n", msg)

		// 通过 Pipeline 发送消息；codec handler 会编码成 WebSocket frame
		c.Session().Conn().Pipeline().FireChannelWrite(&conn.Message{Type: byte(0x1), Data: []byte(msg)})
	}
}

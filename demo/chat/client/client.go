package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/client"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type PrintHandler struct{}

func (h *PrintHandler) Name() string { return "print" }

func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
	m, ok := msg.(*conn.Message)
	if ok && m.Type == 0x1 {
		fmt.Printf("\n【收到消息】%s\n", string(m.Data))
		fmt.Print("输入: ")
	}
	ctx.FireChannelRead(msg)
}

func (h *PrintHandler) ChannelActive(ctx pipeline.Context)  { ctx.FireChannelActive() }
func (h *PrintHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *PrintHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: go run client.go <user_id>")
		os.Exit(1)
	}
	userID := os.Args[1]

	c := client.NewClient(ws.Config{
		Addr:         "ws://localhost:8080/",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})
	if err := c.Connect(); err != nil {
		log.Fatal("连接失败:", err)
	}

	sess := c.Session()
	sess.Conn().Pipeline().AddLast("print", &PrintHandler{})

	// 发送登录消息
	login, _ := json.Marshal(map[string]string{"type": "login", "user_id": userID})
	sess.Conn().Pipeline().FireChannelWrite(&conn.Message{Type: 0x1, Data: login})

	fmt.Printf("已连接，用户ID: %s\n", userID)
	fmt.Println("发送格式: 输入接收方ID 和 消息内容")
	fmt.Print("输入: ")

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		text := scanner.Text()
		if text == "exit" {
			fmt.Println("退出客户端")
			c.Close()
			return
		}

		var to, content string
		if n, _ := fmt.Sscanf(text, "%s %s", &to, &content); n < 2 {
			fmt.Println("格式错误，请使用: 接收方ID 消息内容")
			fmt.Print("输入: ")
			continue
		}

		msg, _ := json.Marshal(map[string]string{
			"type": "chat",
			"to":   to,
			"text": content,
		})
		sess.Conn().Pipeline().FireChannelWrite(&conn.Message{Type: 0x1, Data: msg})
		fmt.Print("输入: ")
	}
}

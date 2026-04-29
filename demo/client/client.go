package main

import (
	"fmt"
	"log"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/client"
	"github.com/lufeijun/goTools/ws/session"
)

func main() {
	cfg := ws.Config{
		Addr:         "ws://localhost:8080/",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	}

	c := client.NewClient(cfg)

	// 在单独的 goroutine 中监听连接状态变化
	go func() {
		sess := c.Session()
		if sess == nil {
			return
		}
		for st := range sess.StateChan() {
			switch st {
			case session.StateConnecting:
				fmt.Println("[状态] 正在连接...")
			case session.StateConnected:
				fmt.Println("[状态] 已连接")
			case session.StateDisconnected:
				fmt.Println("[状态] 已断开")
			case session.StateClosed:
				fmt.Println("[状态] 连接已关闭")
				return
			}
		}
	}()

	if err := c.Connect(); err != nil {
		log.Fatal("连接失败:", err)
	}

	fmt.Println("已连接到服务端，按 Ctrl+C 退出")

	// 保持运行，让心跳保活机制持续工作
	// V2.1 将支持通过 Pipeline 发送和接收消息
	for {
		time.Sleep(1 * time.Second)
	}
}

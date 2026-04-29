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
	"github.com/lufeijun/goTools/ws/server"
)

func main() {
	cfg := ws.Config{
		Addr:         ":8080",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	}

	srv := server.NewServer(cfg)

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

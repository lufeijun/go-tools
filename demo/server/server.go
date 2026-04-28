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
)

func main() {
	srv := ws.NewServer(ws.ServerConfig{
		Addr:         ":8080",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	go srv.Hub().Run()

	go func() {
		for sess := range srv.ConnChan() {
			go func(s *ws.Session) {
				for msg := range s.ReadChan() {
					if msg.Type == ws.OpcodeText {

						sendtime := time.Now().Format("2006-01-02 15:04:05")

						fmt.Printf("接收时间:%s，recv: %s\n", sendtime, string(msg.Data))

						msg.Data = fmt.Appendf(nil, "服务端: %s", sendtime+": "+string(msg.Data))

						s.WriteChan() <- msg
					}
				}
			}(sess)
		}
	}()

	go func() {
		for sess := range srv.ConnChan() {
			srv.Hub().Register(sess)
		}
	}()

	log.Println("echo server on :8080")
	go srv.ListenAndServe()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	ctx, cancel := context.WithTimeout(context.Background(), 5e9)
	defer cancel()
	srv.Shutdown(ctx)
	fmt.Println("server stopped")
}

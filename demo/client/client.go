package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lufeijun/goTools/ws"
)

func main() {
	client := ws.NewClient(ws.ClientConfig{
		URL: "ws://localhost:8080/",
	})

	if err := client.Connect(); err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	// 接收消息
	go func() {
		for msg := range client.ReadChan() {
			if msg.Type == ws.OpcodeText {
				fmt.Printf("echo: %s\n", string(msg.Data))
			}
		}
		fmt.Println("disconnected")
		os.Exit(0)
	}()

	// 监听状态
	go func() {
		for state := range client.StateChan() {
			fmt.Println("state:", state)
		}
	}()

	// 读取终端输入并发送
	fmt.Println("type message and press enter (ctrl+c to quit):")
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		text := scanner.Text()
		if text == "" {
			continue
		}
		sendtime := time.Now().Format("2006-01-02 15:04:05")
		fmt.Println("发送时间", sendtime)
		text = sendtime + ": " + text
		client.Send(ws.Message{Type: ws.OpcodeText, Data: []byte(text)})
	}
}

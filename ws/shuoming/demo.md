# demo — 完整可运行示例

`demo/` 目录包含一个完整的 Echo 示例，演示服务端和客户端如何通过 `ws` v2 框架进行实时双向通信。客户端发送消息并附带发送时间，服务端收到后追加服务端时间并原样返回。

---

## 目录结构

```
demo/
├── server/
│   └── server.go   # Echo 服务端
└── client/
    └── client.go   # 交互式客户端
```

---

## 运行方式

**终端 1：启动服务端**

```bash
cd demo/server
go run server.go
```

输出：

```
2026/04/29 14:00:00 服务端启动，监听 :8080 ...
```

**终端 2：启动客户端**

```bash
cd demo/client
go run client.go
```

输出：

```
已连接到服务端
已连接到服务端，输入消息并按回车发送（输入 exit 退出）
请输入消息: 
```

---

## 服务端详解

### 功能

1. 监听 `:8080`，接受 WebSocket 连接
2. 为每个连接注册 `EchoHandler`
3. 收到文本消息后，追加服务端时间戳并写回客户端
4. 每 5 秒打印当前在线连接数
5. 支持 `Ctrl+C` 优雅关闭

### EchoHandler

```go
type EchoHandler struct{}

func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    m, ok := msg.(*conn.Message)
    if !ok || m.Type != byte(0x1) { // 只处理文本帧
        ctx.FireChannelRead(msg)
        return
    }

    clientMsg := string(m.Data)
    serverTime := time.Now().Format("2006-01-02 15:04:05.000")
    reply := fmt.Sprintf("服务端时间 %s | 客户端内容: %s", serverTime, clientMsg)

    log.Printf("收到: %s\n", clientMsg)
    log.Printf("返回: %s\n", reply)

    ctx.Write(&conn.Message{Type: m.Type, Data: []byte(reply)})
    ctx.FireChannelRead(msg)
}
```

**关键点：**

- `ctx.Write()` 触发 Outbound 链，将消息编码为 WebSocket 帧后发回客户端
- `ctx.FireChannelRead(msg)` 继续传给下一个 InboundHandler（如果存在）

### 主程序

```go
func main() {
    cfg := ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    srv := server.NewServer(cfg)

    // 定时打印在线人数
    go func() {
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            if count := srv.Hub().Count(); count > 0 {
                log.Printf("当前在线: %d\n", count)
            }
        }
    }()

    // 连接建立回调：注册 EchoHandler
    srv.OnConnect(func(sess session.Session) {
        sess.Conn().Pipeline().AddLast("echo", &EchoHandler{})
    })

    log.Println("服务端启动，监听 :8080 ...")
    go func() {
        if err := srv.Start(); err != nil {
            log.Fatal(err)
        }
    }()

    // 优雅关闭
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    srv.Stop()
    log.Println("服务端已停止")
}
```

---

## 客户端详解

### 功能

1. 连接 `ws://localhost:8080/`
2. 注册 `PrintHandler`，打印服务端返回的消息
3. 从标准输入读取用户输入，发送到服务端
4. 输入 `exit` 退出
5. 监听连接状态变化

### PrintHandler

```go
type PrintHandler struct{}

func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    m, ok := msg.(*conn.Message)
    if !ok || m.Type != byte(0x1) {
        ctx.FireChannelRead(msg)
        return
    }
    fmt.Printf("\n【收到服务端回复】%s\n", string(m.Data))
    fmt.Print("请输入消息: ")
    ctx.FireChannelRead(msg)
}
```

### 主程序

```go
func main() {
    cfg := ws.Config{
        Addr:         "ws://localhost:8080/",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    c := client.NewClient(cfg)

    // 状态监听
    go func() {
        sess := c.Session()
        if sess == nil { return }
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

    c.Session().Conn().Pipeline().AddLast("print", &PrintHandler{})

    fmt.Println("已连接到服务端，输入消息并按回车发送（输入 exit 退出）")
    fmt.Print("请输入消息: ")

    scanner := bufio.NewScanner(os.Stdin)
    for scanner.Scan() {
        text := scanner.Text()
        if text == "exit" {
            c.Close()
            return
        }

        sendTime := time.Now().Format("2006-01-02 15:04:05.000")
        msg := fmt.Sprintf("客户端发送时间 %s | 内容: %s", sendTime, text)

        c.Session().Conn().Pipeline().FireChannelWrite(
            &conn.Message{Type: byte(0x1), Data: []byte(msg)})
    }
}
```

---

## 交互示例

**客户端输入：**

```
请输入消息: hello
【发送】客户端发送时间 2026-04-29 14:05:00.123 | 内容: hello

【收到服务端回复】服务端时间 2026-04-29 14:05:00.125 | 客户端内容: 客户端发送时间 2026-04-29 14:05:00.123 | 内容: hello
请输入消息: 
```

**服务端日志：**

```
2026/04/29 14:00:00 服务端启动，监听 :8080 ...
客户端已连接
2026/04/29 14:05:00 收到: 客户端发送时间 2026-04-29 14:05:00.123 | 内容: hello
2026/04/29 14:05:00 返回: 服务端时间 2026-04-29 14:05:00.125 | 客户端内容: 客户端发送时间 2026-04-29 14:05:00.123 | 内容: hello
客户端已断开
```

---

## 扩展思路

1. **多客户端** — 同时启动多个客户端，观察 Hub.Count() 变化
2. **聊天室** — 将 EchoHandler 改为 Broadcast，实现一人说话全员可见
3. **自定义 Handler** — 在 Pipeline 中添加认证、限流、日志等 Handler
4. **状态机扩展** — 利用 `StateChan` 做断线重连提示、连接质量监控

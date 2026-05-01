# demo — 完整可运行示例

`demo/` 目录包含完整的可运行示例，演示服务端和客户端如何通过 `ws` v2 框架进行实时双向通信。

---

## 目录结构

```
demo/
├── server/
│   └── server.go   # Echo 服务端
├── client/
│   └── client.go   # 交互式客户端
└── chat/
    ├── server/
    │   └── server.go   # 聊天室服务端（Hub 广播 + userID 绑定）
    └── client/
        └── client.go   # 聊天室客户端
```

---

## 示例 1：Echo（服务端 + 客户端）

### 运行方式

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

### 服务端详解

#### 功能

1. 监听 `:8080`，接受 WebSocket 连接
2. 为每个连接注册 `EchoHandler`
3. 收到文本消息后，追加服务端时间戳并写回客户端
4. 每 5 秒打印当前在线连接数
5. 支持 `Ctrl+C` 优雅关闭

#### EchoHandler

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

#### 主程序

```go
func main() {
    cfg := ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    srv, err := server.NewServer(cfg)
    if err != nil {
        log.Fatal(err)
    }

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
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }

    // 优雅关闭
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    srv.Stop()
    log.Println("服务端已停止")
}
```

### 客户端详解

#### 功能

1. 连接 `ws://localhost:8080/`
2. 注册 `PrintHandler`，打印服务端返回的消息
3. 从标准输入读取用户输入，发送到服务端
4. 输入 `exit` 退出
5. 监听连接状态变化

#### PrintHandler

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

#### 主程序

```go
func main() {
    cfg := ws.Config{
        Addr:         "ws://localhost:8080/",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    c, err := client.NewClient(cfg)
    if err != nil {
        log.Fatal(err)
    }

    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

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

### 交互示例

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

## 示例 2：Epoll 模式 Echo（仅 Linux）

在 Linux 上使用 epoll Reactor 模式运行 Echo 服务端，支持更高并发：

```go
package main

import (
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

type EchoHandler struct{}

func (h *EchoHandler) Name() string { return "echo" }

func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    m, ok := msg.(*conn.Message)
    if !ok || m.Type != byte(0x1) {
        ctx.FireChannelRead(msg)
        return
    }
    serverTime := time.Now().Format("2006-01-02 15:04:05.000")
    reply := fmt.Sprintf("服务端时间 %s | 客户端内容: %s", serverTime, string(m.Data))
    ctx.Write(&conn.Message{Type: m.Type, Data: []byte(reply)})
    ctx.FireChannelRead(msg)
}

func (h *EchoHandler) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *EchoHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *EchoHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

func main() {
    // 关键：设置 Mode 为 ws.ModeEpoll
    cfg := ws.Config{
        Mode:             ws.ModeEpoll,
        Addr:             ":8080",
        PingInterval:     30 * time.Second,
        PongTimeout:      60 * time.Second,
        EventLoopWorkers: 0,   // 0 表示自动使用 CPU 核数
    }
    srv, err := server.NewServer(cfg)
    if err != nil {
        log.Fatal(err)
    }

    go func() {
        ticker := time.NewTicker(5 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            if count := srv.Hub().Count(); count > 0 {
                log.Printf("当前在线: %d\n", count)
            }
        }
    }()

    srv.OnConnect(func(sess session.Session) {
        sess.Conn().Pipeline().AddLast("echo", &EchoHandler{})
    })

    log.Println("Epoll 服务端启动，监听 :8080 ...")
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }

    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    srv.Stop()
    log.Println("服务端已停止")
}
```

**Epoll 模式客户端：**

```go
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

type PrintHandler struct{}

func (h *PrintHandler) Name() string { return "print" }

func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    m, ok := msg.(*conn.Message)
    if !ok || m.Type != 0x1 {
        ctx.FireChannelRead(msg)
        return
    }
    fmt.Printf("收到: %s\n", string(m.Data))
    ctx.FireChannelRead(msg)
}

func main() {
    // 关键：设置 Mode 为 ws.ModeEpoll
    cfg := ws.Config{
        Mode:              ws.ModeEpoll,
        Addr:              "ws://localhost:8080/",
        PingInterval:      30 * time.Second,
        PongTimeout:       60 * time.Second,
        ReconnectInterval: 5 * time.Second,
        MaxReconnect:      5,
    }
    c, err := client.NewClient(cfg)
    if err != nil {
        log.Fatal(err)
    }

    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

    c.OnConnect(func(sess session.Session) {
        sess.Conn().Pipeline().AddLast("print", &PrintHandler{})
    })

    go func() {
        sess := c.Session()
        if sess == nil { return }
        for st := range sess.StateChan() {
            switch st {
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

    fmt.Println("已连接到服务端，输入消息并按回车发送（输入 exit 退出）")
    fmt.Print("请输入消息: ")

    scanner := bufio.NewScanner(os.Stdin)
    for scanner.Scan() {
        text := scanner.Text()
        if text == "exit" {
            c.Close()
            return
        }
        c.Session().Conn().Pipeline().FireChannelWrite(
            &conn.Message{Type: 0x1, Data: []byte(text)})
    }
}
```

**注意：** Epoll 模式仅在 Linux 上可用。在非 Linux 平台运行时，`NewServer` 或 `NewClient` 会返回错误。

---

## 示例 3：聊天室（Hub 广播 + userID 绑定）

`demo/chat/` 演示聊天室场景：

- 服务端使用 `UserManager` 绑定业务 userID 到 Session
- 支持广播（一人说话全员可见）和私信（定向发送）
- 客户端自动重连

### 核心设计：UserManager

```go
type UserManager struct {
    mu    sync.RWMutex
    users map[string]session.Session // userID → Session
}

func (um *UserManager) Bind(userID string, sess session.Session) {
    um.mu.Lock()
    um.users[userID] = sess
    um.mu.Unlock()
}

func (um *UserManager) SendTo(userID string, msg conn.Message) {
    um.mu.RLock()
    sess := um.users[userID]
    um.mu.RUnlock()
    if sess != nil {
        sess.Conn().Pipeline().FireChannelWrite(msg)
    }
}
```

---

## 扩展思路

1. **多客户端** — 同时启动多个客户端，观察 Hub.Count() 变化
2. **自定义 Handler** — 在 Pipeline 中添加认证、限流、日志等 Handler
3. **状态机扩展** — 利用 `StateChan` 做断线重连提示、连接质量监控
4. **性能测试** — 使用 `ws/hub` 的 benchmark 测试广播性能
5. **Epoll 压测** — 在 Linux 上使用 `ws.ModeEpoll` 进行高并发压测（10 万+ 连接）

# ws — API 使用说明

## 安装

```bash
go get github.com/lufeijun/goTools/ws
```

## 快速开始

```go
import "github.com/lufeijun/goTools/ws"
```

## 核心类型

```go
// 消息类型
ws.OpcodeText   // 0x1 文本帧
ws.OpcodeBinary // 0x2 二进制帧
ws.OpcodeClose  // 0x8 关闭帧
ws.OpcodePing   // 0x9 心跳帧
ws.OpcodePong   // 0xA 心跳回复帧

// 消息结构
ws.Message{
    Type   ws.Opcode  // 帧类型
    Data   []byte     // 消息内容
    Status uint16     // 仅 Close 帧使用
}

// 连接状态
ws.StateDisconnected  // 已断开
ws.StateConnecting    // 连接中
ws.StateConnected     // 已连接
ws.StateReconnecting  // 重连中
ws.StateClosed        // 已关闭
```

---

## 创建 Server

### 最简 Server

```go
package main

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
)

func main() {
    srv := ws.NewServer(ws.ServerConfig{
        Addr: ":8080",
    })

    // 启动 Hub 事件循环
    go srv.Hub().Run()

    // 处理新连接
    go func() {
        for sess := range srv.ConnChan() {
            go handleSession(sess)
        }
    }()

    log.Println("server listening on :8080")
    log.Fatal(srv.ListenAndServe())
}

func handleSession(sess *ws.Session) {
    for msg := range sess.ReadChan() {
        switch msg.Type {
        case ws.OpcodeText:
            fmt.Printf("text: %s\n", string(msg.Data))
        case ws.OpcodeBinary:
            fmt.Printf("binary: %x\n", msg.Data)
        case ws.OpcodeClose:
            fmt.Println("client closed:", msg.Status)
            return
        }
    }
}
```

### Server 配置项

```go
srv := ws.NewServer(ws.ServerConfig{
    Addr:              ":8080",         // 监听地址
    PingInterval:      30 * time.Second, // 心跳间隔，默认 30s
    PongTimeout:       60 * time.Second, // Pong 超时，默认 60s
    MaxConnections:    10000,            // 最大连接数，0 表示不限制
    HandshakeTimeout:  10 * time.Second, // 握手超时，默认 10s
    ReadBufferSize:    4096,             // 读 buffer 大小，默认 4096
    WriteBufferSize:   4096,             // 写 buffer 大小，默认 4096
})
```

### 优雅关闭

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
srv.Shutdown(ctx)
```

---

## 创建 Client

### 最简 Client

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
)

func main() {
    client := ws.NewClient(ws.ClientConfig{
        URL: "ws://localhost:8080/",
    })

    if err := client.Connect(); err != nil {
        log.Fatal("connect failed:", err)
    }
    defer client.Close()

    // 发送消息
    client.Send(ws.Message{Type: ws.OpcodeText, Data: []byte("hello")})

    // 接收消息
    for msg := range client.ReadChan() {
        if msg.Type == ws.OpcodeText {
            fmt.Println("received:", string(msg.Data))
        }
    }

    fmt.Println("disconnected")
}
```

### Client 配置项

```go
client := ws.NewClient(ws.ClientConfig{
    URL:               "ws://localhost:8080/",
    Headers:           nil,               // 自定义 HTTP 头
    PingInterval:      30 * time.Second,  // 心跳间隔，默认 30s
    PongTimeout:       60 * time.Second,  // Pong 超时，默认 60s
    ReconnectInterval: 5 * time.Second,   // 重连间隔，默认 5s
    MaxReconnect:      5,                 // 最大重连次数，默认 5
})
```

### 监听状态变化

```go
go func() {
    for state := range client.StateChan() {
        fmt.Println("state:", state)
    }
}()
```

### 发送二进制消息

```go
client.Send(ws.Message{Type: ws.OpcodeBinary, Data: []byte{0x01, 0x02, 0x03}})
```

### 使用 WriteChan 直接写入

```go
client.WriteChan() <- ws.Message{Type: ws.OpcodeText, Data: []byte("hello")}
```

---

## Hub — 连接管理

Hub 是内置的连接管理中心，单 goroutine 事件循环，线程安全。

### 注册连接

```go
go func() {
    for sess := range srv.ConnChan() {
        srv.Hub().Register(sess)
    }
}()
```

### 注销连接

```go
srv.Hub().Unregister(connID)
```

### 广播消息

```go
srv.Hub().Broadcast(ws.Message{Type: ws.OpcodeText, Data: []byte("hello everyone")})
```

### 定向发送

```go
srv.Hub().Send(connID, ws.Message{Type: ws.OpcodeText, Data: []byte("hello you")})
```

### 查询连接

```go
// 获取当前连接数
count := srv.Hub().Count()

// 按 ID 查找连接
sess := srv.Hub().Get(connID)
if sess != nil {
    sess.WriteChan() <- ws.Message{Type: ws.OpcodeText, Data: []byte("found you")}
}
```

---

## 完整示例

### Echo Server + Client

**server.go**

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/lufeijun/goTools/ws"
)

func main() {
    srv := ws.NewServer(ws.ServerConfig{
        Addr:         ":8080",
        PingInterval: 30,
        PongTimeout:  60,
    })

    go srv.Hub().Run()

    go func() {
        for sess := range srv.ConnChan() {
            go func(s *ws.Session) {
                for msg := range s.ReadChan() {
                    if msg.Type == ws.OpcodeText || msg.Type == ws.OpcodeBinary {
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
```

**client.go**

```go
package main

import (
    "bufio"
    "fmt"
    "log"
    "os"

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
        client.Send(ws.Message{Type: ws.OpcodeText, Data: []byte(text)})
    }
}
```

### 聊天室 Server

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"

    "github.com/lufeijun/goTools/ws"
)

func main() {
    srv := ws.NewServer(ws.ServerConfig{Addr: ":8080"})
    go srv.Hub().Run()

    // 新连接注册到 Hub，断开时注销
    go func() {
        for sess := range srv.ConnChan() {
            go func(s *ws.Session) {
                id := s.Conn().ID()
                srv.Hub().Register(s)

                // 广播上线通知
                srv.Hub().Broadcast(ws.Message{
                    Type: ws.OpcodeText,
                    Data: []byte(fmt.Sprintf("user %d joined", id)),
                })

                // 转发该用户的消息给所有人
                for msg := range s.ReadChan() {
                    if msg.Type == ws.OpcodeText {
                        srv.Hub().Broadcast(ws.Message{
                            Type: ws.OpcodeText,
                            Data: []byte(fmt.Sprintf("user %d: %s", id, string(msg.Data))),
                        })
                    }
                }

                // 断开后广播下线通知
                srv.Hub().Unregister(id)
                srv.Hub().Broadcast(ws.Message{
                    Type: ws.OpcodeText,
                    Data: []byte(fmt.Sprintf("user %d left", id)),
                })
            }(sess)
        }
    }()

    log.Println("chat room on :8080")
    go srv.ListenAndServe()

    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    ctx, cancel := context.WithTimeout(context.Background(), 5e9)
    defer cancel()
    srv.Shutdown(ctx)
}
```

---

## 注意事项

1. **Hub 必须启动**：调用 `srv.Hub().Run()` 启动事件循环，否则注册/广播等操作会永久阻塞
2. **Hub 注册不是自动的**：新连接通过 `srv.ConnChan()` 传出，需要手动调用 `srv.Hub().Register(sess)` 注册到 Hub
3. **ReadChan 关闭表示断连**：当 `for range sess.ReadChan()` 循环退出时，说明连接已断开
4. **Ping/Pong 自动处理**：`goroutineConn` 收到 Ping 自动回复 Pong，无需手动处理
5. **MaxConnections 是近似限制**：V1 中 MaxConnections 检查和 Hub 注册之间存在时间差，高并发下可能短暂超出限制

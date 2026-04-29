# ws/client — 客户端 API

`ws/client` 提供 WebSocket 客户端的连接管理、握手、心跳、自动重连和 Session 生命周期管理。

---

## 核心接口

```go
type Client interface {
    Config() ws.Config
    Connect() error
    Close() error
    Session() session.Session
}
```

| 方法 | 说明 |
|---|---|
| `Config()` | 获取客户端配置 |
| `Connect()` | 连接服务端（阻塞直到握手完成或失败） |
| `Close()` | 关闭连接 |
| `Session()` | 获取当前会话（Connect 成功后非 nil） |

---

## 默认实现：defaultClient

```go
type defaultClient struct {
    config ws.Config
    sess   session.Session
}
```

---

## 创建客户端

```go
func NewClient(cfg ws.Config) Client
```

**零值处理：** 若 `cfg.ReadBufferSize == 0`，自动填充所有默认配置：

```go
if cfg.ReadBufferSize == 0 {
    defaults := ws.DefaultConfig()
    cfg.ReadBufferSize = defaults.ReadBufferSize
    cfg.WriteBufferSize = defaults.WriteBufferSize
    cfg.PingInterval = defaults.PingInterval
    // ... 其他字段同理
}
```

**示例：**

```go
c := client.NewClient(ws.Config{
    Addr:              "ws://localhost:8080/",
    PingInterval:      30 * time.Second,
    PongTimeout:       60 * time.Second,
    ReconnectInterval: 5 * time.Second,
    MaxReconnect:      3,
})
```

---

## 连接流程

```go
func (c *defaultClient) Connect() error {
    // 1. WebSocket 握手
    nc, err := conn.ClientHandshake(c.config.Addr, c.config.Headers)
    if err != nil {
        return err
    }

    // 2. 创建 netConn
    wc := conn.NewNetConn(nc, true, 1)

    // 3. 创建 Session
    sess := session.NewSession(wc, session.Config{
        PingInterval:      c.config.PingInterval,
        PongTimeout:       c.config.PongTimeout,
        ReconnectInterval: c.config.ReconnectInterval,
        MaxReconnect:      c.config.MaxReconnect,
    })

    // 4. 启动心跳
    hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
    hb.SetOnTimeout(func() {
        sess.SetState(session.StateDisconnected)
        wc.Close()
    })
    sess.SetState(session.StateConnected)
    hb.Start(sess)

    // 5. 添加帧编解码器
    wc.Pipeline().AddLast("codec", &conn.FrameCodec{Writer: nc, IsClient: true})

    // 6. 启动读循环
    go c.serveConn(nc)

    c.sess = sess
    return nil
}
```

### serveConn 读循环

```go
func (c *defaultClient) serveConn(nc net.Conn) {
    sess := c.sess
    if sess == nil {
        return
    }
    defer sess.Close()

    for {
        f, err := frame.ReadFrame(nc)
        if err != nil {
            return
        }

        switch f.Opcode {
        case frame.OpcodeText, frame.OpcodeBinary:
            msg := &conn.Message{Type: byte(f.Opcode), Data: f.Payload}
            sess.Conn().Pipeline().FireChannelRead(msg)

        case frame.OpcodePing:
            _ = frame.WriteFrame(nc, frame.NewPongFrame(f.Payload))

        case frame.OpcodeClose:
            return
        }
    }
}
```

---

## 发送消息

通过 Pipeline 触发 Outbound 链：

```go
sess := c.Session()
p := sess.Conn().Pipeline()
p.FireChannelWrite(&conn.Message{
    Type: 0x1,              // 文本帧
    Data: []byte("hello"),
})
```

`FrameCodec` 会自动将 `*conn.Message` 编码为 WebSocket 帧并发送。

---

## 接收消息

在 Pipeline 上注册 InboundHandler：

```go
type PrintHandler struct{}

func (h *PrintHandler) Name() string { return "print" }

func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*conn.Message); ok && m.Type == 0x1 {
        fmt.Printf("收到: %s\n", string(m.Data))
    }
    ctx.FireChannelRead(msg)
}

// 连接成功后注册
c.Connect()
c.Session().Conn().Pipeline().AddLast("print", &PrintHandler{})
```

---

## 状态监听

```go
c.Connect()
sess := c.Session()

go func() {
    for st := range sess.StateChan() {
        switch st {
        case session.StateConnected:
            fmt.Println("已连接")
        case session.StateDisconnected:
            fmt.Println("已断开")
        case session.StateReconnecting:
            fmt.Println("正在重连...")
        case session.StateClosed:
            fmt.Println("连接已关闭")
            return
        }
    }
}()
```

---

## 使用示例：完整客户端

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
    cfg := ws.Config{
        Addr:         "ws://localhost:8080/",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    c := client.NewClient(cfg)

    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

    c.Session().Conn().Pipeline().AddLast("print", &PrintHandler{})

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

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `client.go` | `Client` 接口、`defaultClient` 实现、Connect/Close/serveConn |
| `client_test.go` | NewClient、配置测试 |

---

## 注意事项

1. **Connect 成功后才可获取 Session** — `c.Session()` 在 Connect 前返回 nil
2. **FrameCodec 自动添加** — 无需手动添加，Connect() 内部已处理
3. **自动重连在 V2.1 完善** — V2.0 断线后不会自动重连，需要业务层检测并重新 Connect
4. **Ping 自动回复 Pong** — 在 serveConn 中处理，不经过 Pipeline
5. **Headers 用于握手时附加 HTTP 头** — 如认证 Token：`cfg.Headers = http.Header{"Authorization": []string{"Bearer xxx"}}`

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
    OnConnect(fn func(session.Session))
}
```

| 方法 | 说明 |
|---|---|
| `Config()` | 获取客户端配置 |
| `Connect()` | 连接服务端（阻塞直到握手完成或失败） |
| `Close()` | 关闭连接 |
| `Session()` | 获取当前会话（Connect 成功后非 nil） |
| `OnConnect(fn)` | 注册连接建立回调 |

---

## 默认实现：defaultClient

```go
type defaultClient struct {
    config    ws.Config
    sess      session.Session
    onConnect func(session.Session)
    mu        sync.Mutex
    closed    bool
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
    c.mu.Lock()
    c.closed = false
    c.mu.Unlock()
    return c.doConnect()
}

func (c *defaultClient) doConnect() error {
    // 1. WebSocket 握手
    nc, err := conn.ClientHandshake(c.config.Addr, c.config.Headers)
    if err != nil {
        return err
    }

    // 2. 设置 TCP 参数
    if err := conn.ApplyTCPOptions(nc, c.config.TCPNoDelay, c.config.TCPQuickAck); err != nil {
        nc.Close()
        return err
    }

    // 3. 创建 netConn
    wc := conn.NewNetConn(nc, true, 1)

    // 4. 创建 Session
    sess := session.NewSession(wc, session.Config{
        PingInterval:      c.config.PingInterval,
        PongTimeout:       c.config.PongTimeout,
        ReconnectInterval: c.config.ReconnectInterval,
        MaxReconnect:      c.config.MaxReconnect,
    })

    // 5. 启动心跳（共享时间轮）
    hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
    hb.SetOnTimeout(func() {
        sess.SetState(session.StateDisconnected)
        wc.Close()
    })
    sess.SetState(session.StateConnected)
    hb.Start(sess)

    // 6. 添加帧编解码器
    wc.Pipeline().AddLast("codec", &conn.FrameCodec{Writer: nc, IsClient: true})

    // 7. 触发连接回调
    go c.serveConn(nc)

    c.sess = sess

    if c.onConnect != nil {
        c.onConnect(sess)
    }
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
    defer func() {
        sess.Close()
        c.maybeReconnect()
    }()

    readTimeout := c.config.PongTimeout * 2
    if readTimeout == 0 {
        readTimeout = 120 * time.Second
    }

    br := bufio.NewReaderSize(nc, 65536)

    for {
        nc.SetReadDeadline(time.Now().Add(readTimeout))
        f, err := frame.ReadFrameLimit(br, c.config.MaxFrameSize)
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
            // RFC 合规：回写 Close 帧
            code := uint16(1000)
            reason := ""
            if len(f.Payload) >= 2 {
                code = binary.BigEndian.Uint16(f.Payload[:2])
                reason = string(f.Payload[2:])
            }
            _ = frame.WriteFrame(nc, frame.NewCloseFrame(code, reason))
            return
        }
    }
}
```

### 自动重连（P2 优化 5.6）

```go
func (c *defaultClient) maybeReconnect() {
    c.mu.Lock()
    if c.closed {
        c.mu.Unlock()
        return
    }
    c.mu.Unlock()

    go func() {
        backoff := c.config.ReconnectInterval
        for i := 0; i < c.config.MaxReconnect; i++ {
            time.Sleep(backoff)

            c.mu.Lock()
            if c.closed {
                c.mu.Unlock()
                return
            }
            c.mu.Unlock()

            if err := c.doConnect(); err == nil {
                return
            }

            // 指数退避：1s → 2s → 4s ... 最大 60s
            if backoff < 60*time.Second {
                backoff *= 2
            }
        }
        // 重连耗尽
        if c.sess != nil {
            c.sess.SetState(session.StateClosed)
        }
    }()
}
```

- 初始间隔 `ReconnectInterval`（默认 5s）
- 每次失败间隔翻倍，最大 60s
- 达到 `MaxReconnect` 后进入 `StateClosed`
- `Close()` 可中断重连过程

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

// 连接成功后注册（或在 OnConnect 回调中注册）
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
        Addr:              "ws://localhost:8080/",
        PingInterval:      30 * time.Second,
        PongTimeout:       60 * time.Second,
        ReconnectInterval: 5 * time.Second,
        MaxReconnect:      5,
    }
    c := client.NewClient(cfg)

    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

    c.OnConnect(func(sess session.Session) {
        sess.Conn().Pipeline().AddLast("print", &PrintHandler{})
    })

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
| `client.go` | `Client` 接口、`defaultClient` 实现、Connect/Close/serveConn/maybeReconnect |
| `client_test.go` | NewClient、配置测试 |

---

## 注意事项

1. **Connect 成功后才可获取 Session** — `c.Session()` 在 Connect 前返回 nil
2. **FrameCodec 自动添加** — 无需手动添加，`doConnect()` 内部已处理
3. **自动重连已实现** — 断线后按指数退避策略自动重连（1s → 2s → 4s ... 最大 60s）
4. **Ping 自动回复 Pong** — 在 serveConn 中处理，不经过 Pipeline
5. **Headers 用于握手时附加 HTTP 头** — 如认证 Token：`cfg.Headers = http.Header{"Authorization": []string{"Bearer xxx"}}`
6. **Close 帧 RFC 合规** — 收到 Close 后回写 Close 响应帧再断开
7. **读 deadline 防止泄漏** — serveConn 带 read deadline，心跳超时兜底

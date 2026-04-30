# ws/server — 服务端 API

`ws/server` 提供 WebSocket 服务端的启动、HTTP Upgrade 处理、Session 生命周期管理和 Hub 集成。

---

## 核心接口

```go
type Server interface {
    Config() ws.Config
    Hub() hub.Hub
    Start() error
    Stop() error
    Listener() net.Listener
    OnConnect(fn func(session.Session))
}
```

| 方法 | 说明 |
|---|---|
| `Config()` | 获取服务端配置 |
| `Hub()` | 获取连接管理中心 |
| `Start()` | 启动服务端（阻塞） |
| `Stop()` | 优雅关闭服务端 |
| `Listener()` | 获取底层 TCP Listener（用于获取实际绑定地址） |
| `OnConnect(fn)` | 注册连接建立回调 |

---

## 默认实现：defaultServer

```go
type defaultServer struct {
    config    ws.Config
    hub       hub.Hub
    listener  net.Listener
    server    *http.Server
    onConnect func(session.Session)
}
```

---

## 创建服务端

```go
func NewServer(cfg ws.Config) Server
```

**零值处理：** 若 `cfg.ReadBufferSize == 0`，自动填充所有默认配置：

```go
if cfg.ReadBufferSize == 0 {
    defaults := ws.DefaultConfig()
    cfg.ReadBufferSize = defaults.ReadBufferSize
    cfg.WriteBufferSize = defaults.WriteBufferSize
    cfg.PingInterval = defaults.PingInterval
    cfg.PongTimeout = defaults.PongTimeout
    cfg.MaxFrameSize = defaults.MaxFrameSize
    // ... 其他字段同理
}
```

**示例：**

```go
srv := server.NewServer(ws.Config{
    Addr:         ":8080",
    PingInterval: 30 * time.Second,
    PongTimeout:  60 * time.Second,
})
```

---

## 启动流程

```go
func (s *defaultServer) Start() error {
    mux := http.NewServeMux()
    mux.HandleFunc("/", s.handleWebSocket)

    s.server = &http.Server{Handler: mux}
    if s.config.SOReusePort {
        s.listener, err = conn.ListenTCPWithReusePort(s.config.Addr)
    } else {
        s.listener, err = net.Listen("tcp", s.config.Addr)
    }
    if err != nil {
        return err
    }
    return s.server.Serve(s.listener)
}
```

1. 创建 `http.ServeMux`，注册 `/` 路径的 WebSocket 处理器
2. 监听 TCP 地址（支持 SO_REUSEPORT）
3. 调用 `http.Server.Serve()` 开始接受连接

---

## 连接处理流程

```go
func (s *defaultServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
    // 1. 连接数限制检查
    if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
        http.Error(w, "Too many connections", http.StatusServiceUnavailable)
        return
    }

    // 2. WebSocket 握手
    nc, err := conn.ServerHandshake(w, r)
    if err != nil {
        return
    }

    // 3. 设置 TCP 参数（NODELAY / QUICKACK）
    if err := conn.ApplyTCPOptions(nc, s.config.TCPNoDelay, s.config.TCPQuickAck); err != nil {
        nc.Close()
        return
    }

    // 4. 创建 netConn
    c := conn.NewNetConn(nc, false, conn.NextConnID())

    // 5. 创建 Session
    sess := session.NewSession(c, session.Config{
        PingInterval: s.config.PingInterval,
        PongTimeout:  s.config.PongTimeout,
    })

    // 6. 启动心跳（共享时间轮）
    hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
    hb.SetOnTimeout(func() {
        sess.SetState(session.StateDisconnected)
        c.Close()
    })
    sess.SetState(session.StateConnected)
    hb.Start(sess)

    // 7. 注册到 Hub
    s.hub.Register(sess)

    // 8. 添加帧编解码器
    sess.Conn().Pipeline().AddLast("codec", &conn.FrameCodec{Writer: nc, IsClient: false})

    // 9. 触发连接回调
    if s.onConnect != nil {
        s.onConnect(sess)
    }

    // 10. 启动读循环
    go s.serveConn(sess, nc)
}
```

### serveConn 读循环（P1 优化 3.3）

```go
func (s *defaultServer) serveConn(sess session.Session, nc net.Conn) {
    defer func() {
        sess.Close()
        s.hub.Unregister(sess.Conn().ID())
    }()

    readTimeout := s.config.PongTimeout * 2
    if readTimeout == 0 {
        readTimeout = 120 * time.Second
    }

    // 64KB bufio 批量预读，减少 syscall 次数
    br := bufio.NewReaderSize(nc, 65536)

    for {
        nc.SetReadDeadline(time.Now().Add(readTimeout))
        f, err := frame.ReadFrameLimit(br, s.config.MaxFrameSize)
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
            // RFC 合规：回写 Close 帧再断开
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

**职责：**

- 使用 `bufio.Reader`（64KB）批量预读，帧解析从 3-5 次 syscall 降至 1 次
- `ReadFrameLimit` 带 MaxFrameSize 校验，防止 DoS
- Text/Binary 帧 → 构造 `conn.Message` → 触发 Pipeline 的 `FireChannelRead`
- Ping 帧 → 自动回复 Pong 帧
- Close 帧 → **回写 Close 响应帧**（RFC 6455 合规），然后退出循环清理资源
- 读 deadline 防止 TCP 半开连接永不返回

---

## OnConnect 回调

用于在连接建立后注册业务 Handler：

```go
srv.OnConnect(func(sess session.Session) {
    sess.Conn().Pipeline().AddLast("echo", &EchoHandler{})
})
```

- 每个新连接都会触发此回调
- 在回调中添加的 Handler 只对当前连接生效
- 此时 FrameCodec 已添加到 Pipeline，业务 Handler 能看到已解码的 `conn.Message`

---

## 停止服务

```go
func (s *defaultServer) Stop() error {
    if s.server != nil {
        return s.server.Shutdown(context.Background())
    }
    return nil
}
```

- 使用 `http.Server.Shutdown` 优雅关闭
- 等待现有连接处理完成后退出

---

## 使用示例：完整 Echo 服务端

```go
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
    cfg := ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    srv := server.NewServer(cfg)

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

    log.Println("服务端启动，监听 :8080 ...")
    go func() {
        if err := srv.Start(); err != nil {
            log.Fatal(err)
        }
    }()

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

## 文件清单

| 文件 | 内容 |
|---|---|
| `server.go` | `Server` 接口、`defaultServer` 实现、Start/Stop/handleWebSocket/serveConn |
| `server_test.go` | NewServer、Start/Stop 测试 |

---

## 注意事项

1. **Start() 阻塞当前 goroutine** — 需要在独立 goroutine 中调用
2. **OnConnect 在帧编解码器之后调用** — 确保业务 Handler 能看到已解码的 `conn.Message`
3. **Hub 自动注册，注销在 serveConn defer 中** — 连接断开时自动从 Hub 移除
4. **Ping 自动回复 Pong** — 在 serveConn 中处理，不经过 Pipeline
5. **MaxConnections 为 0 表示不限制** — 开启限制后，超限连接直接返回 503
6. **TCP 参数默认生效** — `TCPNoDelay=true` 关闭 Nagle，`TCPQuickAck` 和 `SOReusePort` 按需开启
7. **serveConn 使用 bufio 预读** — 64KB 缓冲，减少帧解析 syscall 次数

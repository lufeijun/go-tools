# ws/client — 客户端 API

`ws/client` 提供 WebSocket 客户端的连接管理、握手、心跳、自动重连和 Session 生命周期管理。

v2 引入了 **双模式连接**，支持两种 I/O 模式：
- **ModeNet**（默认）：传统的 `net.Dial` + `ClientHandshake` 方式，每个连接一个 goroutine，所有平台可用
- **ModeEpoll**（Linux 专属）：基于 epoll 的 Reactor 模型，使用 `DialNonBlock` + `ClientHandshakeFD`，支持事件驱动 I/O

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
    elg       eventloop.EventLoopGroup   // Epoll 模式下的 Sub Reactor 组
}
```

**与旧版的区别：** 新增了 `elg` 字段（EventLoopGroup），用于 Epoll 模式下的事件驱动。`closed` 字段用于控制重连行为。

---

## 创建客户端

```go
func NewClient(cfg ws.Config) (Client, error)
```

**返回 error 的原因：** `NewClient` 会调用 `cfg.ValidateMode()` 进行模式校验。如果在非 Linux 平台上使用 `ModeEpoll`，会直接返回错误。

**零值处理：** 若 `cfg.ReadBufferSize == 0`，自动填充所有默认配置：

```go
if cfg.ReadBufferSize == 0 {
    defaults := ws.DefaultConfig()
    cfg.ReadBufferSize = defaults.ReadBufferSize
    cfg.WriteBufferSize = defaults.WriteBufferSize
    cfg.PingInterval = defaults.PingInterval
    cfg.PongTimeout = defaults.PongTimeout
    cfg.MaxFrameSize = defaults.MaxFrameSize
    cfg.EventLoopWorkers = defaults.EventLoopWorkers
    cfg.EventLoopStrategy = defaults.EventLoopStrategy
    cfg.BufferPoolSmall = defaults.BufferPoolSmall
    cfg.BufferPoolDefault = defaults.BufferPoolDefault
    cfg.BufferPoolLarge = defaults.BufferPoolLarge
    cfg.ReconnectInterval = defaults.ReconnectInterval
    cfg.MaxReconnect = defaults.MaxReconnect
}
```

**Epoll 模式下额外初始化：**

```go
c := &defaultClient{config: cfg}
if cfg.Mode == ws.ModeEpoll {
    c.elg = newEventLoopGroupForClient(cfg)
}
return c, nil
```

在 Linux 上，`newEventLoopGroupForClient` 创建真实的 EventLoopGroup。在非 Linux 上，它返回 nil（但 `ValidateMode()` 已确保不会走到这里）。

**示例：**

```go
c, err := client.NewClient(ws.Config{
    Addr:              "ws://localhost:8080/",
    PingInterval:      30 * time.Second,
    PongTimeout:       60 * time.Second,
    ReconnectInterval: 5 * time.Second,
    MaxReconnect:      3,
})
if err != nil {
    log.Fatal(err)
}
```

**Epoll 模式示例（仅 Linux）：**

```go
c, err := client.NewClient(ws.Config{
    Mode:              ws.ModeEpoll,
    Addr:              "ws://localhost:8080/",
    PingInterval:      30 * time.Second,
    PongTimeout:       60 * time.Second,
    ReconnectInterval: 5 * time.Second,
    MaxReconnect:      3,
})
if err != nil {
    log.Fatal(err)
}
```

---

## 双模式连接

### Connect() 入口

```go
func (c *defaultClient) Connect() error {
    c.mu.Lock()
    c.closed = false
    c.mu.Unlock()

    // Epoll 模式：启动 EventLoopGroup
    if c.elg != nil {
        if err := c.elg.Start(); err != nil {
            return err
        }
    }

    return c.doConnect()
}
```

**流程：**

1. 重置 `closed` 标志（允许重连）
2. 如果是 Epoll 模式，先启动 EventLoopGroup（Sub Reactor 组）
3. 调用 `doConnect()` 执行实际的连接逻辑

### doConnect() — 模式分发

```go
func (c *defaultClient) doConnect() error {
    switch c.config.Mode {
    case ws.ModeEpoll:
        return c.doConnectEpoll()
    default:
        return c.doConnectNet()
    }
}
```

根据 `config.Mode` 分发到不同的连接方法。

### doConnectNet() — 标准模式连接

```go
func (c *defaultClient) doConnectNet() error {
    // 1. WebSocket 握手（使用 Go 标准库 net.Dial）
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

    // 4. 初始化 Session
    sess := c.initSession(wc)

    // 5. 启动读循环
    go c.serveConnNet(nc, sess)

    c.sess = sess
    if c.onConnect != nil {
        c.onConnect(sess)
    }
    return nil
}
```

**流程详解：**

1. `conn.ClientHandshake(addr, headers)` — 使用 `net.Dial` 建立 TCP 连接，发送 HTTP Upgrade 请求，完成 WebSocket 握手
2. `conn.ApplyTCPOptions(nc, ...)` — 在 `net.Conn` 上设置 TCP_NODELAY / TCP_QUICKACK
3. `conn.NewNetConn(nc, true, 1)` — 将 `net.Conn` 包装为 `netConn`（`true` 表示客户端，`1` 是客户端的固定 connID）
4. `initSession(wc)` — 初始化 Session（详见下文）
5. `go c.serveConnNet(nc, sess)` — 在新 goroutine 中启动 bufio 读循环

### doConnectEpoll() — Epoll 模式连接（仅 Linux）

此函数定义在 `epoll_linux.go` 中，仅在 Linux 平台编译：

```go
func (c *defaultClient) doConnectEpoll() error {
    // 1. 从 ws:// URL 中提取 host:port
    u, host := parseWSAddr(c.config.Addr)
    if host == "" {
        return fmt.Errorf("invalid addr: %s", c.config.Addr)
    }

    // 2. 非阻塞连接
    fd, err := conn.DialNonBlock(host)
    if err != nil {
        return fmt.Errorf("dial non-block: %w", err)
    }

    // 3. 等待连接完成
    if err := waitForConnect(fd); err != nil {
        unix.Close(fd)
        return fmt.Errorf("connect wait: %w", err)
    }

    // 4. 在原始 fd 上完成 WebSocket 握手
    if err := conn.ClientHandshakeFD(fd, u, c.config.Headers); err != nil {
        unix.Close(fd)
        return fmt.Errorf("client handshake fd: %w", err)
    }

    // 5. 设置 TCP 选项
    if c.config.TCPNoDelay {
        unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
    }
    if c.config.TCPQuickAck {
        unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
    }

    // 6. 创建 epollConn
    ec := conn.NewEpollConn(fd, true, conn.NextConnID())
    if c.config.MaxFrameSize > 0 {
        ec.SetMaxFrameSize(c.config.MaxFrameSize)
    }

    // 7. 绑定到 Sub EventLoop
    el := c.elg.Next()
    ec.SetEventLoop(el)
    el.Register(fd, &conn.EventHandlerAdapter{Conn: ec})

    // 8. 初始化 Session
    sess := c.initSession(ec)

    // 9. 启动等待 goroutine
    go c.serveConnEpoll(sess)

    c.sess = sess
    if c.onConnect != nil {
        c.onConnect(sess)
    }
    return nil
}
```

**流程详解：**

1. `parseWSAddr("ws://localhost:8080/")` — 从 WebSocket URL 中提取 `host:port`（`localhost:8080`）和完整 URL（用于握手）
2. `conn.DialNonBlock(host)` — 创建非阻塞 TCP 套接字并发起连接（`unix.Socket` + `unix.Connect`，非阻塞模式下立即返回）
3. `waitForConnect(fd)` — 使用临时 epoll 实例等待连接完成
   - 创建临时 `EpollPoller`
   - 注册 fd 的写事件（连接完成时 fd 变为可写）
   - `poller.Wait(-1)` 阻塞等待
   - 通过 `unix.GetsockoptInt(fd, SOL_SOCKET, SO_ERROR)` 检查连接是否成功
4. `conn.ClientHandshakeFD(fd, u, headers)` — 在原始 fd 上完成 WebSocket 握手（发送 HTTP Upgrade 请求并读取响应）
5. `conn.NewEpollConn(fd, true, conn.NextConnID())` — 将 fd 包装为 `epollConn`
6. 通过 `elg.Next()` 轮询选择一个 Sub EventLoop，将 `epollConn` 注册到该 EventLoop
7. `initSession(ec)` — 初始化 Session
8. `go c.serveConnEpoll(sess)` — 启动等待 goroutine（不主动读帧）

**非 Linux 平台的存根**（`epoll_nonlinux.go`）：

```go
func (c *defaultClient) doConnectEpoll() error {
    return errors.New("epoll mode is only supported on Linux")
}

func newEventLoopGroupForClient(cfg ws.Config) eventloop.EventLoopGroup {
    return nil
}
```

在非 Linux 平台上，`doConnectEpoll` 始终返回错误。但 `ValidateMode()` 已在 `NewClient` 中阻止了这种情况。

### 两种模式对比

| 方面 | doConnectNet | doConnectEpoll |
|---|---|---|
| TCP 连接 | `conn.ClientHandshake`（内部 net.Dial，阻塞） | `DialNonBlock` + `waitForConnect`（非阻塞 + epoll 等待） |
| WebSocket 握手 | `conn.ClientHandshake`（在 net.Conn 上） | `conn.ClientHandshakeFD`（在原始 fd 上） |
| TCP 选项 | `conn.ApplyTCPOptions(nc, ...)` | `unix.SetsockoptInt` 直接操作 fd |
| 连接对象 | `conn.NewNetConn(nc, true, 1)` | `conn.NewEpollConn(fd, true, conn.NextConnID())` |
| EventLoop 绑定 | 无 | `ec.SetEventLoop(el)` + `el.Register(fd, handler)` |
| 读循环 | `go c.serveConnNet(nc, sess)`（bufio 读循环） | `go c.serveConnEpoll(sess)`（等待 StateChan） |

---

## initSession — 初始化会话

```go
func (c *defaultClient) initSession(cn conn.Conn) session.Session {
    // 1. 创建 Session
    sess := session.NewSession(cn, session.Config{
        PingInterval:      c.config.PingInterval,
        PongTimeout:       c.config.PongTimeout,
        ReconnectInterval: c.config.ReconnectInterval,
        MaxReconnect:      c.config.MaxReconnect,
    })

    // 2. 启动心跳
    hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
    hb.SetOnTimeout(func() {
        sess.SetState(session.StateDisconnected)
        cn.Close()
    })
    sess.SetState(session.StateConnected)
    hb.Start(sess)

    // 3. 添加 Pipeline Handler
    //    ConnWriter: 负责实际的写操作（Head 位置，Outbound 链的最后一站）
    //    FrameCodec: 负责将 conn.Message 编码为 WebSocket 帧（Tail 位置）
    cn.Pipeline().AddFirst("headWriter", &conn.ConnWriter{Conn: cn})
    cn.Pipeline().AddLast("codec", &conn.FrameCodec{IsClient: true})

    // 4. Epoll 模式：设置帧回调
    if edc, ok := cn.(conn.EventDrivenConn); ok {
        c.setupEpollFrameCallback(edc, sess)
    }

    return sess
}
```

**Pipeline 结构（从 Head 到 Tail）：**

```
Head → ConnWriter → FrameCodec → [用户 Handler] → Tail
```

**关键点：**

- `ConnWriter` 放在 Head 位置（Outbound 链的终点），负责将编码后的帧数据写入底层连接
- `FrameCodec` 放在 Tail 位置，负责 `conn.Message` ↔ WebSocket 帧的编解码
- Epoll 模式下，`cn` 是 `epollConn`，实现了 `EventDrivenConn` 接口，因此会设置帧回调
- Net 模式下，`cn` 是 `netConn`，不实现 `EventDrivenConn`，跳过帧回调设置

### setupEpollFrameCallback — Epoll 模式帧回调

```go
func (c *defaultClient) setupEpollFrameCallback(edc conn.EventDrivenConn, sess session.Session) {
    edc.SetOnFrame(func(f frame.Frame) {
        switch f.Opcode {
        case frame.OpcodeText, frame.OpcodeBinary:
            msg := &conn.Message{Type: byte(f.Opcode), Data: f.Payload}
            sess.Conn().Pipeline().FireChannelRead(msg)

        case frame.OpcodePing:
            // 通过 Pipeline 写回 Pong
            pongMsg := &conn.Message{Type: byte(frame.OpcodePong), Data: f.Payload}
            sess.Conn().Pipeline().FireChannelWrite(pongMsg)

        case frame.OpcodeClose:
            code := uint16(1000)
            reason := ""
            if len(f.Payload) >= 2 {
                code = binary.BigEndian.Uint16(f.Payload[:2])
                reason = string(f.Payload[2:])
            }
            // 通过 Pipeline 写回 Close 帧
            closeMsg := &conn.Message{
                Type: byte(frame.OpcodeClose),
                Status: code,
                Data: []byte(reason),
            }
            sess.Conn().Pipeline().FireChannelWrite(closeMsg)
            sess.Conn().Close()
        }
    })
}
```

**与服务端的区别：** 客户端的 `setupEpollFrameCallback` 没有设置 `SetOnClose`（服务端设置了），而是通过 `sess.Conn().Close()` 在收到 Close 帧后主动关闭连接。

---

## serveConnNet — Net 模式读循环

```go
func (c *defaultClient) serveConnNet(nc net.Conn, sess session.Session) {
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

- 使用 `bufio.Reader`（64KB）批量预读，减少 syscall 次数
- `ReadFrameLimit` 带 `MaxFrameSize` 校验，防止 DoS
- Text/Binary 帧 → 构造 `conn.Message` → 触发 Pipeline 的 `FireChannelRead`
- Ping 帧 → 直接通过 `frame.WriteFrame` 回写 Pong 帧
- Close 帧 → 回写 Close 响应帧（RFC 6455 合规），然后退出循环
- 退出后调用 `sess.Close()` 和 `c.maybeReconnect()` 尝试重连

---

## serveConnEpoll — Epoll 模式等待

```go
func (c *defaultClient) serveConnEpoll(sess session.Session) {
    defer func() {
        sess.Close()
        c.maybeReconnect()
    }()
    for st := range sess.StateChan() {
        if st == session.StateClosed || st == session.StateDisconnected {
            return
        }
    }
}
```

**与 Net 模式的区别：**

- 不主动读取帧 — 帧的读取和解析由 EventLoop 驱动，通过 `onFrame` 回调处理
- 只监听 `StateChan` — 当连接关闭或断开时退出
- 退出后同样调用 `sess.Close()` 和 `c.maybeReconnect()`

---

## 完整数据流详解

### Net 模式数据流（初学者必读）

```
1. c.Connect() 被调用
   ↓
2. doConnectNet()
   ├── conn.ClientHandshake(addr, headers)
   │   └── net.Dial → 发送 HTTP Upgrade → 读取 101 响应 → 返回 net.Conn
   ├── conn.ApplyTCPOptions(nc, ...) — 设置 TCP 参数
   ├── conn.NewNetConn(nc, true, 1) — 创建 netConn
   ├── initSession(wc)
   │   ├── 创建 Session
   │   ├── 启动心跳
   │   ├── 添加 ConnWriter + FrameCodec
   │   └── （netConn 不实现 EventDrivenConn，跳过帧回调）
   └── go c.serveConnNet(nc, sess) — 启动 bufio 读循环
   ↓
3. serveConnNet 读循环
   ├── 收到 Text/Binary 帧 → Pipeline.FireChannelRead(msg) → 用户 Handler 处理
   ├── 收到 Ping 帧 → 直接回写 Pong
   └── 收到 Close 帧 → 回写 Close 帧后退出
   ↓
4. 用户通过 Pipeline.FireChannelWrite() 发送消息
   → FrameCodec 编码为 WebSocket 帧
   → ConnWriter 写入 net.Conn
   ↓
5. 读循环退出 → sess.Close() → maybeReconnect()
```

### Epoll 模式数据流（初学者必读）

```
1. c.Connect() 被调用
   ├── c.elg.Start() — 启动 EventLoopGroup
   └── doConnect() → doConnectEpoll()
       ↓
2. doConnectEpoll()
   ├── parseWSAddr(addr) — 从 URL 提取 host:port
   ├── conn.DialNonBlock(host) — 创建非阻塞 fd，发起连接
   ├── waitForConnect(fd) — 使用临时 epoll 等待连接完成
   ├── conn.ClientHandshakeFD(fd, u, headers) — 在 fd 上完成 WebSocket 握手
   ├── 设置 TCP 选项（SetsockoptInt）
   ├── conn.NewEpollConn(fd, true, ...) — 创建 epollConn
   ├── elg.Next() 选择 Sub EventLoop
   ├── ec.SetEventLoop(el) + el.Register(fd, handler) — 注册到 Sub Reactor
   ├── initSession(ec)
   │   ├── 创建 Session
   │   ├── 启动心跳
   │   ├── 添加 ConnWriter + FrameCodec
   │   └── setupEpollFrameCallback — 设置 onFrame 回调
   └── go c.serveConnEpoll(sess) — 等待 StateChan
   ↓
3. Sub EventLoop 检测到 fd 可读
   → epollConn 通过 IncrementalParser 解析帧
   → onFrame 回调触发
   ├── Text/Binary → Pipeline.FireChannelRead(msg) → 用户 Handler 处理
   ├── Ping → Pipeline.FireChannelWrite(pongMsg) → ConnWriter 写回
   └── Close → Pipeline.FireChannelWrite(closeMsg) → ConnWriter 写回 → c.Close()
   ↓
4. 用户通过 Pipeline.FireChannelWrite() 发送消息
   → FrameCodec 编码为 WebSocket 帧
   → ConnWriter 写入（通过 epollConn 的 Write 方法）
   ↓
5. 连接断开 → StateChan 收到信号 → serveConnEpoll 退出 → maybeReconnect()
```

---

## 自动重连

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
- `Close()` 可中断重连过程（通过 `c.closed = true`）
- `doConnect()` 会根据模式自动选择 `doConnectNet()` 或 `doConnectEpoll()`

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

`FrameCodec` 会自动将 `*conn.Message` 编码为 WebSocket 帧，`ConnWriter` 负责实际的写入操作。

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
c, err := client.NewClient(cfg)
if err != nil {
    log.Fatal(err)
}
c.Connect()
c.Session().Conn().Pipeline().AddLast("print", &PrintHandler{})
```

---

## 状态监听

```go
c, err := client.NewClient(cfg)
if err != nil {
    log.Fatal(err)
}
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

## 关闭连接

```go
func (c *defaultClient) Close() error {
    c.mu.Lock()
    c.closed = true
    c.mu.Unlock()

    // Epoll 模式：停止 EventLoopGroup
    if c.elg != nil {
        c.elg.Stop()
    }

    if c.sess != nil {
        return c.sess.Close()
    }
    return nil
}
```

**关闭流程：**

1. 设置 `closed = true`，阻止 `maybeReconnect` 发起重连
2. 如果是 Epoll 模式，停止 EventLoopGroup（所有 Sub Reactor 停止轮询）
3. 关闭 Session（关闭底层连接）

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
    cfg := ws.Config{
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

    // 状态监听
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
| `client.go` | `Client` 接口、`defaultClient` 实现、`NewClient`（含 ValidateMode）、`Connect`/`doConnect`/`doConnectNet`/`initSession`/`setupEpollFrameCallback`/`serveConnNet`/`serveConnEpoll`/`maybeReconnect`/`Close` |
| `client_test.go` | `NewClient`（含错误返回）、配置测试 |
| `epoll_linux.go` | `doConnectEpoll` 实现（parseWSAddr / DialNonBlock / waitForConnect / ClientHandshakeFD / NewEpollConn / SetEventLoop / Register），`newEventLoopGroupForClient`，仅 Linux 编译 |
| `epoll_nonlinux.go` | `doConnectEpoll` 存根（返回错误），`newEventLoopGroupForClient` 存根（返回 nil），非 Linux 编译 |

---

## 注意事项

1. **NewClient 返回 error** — 在非 Linux 平台使用 `ModeEpoll` 会返回错误，务必检查
2. **Connect 成功后才可获取 Session** — `c.Session()` 在 Connect 前返回 nil
3. **FrameCodec 和 ConnWriter 自动添加** — 无需手动添加，`initSession()` 内部已处理
4. **自动重连已实现** — 断线后按指数退避策略自动重连（1s → 2s → 4s ... 最大 60s）
5. **Ping 处理方式因模式而异** — Net 模式直接 `frame.WriteFrame`，Epoll 模式走 `Pipeline.FireChannelWrite`
6. **Headers 用于握手时附加 HTTP 头** — 如认证 Token：`cfg.Headers = http.Header{"Authorization": []string{"Bearer xxx"}}`
7. **Close 帧 RFC 合规** — 收到 Close 后回写 Close 响应帧再断开
8. **读 deadline 防止泄漏** — serveConnNet 带 read deadline，心跳超时兜底
9. **Epoll 模式下 serveConnEpoll goroutine 仍存在** — 但它只等待 StateChan，不主动读帧，开销极低
10. **Close() 会停止 EventLoopGroup** — Epoll 模式下关闭客户端时，所有 Sub Reactor 都会停止

# ws/server — 服务端 API

`ws/server` 提供 WebSocket 服务端的启动、连接接收、Session 生命周期管理和 Hub 集成。

v2 引入了 **Acceptor 模式**，支持两种 I/O 模式：
- **ModeNet**（默认）：传统的 `net/http` + Hijack 方式，每个连接一个 goroutine，所有平台可用
- **ModeEpoll**（Linux 专属）：基于 epoll 的 Reactor 模型，使用原始 fd 进行握手和 I/O，目标是 10 万–100 万并发连接

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
| `Start()` | 启动服务端（非阻塞，内部启动 acceptLoop） |
| `Stop()` | 优雅关闭服务端 |
| `Listener()` | 获取底层 TCP Listener（仅 ModeNet 下有效，epoll 模式返回 nil） |
| `OnConnect(fn)` | 注册连接建立回调 |

---

## 默认实现：defaultServer

```go
type defaultServer struct {
    config    ws.Config
    hub       hub.Hub
    acceptor  Acceptor          // 替代了旧版的 listener + server 字段
    onConnect func(session.Session)
    wg        sync.WaitGroup    // 跟踪所有 serveConn goroutine
}
```

**关键变化：** 旧版的 `listener net.Listener` 和 `server *http.Server` 字段已被 `acceptor Acceptor` 替代。Acceptor 是一个抽象接口，封装了"监听 + 接收连接"的逻辑，Net 模式和 Epoll 模式各有不同的实现。

---

## 创建服务端

```go
func NewServer(cfg ws.Config) (Server, error)
```

**返回 error 的原因：** `NewServer` 会调用 `cfg.ValidateMode()` 进行模式校验。如果在非 Linux 平台上使用 `ModeEpoll`，会直接返回错误。

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

**示例：**

```go
srv, err := server.NewServer(ws.Config{
    Addr:         ":8080",
    PingInterval: 30 * time.Second,
    PongTimeout:  60 * time.Second,
})
if err != nil {
    log.Fatal(err)
}
```

**Epoll 模式示例（仅 Linux）：**

```go
srv, err := server.NewServer(ws.Config{
    Mode:         ws.ModeEpoll,
    Addr:         ":8080",
    PingInterval: 30 * time.Second,
    PongTimeout:  60 * time.Second,
})
if err != nil {
    log.Fatal(err)
}
```

---

## Acceptor 模式

Acceptor 是 v2 新引入的抽象层，将"如何监听和接收连接"从 Server 逻辑中解耦出来。

### Acceptor 接口

```go
type Acceptor interface {
    Listen(addr string) error
    Accept() (conn.Conn, net.Conn, error)   // net.Conn 在 epoll 模式下为 nil
    Close() error
}
```

| 方法 | 说明 |
|---|---|
| `Listen(addr)` | 在指定地址上开始监听 |
| `Accept()` | 阻塞等待新连接；返回 `conn.Conn`（框架层连接）和 `net.Conn`（Net 模式下的底层连接，epoll 模式返回 nil） |
| `Close()` | 关闭监听，停止接收新连接 |

**为什么 `Accept()` 返回两个连接对象？**

- **Net 模式**：`conn.Conn` 是 `netConn`，`net.Conn` 是底层的 TCP 连接。`serveConn` 需要 `net.Conn` 来做 bufio 读循环。
- **Epoll 模式**：`conn.Conn` 是 `epollConn`，`net.Conn` 为 `nil`。epoll 模式下数据由 EventLoop 驱动，不需要 `net.Conn`。`serveConn` 通过判断 `nc == nil` 来区分两种模式。

### netAcceptor — 标准模式

`netAcceptor` 使用 Go 标准库的 `net/http` + Hijack 实现 WebSocket 升级：

```go
type netAcceptor struct {
    config   ws.Config
    server   *http.Server
    listener net.Listener
    connCh   chan *acceptedConn   // 缓冲通道，容量 128
    once     sync.Once
}
```

**工作流程：**

1. `Listen(addr)` 创建 `http.Server`，注册 `/` 路径的 `handleUpgrade` 处理器
2. 监听 TCP 地址（支持 `SO_REUSEPORT`）
3. 在后台 goroutine 启动 `http.Server.Serve(listener)`
4. 当客户端发起 WebSocket 升级请求时，`handleUpgrade` 执行：
   - `conn.ServerHandshake(w, r)` — HTTP 握手 + Hijack
   - `conn.ApplyTCPOptions(nc, ...)` — 设置 TCP_NODELAY / TCP_QUICKACK
   - `conn.NewNetConn(nc, false, conn.NextConnID())` — 创建 netConn
   - 将连接放入 `connCh` 缓冲通道
5. `Accept()` 从 `connCh` 中取出连接返回给调用方

**为什么用 `connCh` 而不是直接回调？**

因为 `handleUpgrade` 在 `http.Server` 的 goroutine 中运行，而 `Accept()` 在 `acceptLoop` 中调用。`connCh` 作为两者之间的桥梁，解耦了 HTTP 层和 Server 层。128 的缓冲容量可以应对短时间内的大量并发连接。

### epollAcceptor — Epoll 模式（仅 Linux）

`epollAcceptor` 使用原始 fd + epoll 实现完整的 Reactor 模型：

```go
type epollAcceptor struct {
    config    ws.Config
    listenFd  int                       // 监听套接字的原始 fd
    addr      net.TCPAddr               // 实际绑定地址
    connCh    chan *acceptedConn         // 缓冲通道，容量 128
    elg       eventloop.EventLoopGroup   // Sub Reactor 组
    mainLoop  eventloop.EventLoop        // Main Reactor（负责 accept）
    running   int32                      // 原子标志，控制 accept 循环
    closeOnce sync.Once
}
```

**工作流程：**

1. `Listen(addr)` 执行：
   - `unix.Socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0)` — 创建非阻塞套接字
   - `unix.Bind` + `unix.Listen` — 绑定地址并开始监听
   - 创建 `EventLoopGroup`（Sub Reactor 组，数量 = `EventLoopWorkers`，默认 CPU 核数）
   - 启动 EventLoopGroup
   - 创建 `mainLoop`（Main Reactor），注册 `listenFd` 和 `acceptHandler`
   - 在后台 goroutine 运行 `mainLoop.Run()`

2. 当 `listenFd` 可读时（有新连接），`acceptHandler.OnEvent` 执行：
   - 循环调用 `unix.Accept(fd)` 接收所有等待的连接（非阻塞，直到返回 `EAGAIN`）
   - 对每个新 fd 调用 `handleNewConn(clientFd)`

3. `handleNewConn(clientFd)` 执行：
   - `conn.ServerHandshakeFD(clientFd)` — 在原始 fd 上完成 HTTP 握手
   - 设置 TCP 选项（TCP_NODELAY / TCP_QUICKACK）
   - `conn.NewEpollConn(handshakeFd, false, conn.NextConnID())` — 创建 epollConn
   - 设置 `MaxFrameSize`
   - 通过 `elg.Next()` 选择一个 Sub EventLoop（round-robin）
   - `ec.SetEventLoop(el)` — 绑定 EventLoop
   - `el.Register(handshakeFd, &conn.EventHandlerAdapter{Conn: ec})` — 注册到 Sub Reactor
   - 将连接放入 `connCh`

4. `Accept()` 从 `connCh` 中取出连接返回给调用方（与 netAcceptor 相同）

**Reactor 模型图解：**

```
                    Main Reactor                    Sub Reactor 组
                  ┌──────────────┐              ┌─────────────────────┐
  客户端连接 ────→│  listenFd    │              │  EventLoop[0]       │
                  │  acceptHandler│──handleNewConn→│  epollConn A     │
                  │  (epoll_wait) │              │  epollConn B       │
                  └──────────────┘              ├─────────────────────┤
                                                │  EventLoop[1]       │
                                                │  epollConn C        │
                                                │  epollConn D        │
                                                ├─────────────────────┤
                                                │  ...                │
                                                └─────────────────────┘
```

### newAcceptorForMode — 构建工厂

`newAcceptorForMode` 是根据构建标签（build tag）选择的工厂函数：

**Linux 平台**（`acceptor_factory_linux.go`）：

```go
func newAcceptorForMode(cfg ws.Config) Acceptor {
    switch cfg.Mode {
    case ws.ModeEpoll:
        return newEpollAcceptor(cfg)
    default:
        return newNetAcceptor(cfg)
    }
}
```

**非 Linux 平台**（`acceptor_factory_nonlinux.go`）：

```go
func newAcceptorForMode(cfg ws.Config) Acceptor {
    return newNetAcceptor(cfg)
}
```

**设计要点：**
- Linux 平台根据 `cfg.Mode` 选择 netAcceptor 或 epollAcceptor
- 非 Linux 平台始终使用 netAcceptor（`ValidateMode()` 已在 `NewServer` 中确保不会在非 Linux 上使用 `ModeEpoll`）
- 使用 Go 的 build tag 机制，epoll 相关代码只在 Linux 上编译

---

## 启动流程

```go
func (s *defaultServer) Start() error {
    if err := s.acceptor.Listen(s.config.Addr); err != nil {
        return err
    }
    go s.acceptLoop()
    return nil
}
```

**流程详解：**

1. 调用 `s.acceptor.Listen(s.config.Addr)` — 根据模式执行不同的监听逻辑
   - **Net 模式**：创建 HTTP Server，开始监听 TCP，后台启动 `http.Server.Serve()`
   - **Epoll 模式**：创建原始 fd，绑定并监听，创建 EventLoopGroup 和 Main Reactor，后台运行 `mainLoop.Run()`
2. 启动 `acceptLoop()` goroutine — 循环调用 `s.acceptor.Accept()` 接收新连接

**与旧版的区别：** 旧版 `Start()` 是阻塞的（调用 `http.Server.Serve()`），新版 `Start()` 是非阻塞的，立即返回。连接接收在 `acceptLoop` goroutine 中进行。

### acceptLoop

```go
func (s *defaultServer) acceptLoop() {
    for {
        c, nc, err := s.acceptor.Accept()
        if err != nil {
            return   // Acceptor 关闭后返回
        }

        // 连接数限制检查
        if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
            c.Close()
            continue
        }

        sess := s.initSession(c, nc)
        s.wg.Add(1)
        go s.serveConn(sess, nc)
    }
}
```

**关键点：**

- `Accept()` 返回的 `c` 是 `conn.Conn`（netConn 或 epollConn），`nc` 是 `net.Conn`（epoll 模式下为 nil）
- 连接数超限时直接关闭新连接，不进入会话处理
- 每个新连接都会启动一个 goroutine 来执行 `serveConn`

---

## 连接处理流程

### initSession — 初始化会话

```go
func (s *defaultServer) initSession(c conn.Conn, nc net.Conn) session.Session {
    // 1. 创建 Session
    sess := session.NewSession(c, session.Config{
        PingInterval: s.config.PingInterval,
        PongTimeout:  s.config.PongTimeout,
    })

    // 2. 启动心跳
    hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
    hb.SetOnTimeout(func() {
        sess.SetState(session.StateDisconnected)
        c.Close()
    })
    sess.SetState(session.StateConnected)
    hb.Start(sess)

    // 3. 注册到 Hub
    s.hub.Register(sess)

    // 4. 添加 Pipeline Handler
    //    ConnWriter: 负责实际的写操作（Head 位置，Outbound 链的最后一站）
    //    FrameCodec: 负责将 conn.Message 编码为 WebSocket 帧（Tail 位置）
    sess.Conn().Pipeline().AddFirst("headWriter", &conn.ConnWriter{Conn: c})
    sess.Conn().Pipeline().AddLast("codec", &conn.FrameCodec{IsClient: false})

    // 5. Epoll 模式专属：设置帧回调
    if nc == nil {
        s.setupEpollFrameCallback(c, sess)
    }

    // 6. 触发连接回调
    if s.onConnect != nil {
        s.onConnect(sess)
    }

    return sess
}
```

**Pipeline 结构（从 Head 到 Tail）：**

```
Head → ConnWriter → FrameCodec → [用户 Handler] → Tail
```

- **Outbound 方向**（写操作，Tail → Head）：用户 Handler → FrameCodec（编码为帧）→ ConnWriter（实际写入）
- **Inbound 方向**（读操作，Head → Tail）：帧数据 → FrameCodec（解码为 Message）→ 用户 Handler

**为什么 ConnWriter 放在 Head 位置？**

因为 Outbound 事件从 Tail 流向 Head，`ConnWriter` 是最终执行写操作的 Handler，必须放在链的最前端（Head 位置），这样所有 Outbound 事件最终都会到达它。

### setupEpollFrameCallback — Epoll 模式帧回调

Epoll 模式下，数据读取由 EventLoop 驱动，不经过 `serveConnNet` 的读循环。因此需要通过回调函数处理接收到的帧：

```go
func (s *defaultServer) setupEpollFrameCallback(c conn.Conn, sess session.Session) {
    if edc, ok := c.(conn.EventDrivenConn); ok {
        // 设置关闭回调
        edc.SetOnClose(func() {
            sess.SetState(session.StateDisconnected)
        })

        // 设置帧接收回调
        edc.SetOnFrame(func(f frame.Frame) {
            switch f.Opcode {
            case frame.OpcodeText, frame.OpcodeBinary:
                msg := &conn.Message{Type: byte(f.Opcode), Data: f.Payload}
                sess.Conn().Pipeline().FireChannelRead(msg)

            case frame.OpcodePing:
                // 通过 Pipeline 写回 Pong（走 ConnWriter）
                pongMsg := &conn.Message{Type: byte(frame.OpcodePong), Data: f.Payload}
                sess.Conn().Pipeline().FireChannelWrite(pongMsg)

            case frame.OpcodeClose:
                code := uint16(1000)
                reason := ""
                if len(f.Payload) >= 2 {
                    code = binary.BigEndian.Uint16(f.Payload[:2])
                    reason = string(f.Payload[2:])
                }
                // 通过 Pipeline 写回 Close 帧（RFC 6455 合规）
                closeMsg := &conn.Message{
                    Type: byte(frame.OpcodeClose),
                    Status: code,
                    Data: []byte(reason),
                }
                sess.Conn().Pipeline().FireChannelWrite(closeMsg)
                c.Close()
            }
        })
    }
}
```

**关键点：**

- `EventDrivenConn` 是 `conn.Conn` 的扩展接口，提供 `SetOnFrame` 和 `SetOnClose` 方法
- `epollConn` 实现了 `EventDrivenConn` 接口，`netConn` 不实现
- Ping/Pong 和 Close 帧在 Net 模式下直接通过 `frame.WriteFrame` 写入，在 Epoll 模式下通过 Pipeline 的 `FireChannelWrite` 写入（走 `ConnWriter` + `FrameCodec` 编码路径）
- `SetOnClose` 在连接被 EventLoop 关闭时触发，将 Session 状态设为 `StateDisconnected`

### serveConn — 连接服务

```go
func (s *defaultServer) serveConn(sess session.Session, nc net.Conn) {
    defer func() {
        sess.SetState(session.StateDisconnected)
        sess.Conn().Pipeline().FireChannelInactive()
        sess.Close()
        s.hub.Unregister(sess.Conn().ID())
        s.wg.Done()
    }()

    if nc == nil {
        // Epoll 模式：帧读取由 EventLoop 驱动，此 goroutine 等待关闭信号
        for st := range sess.StateChan() {
            if st == session.StateClosed || st == session.StateDisconnected {
                return
            }
        }
        return
    }

    // Net 模式：使用 bufio 读循环
    s.serveConnNet(sess, nc)
}
```

**两种模式的 serveConn 行为完全不同：**

| 方面 | Net 模式 | Epoll 模式 |
|---|---|---|
| 帧读取方式 | `bufio.Reader` + `frame.ReadFrameLimit` 循环 | EventLoop 驱动，通过 `onFrame` 回调 |
| goroutine 职责 | 读取帧 + 分发 | 仅等待关闭信号 |
| 退出条件 | 读错误 / Close 帧 / deadline 超时 | StateChan 收到 Disconnected/Closed |
| Ping/Pong 处理 | 直接 `frame.WriteFrame` | 通过 `Pipeline.FireChannelWrite` |

### serveConnNet — Net 模式读循环

```go
func (s *defaultServer) serveConnNet(sess session.Session, nc net.Conn) {
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
- `ReadFrameLimit` 带 `MaxFrameSize` 校验，防止 DoS
- Text/Binary 帧 → 构造 `conn.Message` → 触发 Pipeline 的 `FireChannelRead`
- Ping 帧 → 直接通过 `frame.WriteFrame` 回写 Pong 帧（不走 Pipeline，减少开销）
- Close 帧 → 回写 Close 响应帧（RFC 6455 合规），然后退出循环
- 读 deadline 防止 TCP 半开连接永不返回

---

## 完整数据流详解

### Net 模式数据流（初学者必读）

```
1. 客户端发起 HTTP Upgrade 请求
   ↓
2. netAcceptor.handleUpgrade() 被 http.Server 调用
   ↓
3. conn.ServerHandshake(w, r) — 完成握手，Hijack 获取 net.Conn
   ↓
4. conn.ApplyTCPOptions(nc, ...) — 设置 TCP 参数
   ↓
5. conn.NewNetConn(nc, false, conn.NextConnID()) — 创建 netConn
   ↓
6. 放入 connCh → acceptLoop 从 connCh 取出
   ↓
7. initSession(c, nc)
   ├── 创建 Session
   ├── 启动心跳
   ├── 注册到 Hub
   ├── 添加 ConnWriter + FrameCodec 到 Pipeline
   └── 触发 OnConnect 回调（用户在此添加业务 Handler）
   ↓
8. serveConnNet(sess, nc) — bufio 读循环
   ├── 收到 Text/Binary 帧 → Pipeline.FireChannelRead(msg) → 用户 Handler 处理
   ├── 收到 Ping 帧 → 直接回写 Pong
   └── 收到 Close 帧 → 回写 Close 帧后退出
   ↓
9. 用户通过 ctx.Write() 发送消息
   → Pipeline.FireChannelWrite(msg)
   → FrameCodec 编码为 WebSocket 帧
   → ConnWriter 写入 net.Conn
```

### Epoll 模式数据流（初学者必读）

```
1. 客户端发起 TCP 连接
   ↓
2. Main Reactor (mainLoop) 检测到 listenFd 可读
   ↓
3. acceptHandler.OnEvent() 调用 unix.Accept() 获取 clientFd
   ↓
4. handleNewConn(clientFd)
   ├── conn.ServerHandshakeFD(clientFd) — 在原始 fd 上完成 HTTP 握手
   ├── 设置 TCP 选项
   ├── conn.NewEpollConn(handshakeFd, false, ...) — 创建 epollConn
   ├── elg.Next() 选择一个 Sub EventLoop
   ├── ec.SetEventLoop(el) — 绑定 EventLoop
   └── el.Register(handshakeFd, handler) — 注册到 Sub Reactor
   ↓
5. 放入 connCh → acceptLoop 从 connCh 取出
   ↓
6. initSession(c, nc)  [nc == nil]
   ├── 创建 Session
   ├── 启动心跳
   ├── 注册到 Hub
   ├── 添加 ConnWriter + FrameCodec 到 Pipeline
   ├── setupEpollFrameCallback(c, sess)
   │   ├── SetOnClose: 连接关闭时设 StateDisconnected
   │   └── SetOnFrame: 帧到达时分发到 Pipeline
   └── 触发 OnConnect 回调
   ↓
7. serveConn(sess, nil) — 等待 StateChan 信号（不主动读帧）
   ↓
8. Sub Reactor (EventLoop) 检测到 fd 可读
   → epollConn 通过 IncrementalParser 解析帧
   → onFrame 回调触发
   ├── Text/Binary → Pipeline.FireChannelRead(msg) → 用户 Handler 处理
   ├── Ping → Pipeline.FireChannelWrite(pongMsg) → ConnWriter 写回
   └── Close → Pipeline.FireChannelWrite(closeMsg) → ConnWriter 写回 → c.Close()
   ↓
9. 用户通过 ctx.Write() 发送消息
   → Pipeline.FireChannelWrite(msg)
   → FrameCodec 编码为 WebSocket 帧
   → ConnWriter 写入（通过 epollConn 的 Write 方法，最终走 syscall.Write）
```

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
- 此时 ConnWriter 和 FrameCodec 已添加到 Pipeline，业务 Handler 能看到已解码的 `conn.Message`

---

## 停止服务

```go
func (s *defaultServer) Stop() error {
    if err := s.acceptor.Close(); err != nil {
        return err
    }
    // 关闭所有已注册的连接，使 serveConn goroutine 能够退出
    s.hub.CloseAll()
    // 等待所有 serveConn goroutine 完成
    s.wg.Wait()
    return nil
}
```

**关闭流程：**

1. `s.acceptor.Close()` — 停止接收新连接
   - **Net 模式**：关闭 `http.Server`，关闭 `connCh`
   - **Epoll 模式**：停止 Main Reactor 和 Sub Reactor 组，关闭 `listenFd`，关闭 `connCh`
2. `s.hub.CloseAll()` — 关闭所有已注册的连接
   - Net 模式：关闭 `net.Conn`，使 `serveConnNet` 读循环退出
   - Epoll 模式：关闭 `epollConn`，触发 `onClose` 回调，使 `serveConn` 从 `StateChan` 退出
3. `s.wg.Wait()` — 等待所有 `serveConn` goroutine 完成

**与旧版的区别：** 旧版使用 `http.Server.Shutdown()` 优雅关闭，新版通过 `Acceptor.Close()` + `Hub.CloseAll()` + `wg.Wait()` 三步实现，两种模式统一处理。

---

## 使用示例：完整 Echo 服务端

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
    cfg := ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
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

    log.Println("服务端启动，监听 :8080 ...")
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

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `server.go` | `Server` 接口、`defaultServer` 实现、`NewServer`（含 ValidateMode）、`Start`/`Stop`/`acceptLoop`/`initSession`/`setupEpollFrameCallback`/`serveConn`/`serveConnNet` |
| `server_test.go` | `NewServer`（含错误返回）、`Start`/`Stop` 测试 |
| `acceptor.go` | `Acceptor` 接口、`acceptedConn` 结构体、`netAcceptor` 实现（`Listen`/`Accept`/`Close`/`handleUpgrade`） |
| `acceptor_epoll_linux.go` | `epollAcceptor` 实现（`Listen`/`Accept`/`Close`/`handleNewConn`/`acceptHandler`），仅 Linux 编译 |
| `acceptor_factory_linux.go` | `newAcceptorForMode` 工厂函数（Linux 版，根据 Mode 选择 netAcceptor 或 epollAcceptor） |
| `acceptor_factory_nonlinux.go` | `newAcceptorForMode` 工厂函数（非 Linux 版，始终返回 netAcceptor） |
| `acceptor_test.go` | `netAcceptor` 测试 |
| `acceptor_epoll_linux_test.go` | `epollAcceptor` 测试，仅 Linux 编译 |

---

## 注意事项

1. **NewServer 返回 error** — 在非 Linux 平台使用 `ModeEpoll` 会返回错误，务必检查
2. **Start() 非阻塞** — 内部启动 `acceptLoop` goroutine 后立即返回
3. **OnConnect 在帧编解码器之后调用** — 确保业务 Handler 能看到已解码的 `conn.Message`
4. **Hub 自动注册，注销在 serveConn defer 中** — 连接断开时自动从 Hub 移除
5. **Ping 处理方式因模式而异** — Net 模式直接 `frame.WriteFrame`，Epoll 模式走 `Pipeline.FireChannelWrite`
6. **MaxConnections 为 0 表示不限制** — 开启限制后，超限连接直接关闭
7. **TCP 参数默认生效** — `TCPNoDelay=true` 关闭 Nagle，`TCPQuickAck` 和 `SOReusePort` 按需开启
8. **serveConnNet 使用 bufio 预读** — 64KB 缓冲，减少帧解析 syscall 次数
9. **Epoll 模式下 serveConn goroutine 仍存在** — 但它只等待 StateChan，不主动读帧，开销极低
10. **Stop() 会等待所有连接处理完毕** — 通过 `wg.Wait()` 确保优雅关闭

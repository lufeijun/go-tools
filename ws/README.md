# ws v2 — 高并发 WebSocket 库

`ws` 是一个从零实现 RFC 6455 WebSocket 协议的 Go 语言库。v2 版本全面重构为接口化、高并发架构，借鉴 Netty 的 Pipeline + Handler 模型，目标支撑单机 **十万到百万级** WebSocket 连接。

**本文档面向使用者**，涵盖安装、快速开始、核心概念、配置和完整示例。如果你想了解内部架构设计，请阅读 [`DESIGN.md`](DESIGN.md)。

---

## 目录

1. [核心特性](#核心特性)
2. [安装](#安装)
3. [快速开始：Echo 服务端 + 客户端](#快速开始)
4. [核心概念](#核心概念)
5. [完整示例](#完整示例)
6. [配置详解](#配置详解)
7. [架构概览](#架构概览)
8. [常见问题](#常见问题)
9. [注意事项](#注意事项)

---

## 核心特性

- **Pipeline + Handler 链** — Netty 风格的入站/出站处理器链，业务逻辑以插件方式组装
- **跨平台事件驱动** — Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP 预留)
- **引用计数 ByteBuf** — 零拷贝、分级对象池、读写指针分离
- **分片锁 Hub** — 32 个独立锁，广播性能随分片数线性扩展
- **统一错误体系** — `WSError` 携带错误码、可读消息、底层 Cause、连接 ID
- **零 goroutine 空闲开销** — 事件驱动模型下，空闲连接不绑定常驻 goroutine
- **共享时间轮心跳** — 单机级单 goroutine 管理所有连接的心跳超时
- **客户端自动重连** — 断线后按退避策略自动恢复（1s → 2s → 4s ... 最大 60s）

---

## 安装

```bash
go mod init myproject
go get github.com/lufeijun/goTools/ws
```

Go 版本要求：`>= 1.25`

---

## 快速开始

### 服务端（Echo）

```go
package main

import (
    "fmt"
    "log"
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
    if m, ok := msg.(*conn.Message); ok && m.Type == 0x1 {
        reply := fmt.Sprintf("服务端收到: %s", string(m.Data))
        ctx.Write(&conn.Message{Type: m.Type, Data: []byte(reply)})
    }
    ctx.FireChannelRead(msg)
}
func (h *EchoHandler) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *EchoHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *EchoHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

func main() {
    cfg := ws.Config{Addr: ":8080", PingInterval: 30 * time.Second, PongTimeout: 60 * time.Second}
    srv := server.NewServer(cfg)

    srv.OnConnect(func(sess session.Session) {
        sess.Conn().Pipeline().AddLast("echo", &EchoHandler{})
    })

    log.Println("服务端启动，监听 :8080 ...")
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
}
```

### 客户端

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/client"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/pipeline"
)

type PrintHandler struct{}

func (h *PrintHandler) Name() string { return "print" }
func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*conn.Message); ok && m.Type == 0x1 {
        fmt.Printf("收到: %s\n", string(m.Data))
    }
    ctx.FireChannelRead(msg)
}
func (h *PrintHandler) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *PrintHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *PrintHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

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

    // 发送一条消息
    c.Session().Conn().Pipeline().FireChannelWrite(
        &conn.Message{Type: 0x1, Data: []byte("hello")})

    time.Sleep(2 * time.Second)
}
```

**运行：**

```bash
# 终端 1
go run server.go
# 终端 2
go run client.go
```

---

## 核心概念

### Pipeline + Handler

每个连接有一个 `ChannelPipeline`，挂了一串 Handler。数据从网络进来走 **Inbound 链**（从头到尾），数据发出去走 **Outbound 链**（从尾到头）。

```
Inbound:  Head → FrameCodec → BizHandler → Tail
Outbound: Tail → FrameCodec → BizHandler → Head
```

- `InboundHandler` 处理读进来的数据：`ChannelRead`、`ChannelActive`、`ChannelInactive`
- `OutboundHandler` 处理发出去的数据：`Write`、`Flush`
- `ctx.FireChannelRead(msg)` 传给下一个 InboundHandler
- `ctx.Write(msg)` 触发 Outbound 链，从当前 Handler 位置往回走

### ByteBuf

引用计数字节缓冲区，替代 Go 原生 `[]byte`：

- **读写指针分离** — `readerIndex` / `writerIndex`
- **引用计数** — `Retain()` / `Release()` 管理跨 goroutine 生命周期
- **零拷贝切片** — `Slice()` 共享底层数组，不复制数据
- **分级对象池** — `sync.Pool` 按 512B / 4096B / 65536B 分级回收

```go
bb := buf.NewByteBuf(64)
bb.Write([]byte("hello world"))
data := bb.ReadBytes(5) // "hello"
bb.Release()
```

**重要：** ByteBuf 不是线程安全的。跨 goroutine 传递需通过 `Retain()`（发送方）和 `Release()`（接收方）管理所有权。

### Session

会话层管理连接的生命周期：

- `State()` — 当前状态：`Disconnected` / `Connecting` / `Connected` / `Reconnecting` / `Closed`
- `StateChan()` — 状态变化通知通道
- `Conn()` — 获取底层连接和 Pipeline
- `Close()` — 关闭会话

### Hub

连接管理中心，使用分片锁实现高并发：

```go
h := hub.NewHub(32) // 32 个分片
h.Register(sess)
h.Broadcast(conn.Message{Type: 0x1, Data: []byte("公告")})
h.Send(id, conn.Message{Type: 0x1, Data: []byte("私信")})
count := h.Count()
```

---

## 完整示例

### 示例 1：聊天室（Hub 广播）

```go
srv.OnConnect(func(sess session.Session) {
    srv.Hub().Register(sess)

    sess.Conn().Pipeline().AddLast("chat", &pipeline.testHandler{
        // 收到消息后广播给所有人
    })
})
```

### 示例 2：绑定业务 userID 到 Session

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

### 示例 3：客户端状态监听 + 自动重连

```go
c := client.NewClient(ws.Config{
    Addr:              "ws://localhost:8080/",
    ReconnectInterval: 3 * time.Second,
    MaxReconnect:      5,
})
c.Connect()

go func() {
    for st := range c.Session().StateChan() {
        switch st {
        case session.StateConnected:
            fmt.Println("已连接")
        case session.StateReconnecting:
            fmt.Println("正在重连...")
        case session.StateClosed:
            fmt.Println("重连耗尽，连接已关闭")
            return
        }
    }
}()
```

---

## 配置详解

`ws.Config` 是 Server 和 Client 的通用配置：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `Addr` | `string` | `""` | Server 监听地址 / Client 目标地址 |
| `ReadBufferSize` | `int` | `4096` | 读缓冲区大小 |
| `WriteBufferSize` | `int` | `4096` | 写缓冲区大小 |
| `MaxConnections` | `int` | `0` | 最大连接数，`0` 不限制 |
| `TCPNoDelay` | `bool` | `true` | 关闭 Nagle 算法 |
| `TCPQuickAck` | `bool` | `false` | Linux 启用 TCP_QUICKACK |
| `SOReusePort` | `bool` | `false` | 多进程负载均衡 |
| `EventLoopWorkers` | `int` | `runtime.NumCPU()` | SubEventLoop 数量 |
| `EventLoopStrategy` | `string` | `"roundrobin"` | 负载均衡策略 |
| `BufferPoolSmall` | `int` | `4096` | 小 buffer 池对象数 (≤512B) |
| `BufferPoolDefault` | `int` | `1024` | 默认 buffer 池对象数 (≤4096B) |
| `BufferPoolLarge` | `int` | `256` | 大 buffer 池对象数 (≤65536B) |
| `PingInterval` | `time.Duration` | `30s` | 心跳发送间隔 |
| `PongTimeout` | `time.Duration` | `60s` | Pong 回复超时 |
| `MaxFrameSize` | `int` | `64MB` | 单帧最大载荷（防 DoS） |
| `EnableCompression` | `bool` | `false` | 预留：permessage-deflate |
| `Headers` | `http.Header` | `nil` | Client 握手附加头 |
| `ReconnectInterval` | `time.Duration` | `5s` | 客户端重连初始间隔 |
| `MaxReconnect` | `int` | `5` | 客户端最大重连次数 |

### 使用默认配置

```go
cfg := ws.DefaultConfig()
cfg.Addr = ":8080"
cfg.MaxFrameSize = 16 * 1024 * 1024 // 16MB
srv := server.NewServer(cfg)
```

---

## 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│                        用户代码                              │
├─────────────────────────────────────────────────────────────┤
│                      会话层 (session)                         │
│         Session · State · Heartbeater · Reconnector         │
├─────────────────────────────────────────────────────────────┤
│                      处理器链 (pipeline)                      │
│         ChannelPipeline · InboundHandler · OutboundHandler   │
├─────────────────────────────────────────────────────────────┤
│                      连接层 (conn)                            │
│         Conn · netConn · epollConn · Handshake · FrameCodec  │
├─────────────────────────────────────────────────────────────┤
│                      事件驱动 (eventloop)                     │
│         EventLoop · Poller · epoll · kqueue · worker pool    │
├─────────────────────────────────────────────────────────────┤
│              协议层 (frame) + 缓冲区 (buf)                    │
│         Frame · ReadFrame · WriteFrame · ByteBuf · Pool      │
└─────────────────────────────────────────────────────────────┘
```

**分层依赖规则：** 上层只依赖下层接口，不能跨层调用，不能反向依赖。

| 包 | 职责 |
|---|---|
| `ws` | 根包：WSError、Config、DefaultConfig |
| `ws/buf` | ByteBuf 接口 + 引用计数实现 + 分级对象池 |
| `ws/frame` | RFC 6455 帧解析/序列化 |
| `ws/pipeline` | ChannelPipeline + Handler 链 |
| `ws/eventloop` | 跨平台事件驱动 + goroutine pool 调度 |
| `ws/conn` | Conn 接口、netConn、epollConn、握手、编解码器 |
| `ws/session` | Session 接口、状态机、时间轮心跳、自动重连 |
| `ws/hub` | 分片锁 Hub：注册/注销/广播/定向发送 |
| `ws/server` | Server 启动、HTTP Upgrade、Session 生命周期 |
| `ws/client` | Client 连接、握手、重连、Session 生命周期 |

---

## 常见问题

**Q: 客户端连不上服务端？**
1. 确认服务端已启动
2. 检查地址是否匹配（`ws://localhost:8080/` 对应 `:8080`）
3. 检查防火墙是否放行端口

**Q: 消息发出去但服务端没收到？**
1. 确认 Pipeline 中注册了处理该消息的 InboundHandler
2. 确认 Handler 中调用了 `ctx.FireChannelRead(msg)`
3. 确认 `msg.Type` 正确（文本帧 `0x1`，二进制帧 `0x2`）

**Q: v2 和 v1 有什么区别？**

| 对比项 | v1 | v2 |
|---|---|---|
| API 风格 | Channel（`ReadChan()` / `WriteChan()`） | Pipeline Handler |
| 并发模型 | goroutine-per-conn | 事件驱动（epoll/kqueue）|
| 缓冲区 | `sync.Pool` 两级复用 `[]byte` | 引用计数 ByteBuf |
| 心跳 | per-conn ticker goroutine | 共享时间轮（单 goroutine）|
| Hub | 单 goroutine + channel | 分片锁（32 个 RWMutex）|

v2 **不保证向后兼容**。

---

## 注意事项

1. **v2 不兼容 v1** — API 从 Channel 式改为 Pipeline Handler 式
2. **Pipeline 在连接建立后注册** — 通过 `srv.OnConnect` 或 `c.OnConnect` 回调添加 Handler
3. **epollConn 在 V2.1 完善** — 当前生产环境使用 `netConn`，事件驱动 Conn 的非阻塞读写完整实现将在 V2.1 交付
4. **ByteBuf 不是线程安全的** — 跨 goroutine 传递需 `Retain/Release` 管理所有权
5. **Hub 广播非阻塞** — 对慢连接直接跳过，避免广播被单个慢连接拖住
6. **MaxFrameSize 防 DoS** — 默认 64MB，建议根据业务调整

---

## License

MIT

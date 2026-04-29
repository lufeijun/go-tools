# ws v2 — Go WebSocket 库技术设计文档

## 1. 项目概览

`ws` 是一个从零实现 RFC 6455 WebSocket 协议的 Go 语言库，v2 版本在 v1 功能完整性基础上全面重构为高并发架构，目标支撑单机 **十万到百万级** WebSocket 连接。

### 核心决策

| 项 | v1 | v2 |
|---|---|---|
| 并发模型 | goroutine-per-conn（2-3 goroutine/连接） | 跨平台事件驱动（epoll/kqueue），连接不绑定常驻 goroutine |
| API 风格 | Channel 式（`ReadChan()` / `WriteChan()`） | Pipeline + Handler 链（Netty 风格） |
| 包可见性 | `internal` 隐藏实现细节 | 全部公开，接口隔离实现 |
| 缓冲区 | `sync.Pool` 两级复用 `[]byte` | 引用计数 ByteBuf，支持零拷贝、池化、读写指针分离 |
| 心跳 | per-conn ticker goroutine | 接口化，`perConnHeartbeater` 可替换为时间轮 |
| Hub | 单 goroutine + channel | 分片锁（sharded lock），32 个 `sync.RWMutex` |
| 兼容性 | — | 不保证向后兼容，全新 API |

### 设计原则

1. **面向接口编程** — 每一层只依赖下层接口，不依赖具体实现
2. **借鉴 Netty，适配 Go** — 引入 Pipeline/Handler/ByteBuf/Reactor 模型，但用 Go 协程替代 Java NIO 线程模型
3. **跨平台事件驱动** — Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP 预留) 统一抽象
4. **零 goroutine 空闲开销** — 百万连接下，无数据时 goroutine 数量 ≈ EventLoop 线程数（CPU 核数级别）

---

## 2. 架构概览

### 包结构

```
ws/
├── eventloop/          # 跨平台事件驱动层
│   ├── eventloop.go         # EventLoop / Poller / EventHandler 接口
│   ├── epoll_linux.go       # Linux epoll 实现
│   └── kqueue_bsd.go        # BSD kqueue 实现
│
├── buf/                # 引用计数 ByteBuf
│   ├── bytebuf.go           # ByteBuf 接口 + 默认实现
│   └── pool.go              # ByteBuf 对象池
│
├── frame/              # 协议层（公有，可独立使用）
│   ├── frame.go             # Frame 结构、ReadFrame、WriteFrame
│   ├── mask.go              # 掩码处理
│   └── frame_test.go        # v1 测试保留
│
├── pipeline/           # 处理器链（Netty 风格）
│   ├── handler.go           # ChannelHandler / InboundHandler / OutboundHandler / Context 接口
│   └── pipeline.go          # ChannelPipeline 实现
│
├── conn/               # 连接层
│   ├── conn.go              # Conn / EventDrivenConn 接口
│   ├── netconn.go           # 标准 net.Conn 实现
│   ├── epollconn.go         # 事件驱动 Conn 实现（V2.1 完善）
│   └── handshake.go         # RFC 6455 握手
│
├── session/            # 会话层
│   ├── session.go           # Session 接口 + 实现
│   ├── heartbeat.go         # Heartbeater 接口 + 实现
│   └── reconnect.go         # Reconnector 接口 + 实现
│
├── hub/                # 连接管理中心
│   └── hub.go               # Hub 接口 + 分片锁实现
│
├── server/             # 服务端 API
│   └── server.go            # Server + Bootstrap
│
├── client/             # 客户端 API
│   └── client.go            # Client + Bootstrap
│
└── ws.go               # 根包：WSError、Config、DefaultConfig
```

### 分层关系

```
┌─────────────────────────────────────────────────────────────┐
│                        用户代码                              │
│                  server.NewServer / client.NewClient        │
├─────────────────────────────────────────────────────────────┤
│                      会话层 (session)                         │
│         Session 接口 · State · Heartbeater · Reconnector     │
├─────────────────────────────────────────────────────────────┤
│                      处理器链 (pipeline)                      │
│         ChannelPipeline · InboundHandler · OutboundHandler   │
├─────────────────────────────────────────────────────────────┤
│                      连接层 (conn)                            │
│         Conn 接口 · netConn · epollConn · Handshake          │
├─────────────────────────────────────────────────────────────┤
│                      事件驱动 (eventloop)                     │
│         EventLoop · Poller · epollPoller · kqueuePoller      │
├─────────────────────────────────────────────────────────────┤
│              协议层 (frame) + 缓冲区 (buf)                    │
│         Frame · ReadFrame · WriteFrame · ByteBuf · Pool      │
└─────────────────────────────────────────────────────────────┘
```

**依赖规则：上层只能依赖下层接口，不能跨层调用，不能反向依赖。** 例如 `session` 依赖 `conn.Conn` 接口和 `pipeline.ChannelPipeline` 接口，但不知道 `netConn` 或 `epollConn` 的存在。

---

## 3. 核心接口定义

### 3.1 错误处理层（ws 根包）

v2 引入统一的 `WSError` 错误体系，替代 v1 的 `CloseError`。

```go
type WSError struct {
    Code    int      // 错误码：协议层(1xxx) / 网络层(2xxx) / 应用层(3xxx)
    Message string   // 可读错误描述
    Cause   error    // 底层错误（支持 errors.Is / errors.As 链）
    ConnID  uint64   // 关联连接 ID，0 表示全局错误
}
```

预定义错误码：

| 错误码 | 含义 |
|---|---|
| 1002 | 协议格式错误 |
| 1003 | 不支持的数据类型 |
| 1007 | 无效的帧格式 |
| 1008 | 策略违规 |
| 1009 | 消息过大 |
| 1011 | 内部错误 |
| 2001 | 网络读超时 |
| 2002 | 网络写超时 |
| 2003 | 连接重置 |
| 3001 | Hub 连接数超限 |

**Pipeline 错误传播：** 任何 Handler 中产生的错误通过 `ctx.FireExceptionCaught(err)` 传播到 Pipeline 链。默认处理逻辑记录日志后关闭连接，用户可自定义 `ExceptionHandler` 替换。

### 3.2 eventloop 层

```go
// EventHandler 是事件回调接口，由 conn 层实现
type EventHandler interface {
    OnEvent(fd int, events uint32)
}

// EventLoop 是事件循环抽象，每个 EventLoop 绑定一个 goroutine
type EventLoop interface {
    Register(fd int, handler EventHandler) error
    Deregister(fd int) error
    Wake()
    Run() error
    Stop() error
}

// Poller 是底层系统调用抽象
type Poller interface {
    Open() error
    Close() error
    Add(fd int, events uint32) error
    Mod(fd int, events uint32) error
    Del(fd int) error
    Wait(timeoutMs int) ([]Event, error)
}
```

**主从 Reactor 模型：**
- `MainEventLoop`：1 个，负责 `Accept` 新连接
- `SubEventLoopGroup`：N 个（默认 N = CPU 核数），每个负责一组连接的 I/O
- 新连接通过负载均衡（轮询 / 最少连接）分配到某个 SubEventLoop

**循环依赖解决：** `eventloop.EventLoop.Register` 原本需要 `conn.Conn`，但 `conn.EventDrivenConn.SetEventLoop` 又需要 `eventloop.EventLoop`。通过引入 `EventHandler` 接口（conn 实现它，eventloop 只依赖接口）以及 `SetEventLoop(el interface{})` 解除循环依赖。

### 3.3 配置管理（ws 根包）

v2 提供统一的 `Config` 结构体，Server 和 Client 共用：

```go
type Config struct {
    Addr              string
    ReadBufferSize    int           // 默认 4096
    WriteBufferSize   int           // 默认 4096
    MaxConnections    int           // 0 表示不限制
    TCPNoDelay        bool          // 默认 true
    TCPQuickAck       bool
    SOReusePort       bool
    EventLoopWorkers  int           // 默认 runtime.NumCPU()
    EventLoopStrategy string        // "roundrobin" | "leastconn"
    BufferPoolSmall   int           // 默认 4096
    BufferPoolDefault int           // 默认 1024
    BufferPoolLarge   int           // 默认 256
    PingInterval      time.Duration // 默认 30s
    PongTimeout       time.Duration // 默认 60s
    MaxFrameSize      int           // 默认 64MB
    EnableCompression bool
    Headers           http.Header
    ReconnectInterval time.Duration // 默认 5s
    MaxReconnect      int           // 默认 5
}
```

### 3.4 buf 层 — ByteBuf

```go
type ByteBuf interface {
    // 读操作
    ReadableBytes() int
    ReadBytes(n int) []byte
    ReadAll() []byte
    Skip(n int)
    Peek(n int) []byte

    // 写操作
    WritableBytes() int
    Write(p []byte) (int, error)
    WriteByte(b byte) error
    EnsureWritable(min int)

    // 零拷贝切片（共享底层数组，引用计数 +1）
    Slice(start, length int) ByteBuf

    // 引用计数
    Retain() ByteBuf
    Release()
    RefCount() int

    // 内部访问
    Bytes() []byte
    ReaderIndex() int
    WriterIndex() int
    SetReaderIndex(int)
    SetWriterIndex(int)
}

type Pool interface {
    Get(capacity int) ByteBuf
    Put(ByteBuf)
}
```

**设计要点：**
- `Retain()` / `Release()` 管理生命周期，防止 goroutine 间传递时的提前释放
- `Slice()` 创建共享底层数组的新视图，零拷贝，引用计数 +1
- 默认实现 `byteBuf` 使用 `sync.Pool` 管理底层 `[]byte`，支持分级回收

**内存泄漏防护：**
- `Release()` 时 `refCount < 0` 触发 panic（double-free 保护）
- `Retain()` 时 `refCount <= 1` 触发 panic（在已释放的 buffer 上操作）
- 所有从 Pool 取出的 ByteBuf `refCount` 初始化为 1

### 3.5 pipeline 层

```go
type ChannelPipeline interface {
    AddFirst(name string, handler ChannelHandler) ChannelPipeline
    AddLast(name string, handler ChannelHandler) ChannelPipeline
    Remove(name string) ChannelPipeline
    FireChannelRead(msg interface{})
    FireChannelWrite(msg interface{})
    FireChannelActive()
    FireChannelInactive()
    FireExceptionCaught(err error)
}

type InboundHandler interface {
    ChannelHandler
    ChannelRead(ctx Context, msg interface{})
    ChannelActive(ctx Context)
    ChannelInactive(ctx Context)
    ExceptionCaught(ctx Context, err error)
}

type OutboundHandler interface {
    ChannelHandler
    Write(ctx Context, msg interface{})
    Flush(ctx Context)
}

type Context interface {
    Pipeline() ChannelPipeline
    FireChannelRead(msg interface{})
    FireChannelWrite(msg interface{})
    FireChannelActive()
    FireChannelInactive()
    Write(msg interface{})
    Flush()
}
```

**数据流向：**
```
Inbound:  eventloop 读数据 → ByteBuf → Head → FrameDecoder → HeartbeatHandler → BizHandler → Tail
Outbound: BizHandler.Write → FrameEncoder → Head.Write → eventloop 写数据
```

- `FireChannelRead` 从 Head 向 Tail 遍历所有 `InboundHandler`
- `FireChannelWrite` 从 Tail 向 Head 遍历所有 `OutboundHandler`
- 每个 Handler 通过 `Context` 将事件传递给链中的下一个 Handler
- `AddFirst` / `AddLast` 在 Pipeline 构建时使用，运行时事件遍历不加锁（Pipeline 构建后不再修改）

### 3.6 conn 层

```go
type Conn interface {
    ID() uint64
    Pipeline() pipeline.ChannelPipeline
    Read(b buf.ByteBuf) error
    Write(b buf.ByteBuf) error
    RemoteAddr() net.Addr
    LocalAddr() net.Addr
    IsClient() bool
    Close() error
    Active() bool
}

type EventDrivenConn interface {
    Conn
    FD() int
    OnEvent(events uint32)
    SetEventLoop(el interface{})
}
```

**两种实现：**
- `netConn`：基于标准 `net.Conn`，开发调试和跨平台 fallback
- `epollConn`：基于 `eventloop.Poller`，无常驻 goroutine，事件触发时调度（V2.1 完善非阻塞读写）

**握手协议（RFC 6455）：**
- `ServerHandshake(w, r)`：验证 GET / Upgrade / Connection / Sec-WebSocket-Key / Version，计算 `Sec-WebSocket-Accept`，通过 `http.Hijacker` 接管连接
- `ClientHandshake(rawURL, headers)`：支持 `ws://`（TCP 80）和 `wss://`（TLS 443），生成随机 Key，验证服务端 101 响应

### 3.7 session 层

```go
type Session interface {
    Conn() conn.Conn
    State() State
    StateChan() <-chan State
    SetState(State)
    Close() error
}

type State int
const (
    StateDisconnected State = iota
    StateConnecting
    StateConnected
    StateReconnecting
    StateClosed
)
```

**心跳（Heartbeater）：** `perConnHeartbeater` 使用 `time.Ticker` 定时发送 Ping 帧。V2 设计改为只发 Ping 不消费 ReadChan，避免心跳与用户读消息冲突。Pong 超时检测推迟到 V2.1（用时间轮在 conn 层拦截 Pong）。

**自动重连（Reconnector）：** 客户端断开后按配置间隔重试拨号，达到最大重试次数后进入 `StateClosed`。

### 3.8 hub 层 — 分片锁

```go
type Hub interface {
    Register(s session.Session)
    Unregister(id uint64)
    Broadcast(msg conn.Message)
    Send(id uint64, msg conn.Message)
    Count() int
    Get(id uint64) session.Session
}
```

v1 的单 goroutine + channel 模式在广播时成为瓶颈。v2 改用**分片锁（sharded lock）**：

```go
type shardedHub struct {
    shardCount int
    shards     []*shard
}

type shard struct {
    mu    sync.RWMutex
    conns map[uint64]session.Session
}
```

- `shardCount` 默认 32，每个 shard 独立 `sync.RWMutex`
- `Register` / `Unregister`：写锁单个 shard，不影响其他 shard
- `Get`：读锁单个 shard，O(1)
- `Count`：遍历所有 shard 读锁求和
- `Broadcast`：并发遍历所有 shard（每个 shard 一个 goroutine），shard 内对每个 Session 非阻塞写入

---

## 4. 数据流

### 4.1 服务端收消息（Inbound）

```
客户端 TCP 帧
    │
    ▼
[eventloop.Poller.Wait] 检测到 Read 事件
    │
    ▼
[EventLoop] 调度到对应 Conn
    │
    ▼
[conn.Read] 从 fd 读取原始字节到 ByteBuf
    │
    ▼
[pipeline.FireChannelRead] 触发 Inbound 链
    │
    ├── [FrameDecoder] ByteBuf → Frame（RFC 6455 解析）
    │
    ├── [MaskDecoder] 客户端帧去掩码
    │
    ├── [HeartbeatHandler] Ping 自动回 Pong
    │
    └── [BizHandler] Frame → Message → 业务逻辑
```

### 4.2 服务端发消息（Outbound）

```
业务逻辑调用 ctx.Write(Message)
    │
    ▼
[pipeline.FireChannelWrite] 触发 Outbound 链（从尾到头）
    │
    ├── [BizHandler] Message → Frame
    │
    ├── [MaskEncoder] 服务端→客户端帧加掩码
    │
    └── [FrameEncoder] Frame → ByteBuf
    │
    ▼
[HeadContext.Write] 将 ByteBuf 加入 Conn 发送队列
    │
    ▼
[eventloop] 注册 Write 事件，触发 TCP 发送
    │
    ▼
[conn.Write] ByteBuf → fd
    │
    ▼
ByteBuf.Release()  引用计数 -1，归零时回收到 Pool
```

### 4.3 广播

```
Hub.Broadcast(Message)
    │
    ▼
[shardedHub] 并发遍历所有 shard（每个 shard 一个 goroutine）
    │
    ├── shard[0]: RLock → 遍历 conns → 每个 Session Pipeline 非阻塞写入
    ├── shard[1]: RLock → ...
    └── ...
```

---

## 5. 高性能设计要点

### 5.1 零 goroutine 空闲开销

v1 每个连接 2-3 个 goroutine，百万连接 ≈ 200-300 万 goroutine ≈ 4-6GB 栈内存。

v2 的 epollConn：
- 连接建立时：0 个专属 goroutine
- 有数据可读时：EventLoop goroutine 直接处理
- 需要长时间计算的业务逻辑：通过 Pipeline Handler 提交到业务 goroutine 池
- **空闲连接零 goroutine 开销**

### 5.2 零拷贝

- eventloop 读数据时直接写入 `ByteBuf`，不经过中间 buffer
- `FrameDecoder` 解析出的 Payload 通过 `ByteBuf.Slice()` 共享底层数组
- `FrameEncoder` 序列化时直接写入发送队列的 `ByteBuf`
- 广播时同一份 `ByteBuf` 通过 `Retain()` 增加引用计数，发送到多个连接后各 `Release()`

### 5.3 内存池化

- `ByteBuf` 使用 `sync.Pool` 分级回收：≤512B / ≤4096B / ≤65536B / 直接分配
- 帧读写频繁分配/释放内存，百万连接下 GC 压力巨大，池化是必需品

### 5.4 减少系统调用

- `WriteFrame` 合并 header + payload 到一个 `ByteBuf` 后单次 `write()`
- 建连时默认 `TCP_NODELAY`（关闭 Nagle），避免小包延迟
- V2 修复了 v1 中 header + payload 两次 Write 导致的 Nagle + Delayed ACK 交互延迟问题

### 5.5 TCP 优化

- `TCP_NODELAY`：默认开启，关闭 Nagle 算法
- `TCP_QUICKACK`：Linux 可选开启
- `SO_REUSEPORT`：多进程负载均衡可选开启

### 5.6 背压处理（Backpressure）

**Hub 广播背压：** `Broadcast()` 对慢连接非阻塞写入，写满直接跳过，避免广播 goroutine 阻塞在单个慢连接上。

**Pipeline 层背压：** 每个 Conn 的发送队列设上限，队列满时 `ctx.Write()` 返回错误，业务层可选择丢弃或阻塞。

---

## 6. 跨平台策略

| 平台 | 事件驱动机制 | 实现文件 | 备注 |
|---|---|---|---|
| Linux | epoll | `eventloop/epoll_linux.go` | 主力平台，百万连接目标 |
| macOS / FreeBSD / OpenBSD | kqueue | `eventloop/kqueue_bsd.go` | 开发调试 |
| Windows | IOCP | 预留接口 | V2.1 实现 |
| 其他 | 标准 net.Conn | `conn/netconn.go` | fallback，功能完整但性能受限 |

**编译约束：**
```go
// eventloop/epoll_linux.go
//go:build linux

// eventloop/kqueue_bsd.go
//go:build darwin || freebsd || openbsd
```

---

## 7. V1 → V2 迁移说明

v2 **不保证向后兼容**。关键变化：

| 变化项 | v1 | v2 |
|---|---|---|
| import 路径 | 全部 `ws` 根包 | 子包按需 import |
| API 风格 | Channel（`ReadChan()` / `WriteChan()`） | Pipeline Handler（`ChannelRead(ctx, msg)`） |
| 接收消息 | `for msg := range sess.ReadChan()` | 实现 `InboundHandler`，注册到 Pipeline |
| 发送消息 | `sess.WriteChan() <- msg` | `ctx.Write(msg)` 在 Handler 中发送 |
| 连接管理 | `internal` 包不可扩展 | `conn`、`session`、`hub` 全部公开，接口隔离 |
| 类型位置 | `ws.Session` 是类型别名 | `session.Session` 是直接类型（包已公开） |

**迁移示例：**
```go
// v1
for msg := range sess.ReadChan() {
    sess.WriteChan() <- msg
}

// v2
type EchoHandler struct{}
func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    ctx.Write(msg)
}
```

---

## 8. 测试策略

| 层级 | 测试方式 | 目标 |
|---|---|---|
| eventloop | mock Poller，测试 EventLoop 调度逻辑 | 事件分发正确 |
| buf | 单元测试引用计数、Slice、Pool | 无内存泄漏、零拷贝正确 |
| frame | 单元测试，构造字节序列验证解析/序列化 | RFC 6455 合规（v1 测试保留） |
| pipeline | mock Handler，测试链式调用 | Inbound/Outbound 顺序正确 |
| conn | `net.Pipe` 测试 netConn；接口测试 epollConn | 读写、握手正确 |
| session | mock Conn + Pipeline，测试状态/心跳/重连 | 状态机正确 |
| hub | mock Session，测试注册/注销/广播 | 分片锁正确 |
| 集成 | 真实 Server + Client 端到端 | 功能完整 |
| 压力 | 10万+ 连接监控 goroutine/内存/CPU | 确认性能目标 |

---

## 9. V2 演进路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 接口化重构 + Pipeline + 事件驱动（epoll/kqueue） | 十万连接 |
| V2.1 | Windows IOCP + 时间轮心跳 + 非阻塞 syscall 读写 | 跨平台完整 |
| V2.2 | goroutine 池化（业务计算池 + I/O 池分离） | 五十万连接 |
| V2.3 | sendfile/splice 零拷贝、SO_REUSEPORT | 百万连接 |

# ws v2 — 高并发 WebSocket 库设计文档

## 1. 项目概览

v2 在 v1 的功能完整性基础上，全面重构为高并发架构，目标支撑单机 **十万到百万级** WebSocket 连接。

### 核心决策

| 项 | v1 | v2 |
|---|---|---|
| 并发模型 | goroutine-per-conn（2-3 goroutine/连接） | 跨平台事件驱动（epoll/kqueue/iocp），连接不绑定常驻 goroutine |
| API 风格 | Channel 式 | Pipeline + Handler 链（Netty 风格） |
| 包可见性 | internal 隐藏实现细节 | 全部公开，接口隔离实现 |
| 缓冲区 | sync.Pool 两级复用 `[]byte` | 引用计数 ByteBuf，支持零拷贝、池化、读写指针分离 |
| 心跳 | per-conn ticker goroutine | 接口化，V2 默认 per-conn，可替换为时间轮 |
| 兼容性 | — | 不保证向后兼容，全新 API |

### 设计原则

1. **面向接口编程** — 每一层只依赖下层接口，不依赖具体实现
2. **借鉴 Netty，适配 Go** — 引入 Pipeline/Handler/ByteBuf/Reactor 模型，但用 Go 协程替代 Java NIO 线程模型
3. **跨平台事件驱动** — Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP) 统一抽象
4. **零 goroutine 空闲开销** — 百万连接下，无数据时 goroutine 数量 ≈ EventLoop 线程数（通常 CPU 核数级别）

---

## 2. 架构概览

### 包结构

```
ws/
├── eventloop/          # 跨平台事件驱动层
│   ├── eventloop.go         # EventLoop / Poller 接口
│   ├── epoll_linux.go       # Linux epoll 实现
│   ├── kqueue_bsd.go        # BSD kqueue 实现
│   └── iocp_windows.go      # Windows IOCP 实现（预留）
│
├── buf/                # 引用计数 ByteBuf
│   ├── bytebuf.go           # ByteBuf 接口 + 实现
│   └── pool.go              # ByteBuf 对象池
│
├── frame/              # 协议层（公有，可独立使用）
│   ├── frame.go             # Frame 结构、ReadFrame、WriteFrame
│   ├── mask.go              # 掩码处理
│   └── pool.go              # 底层 buffer 池（使用 buf 包）
│
├── pipeline/           # 处理器链
│   ├── pipeline.go          # ChannelPipeline 接口 + 实现
│   └── handler.go           # ChannelHandler 接口族
│
├── conn/               # 连接层
│   ├── conn.go              # Conn 接口
│   ├── netconn.go           # 标准 net.Conn 实现（fallback / 开发调试）
│   └── epollconn.go         # 事件驱动 Conn 实现
│
├── session/            # 会话层
│   ├── session.go           # Session 接口 + 实现
│   ├── heartbeat.go         # Heartbeater 接口 + 实现
│   └── reconnect.go         # Reconnector 接口 + 实现
│
├── hub/                # 连接管理中心
│   └── hub.go               # Hub 接口 + 实现
│
├── server/             # 服务端 API
│   └── server.go            # Server、Bootstrap、配置
│
├── client/             # 客户端 API
│   └── client.go            # Client、Bootstrap、配置
│
└── ws.go               # 根包：可选 re-export 关键类型
```

### 分层关系

```
┌─────────────────────────────────────────────────────────────┐
│                        用户代码                              │
│                  server.Bootstrap / client.Bootstrap         │
├─────────────────────────────────────────────────────────────┤
│                      会话层 (session)                         │
│         Session 接口 · State · Heartbeater · Reconnector     │
├─────────────────────────────────────────────────────────────┤
│                      处理器链 (pipeline)                      │
│         ChannelPipeline · InboundHandler · OutboundHandler   │
├─────────────────────────────────────────────────────────────┤
│                      连接层 (conn)                            │
│         Conn 接口 · epollConn · netConn                      │
├─────────────────────────────────────────────────────────────┤
│                      事件驱动 (eventloop)                     │
│         EventLoop · Poller · epollPoller · kqueuePoller      │
├─────────────────────────────────────────────────────────────┤
│              协议层 (frame) + 缓冲区 (buf)                    │
│         Frame · ReadFrame · WriteFrame · ByteBuf · Pool      │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. 核心接口定义

### 3.1 错误处理层

v2 引入统一的错误体系，替代 v1 的 `CloseError`。

```go
package ws

// WSError 是 v2 统一错误类型，携带连接上下文便于定位问题
type WSError struct {
    Code    int      // 错误码：协议层(1xxx) / 网络层(2xxx) / 应用层(3xxx)
    Message string   // 可读错误描述
    Cause   error    // 底层错误（支持 errors.Is / errors.As 链）
    ConnID  uint64   // 关联连接 ID，0 表示全局错误
}

func (e *WSError) Error() string
func (e *WSError) Unwrap() error

// 预定义错误码
const (
    ErrCodeProtocolError    = 1002 // 协议格式错误
    ErrCodeUnsupportedData  = 1003
    ErrCodeInvalidFrame     = 1007
    ErrCodePolicyViolation  = 1008
    ErrCodeMessageTooBig    = 1009
    ErrCodeInternalError    = 1011
    ErrCodeReadTimeout      = 2001 // 网络读超时
    ErrCodeWriteTimeout     = 2002
    ErrCodeConnReset        = 2003
    ErrCodeHubFull          = 3001 // Hub 连接数超限
)
```

**Pipeline 错误传播：**
- 任何 Handler 中产生的错误通过 `ctx.FireExceptionCaught(err)` 传播
- 默认 `ExceptionHandler` 记录日志后关闭连接
- 用户可自定义 `ExceptionHandler` 替换默认行为

---

### 3.2 eventloop 层

```go
package eventloop

// EventHandler 是事件回调接口，由 conn 层实现，eventloop 只操作 fd 和回调
type EventHandler interface {
    OnEvent(fd int, events uint32)  // 有事件时回调
}

// EventLoop 是事件循环抽象，每个 EventLoop 绑定一个 goroutine，管理一组连接
type EventLoop interface {
    Register(fd int, handler EventHandler) error  // 将 fd 注册到事件循环
    Deregister(fd int) error                      // 注销 fd
    Wake()                                        // 唤醒事件循环（用于跨 goroutine 写）
    Run() error                                   // 阻塞运行事件循环
    Stop() error                                  // 停止事件循环
}

// Poller 是底层系统调用抽象
type Poller interface {
    Open() error
    Close() error
    Add(fd int, events uint32) error         // 注册 fd 关心的事件
    Mod(fd int, events uint32) error         // 修改 fd 关心的事件
    Del(fd int) error                        // 删除 fd
    Wait(timeout int) ([]Event, error)       // 阻塞等待事件，timeout 毫秒
}

type Event struct {
    FD     int
    Events uint32  // Read / Write / Error / Close
}

const (
    EventRead  uint32 = 1 << iota
    EventWrite
    EventError
    EventHup
)
```

**主从 Reactor 模型：**
- `MainEventLoop`：1 个，只负责 `Accept` 新连接
- `SubEventLoopGroup`：N 个（通常 N = CPU 核数），每个 SubEventLoop 负责一组连接的 I/O 读写
- 新连接通过负载均衡（轮询 / 最少连接）分配到某个 SubEventLoop

### 3.3 配置管理

v2 提供统一的 `Config` 结构体，Server 和 Client 通过 `Bootstrap` 模式配置。

```go
package ws

import "time"

// Config 是全局配置，Server 和 Client 共用
// 各字段零值表示使用默认值
 type Config struct {
    // 网络配置
    Addr              string        // 监听地址（Server）/ 目标地址（Client）
    ReadBufferSize    int           // 读缓冲区大小，默认 4096
    WriteBufferSize   int           // 写缓冲区大小，默认 4096
    MaxConnections    int           // 最大连接数，0 表示不限制
    TCPNoDelay        bool          // 默认 true（关闭 Nagle）
    TCPQuickAck       bool          // Linux 下启用 TCP_QUICKACK，默认 false
    SOReusePort       bool          // 启用 SO_REUSEPORT（多进程负载均衡），默认 false

    // 事件驱动配置
    EventLoopWorkers  int           // SubEventLoop 数量，默认 runtime.NumCPU()
    EventLoopStrategy string        // 负载均衡策略: "roundrobin" | "leastconn"，默认 "roundrobin"

    // 缓冲区配置
    BufferPoolSmall   int           // 小 buffer 池大小 (≤512B)，默认 4096
    BufferPoolDefault int           // 默认 buffer 池大小 (≤4096B)，默认 1024
    BufferPoolLarge   int           // 大 buffer 池大小 (≤65536B)，默认 256

    // 心跳配置
    PingInterval      time.Duration // 心跳间隔，默认 30s
    PongTimeout       time.Duration // Pong 超时，默认 60s

    // 协议配置
    MaxFrameSize      int           // 单帧最大载荷，默认 64MB
    EnableCompression bool          // 预留：permessage-deflate 扩展，默认 false

    // 客户端专用
    Headers           http.Header   // 握手自定义 HTTP 头
    ReconnectInterval time.Duration // 重连间隔，默认 5s
    MaxReconnect      int           // 最大重连次数，默认 5
}
```

**Bootstrap 使用示例：**
```go
srv := server.NewBootstrap(ws.Config{
    Addr:             ":8080",
    MaxConnections:   100000,
    EventLoopWorkers: 8,
    MaxFrameSize:     1024 * 1024, // 1MB
}).Handler(myHandler).Build()
```

---

### 3.4 buf 层 — ByteBuf

```go
package buf

// ByteBuf 是引用计数的字节缓冲区，支持读写指针分离、零拷贝切片
type ByteBuf interface {
    // 读操作
    ReadableBytes() int
    ReadBytes(n int) []byte
    ReadAll() []byte
    Skip(n int)
    Peek(n int) []byte  // 不移动读指针

    // 写操作
    WritableBytes() int
    Write(p []byte) (int, error)
    WriteByte(b byte) error
    EnsureWritable(min int)

    // 零拷贝切片（共享底层数组，新 ByteBuf 引用计数 +1）
    Slice(start, length int) ByteBuf

    // 引用计数
    Retain() ByteBuf
    Release()
    RefCount() int

    // 内部访问（供 frame 层使用）
    Bytes() []byte
    ReaderIndex() int
    WriterIndex() int
    SetReaderIndex(int)
    SetWriterIndex(int)
}

// Pool 是 ByteBuf 对象池
type Pool interface {
    Get(capacity int) ByteBuf
    Put(ByteBuf)
}
```

**设计要点：**
- `Retain()` / `Release()` 管理生命周期，防止 goroutine 间传递时的提前释放
- `Slice()` 创建共享底层数组的新视图，零拷贝，引用计数 +1
- 写入 frame 时，`WriteFrame` 直接操作 ByteBuf 的 `WriterIndex`，避免多次内存拷贝
- 默认实现使用 `sync.Pool` 管理底层 `[]byte`，大 buffer 直接分配

**内存泄漏防护：**
```go
func (b *byteBuf) Release() {
    rc := atomic.AddInt32(&b.refCount, -1)
    if rc == 0 {
        // 引用计数归零，回收到 Pool
        b.readerIndex = 0
        b.writerIndex = 0
        if b.pool != nil {
            b.pool.Put(b)
        }
    } else if rc < 0 {
        // double-free 保护：引用计数已为负，说明存在 Bug
        // 开发模式 panic，生产模式记录错误日志
        panic(fmt.Sprintf("ByteBuf(%p) reference count below zero: %d", b, rc))
    }
}
```
- `Retain()` 时若 `refCount <= 0` 同样 panic，防止在已释放的 ByteBuf 上操作
- 所有从 Pool 取出的 ByteBuf `refCount` 初始化为 1
- 通过 `go vet` + 单元测试覆盖所有 `Retain()`/`Release()` 路径

### 3.3 pipeline 层

```go
package pipeline

import "github.com/lufeijun/goTools/ws/buf"

// ChannelPipeline 是每个连接的数据处理链
type ChannelPipeline interface {
    AddFirst(name string, handler ChannelHandler) ChannelPipeline
    AddLast(name string, handler ChannelHandler) ChannelPipeline
    Remove(name string) ChannelPipeline
    FireChannelRead(msg interface{})  // 触发 Inbound 链
    FireChannelWrite(msg interface{}) // 触发 Outbound 链
    FireChannelActive()
    FireChannelInactive()
    FireExceptionCaught(err error)
}

// ChannelHandler 是 Handler 基接口
type ChannelHandler interface {
    Name() string
}

// InboundHandler 处理入站数据（读）
type InboundHandler interface {
    ChannelHandler
    ChannelRead(ctx Context, msg interface{})
    ChannelActive(ctx Context)
    ChannelInactive(ctx Context)
    ExceptionCaught(ctx Context, err error)
}

// OutboundHandler 处理出站数据（写）
type OutboundHandler interface {
    ChannelHandler
    Write(ctx Context, msg interface{})
    Flush(ctx Context)
}

// Context 是 Handler 在 Pipeline 中的上下文
type Context interface {
    Pipeline() ChannelPipeline
    FireChannelRead(msg interface{})
    FireChannelWrite(msg interface{})
    Write(msg interface{})
    Flush()
    // ... 其他辅助方法
}
```

**数据流向：**
```
Inbound:  eventloop 读数据 → ByteBuf → HeadContext.Read → FrameDecoder → HeartbeatHandler → BizHandler → TailContext
Outbound: BizHandler.Write → FrameEncoder → HeadContext.Write → eventloop 写数据
```

- `HeadContext` 是 Pipeline 的头部，对接 conn/eventloop，负责实际的 I/O
- `TailContext` 是 Pipeline 的尾部，默认丢弃未处理的入站数据

### 3.4 conn 层

```go
package conn

import (
    "net"
    "github.com/lufeijun/goTools/ws/buf"
    "github.com/lufeijun/goTools/ws/pipeline"
)

// Conn 是 WebSocket 连接的抽象接口
type Conn interface {
    ID() uint64
    Pipeline() pipeline.ChannelPipeline

    // I/O — 由 eventloop 调用，用户不直接调用
    Read(buf.ByteBuf) error   // eventloop 触发读时调用
    Write(buf.ByteBuf) error  // eventloop 触发写时调用

    // 连接信息
    RemoteAddr() net.Addr
    LocalAddr() net.Addr
    IsClient() bool

    // 生命周期
    Close() error
    Active() bool
}

// EventDrivenConn 是事件驱动实现的 Conn 接口
type EventDrivenConn interface {
    Conn
    FD() int                    // 底层文件描述符
    OnEvent(events uint32)      // eventloop 回调：有事件时触发
    SetEventLoop(el interface{}) // 设置事件循环（实际类型为 eventloop.EventLoop，用 interface{} 避免循环依赖）
}
```

**两种实现：**
- `netConn`：基于标准 `net.Conn`，一个 goroutine 处理读写（开发调试 / 跨平台 fallback）
- `epollConn`：基于 `eventloop.Poller`，无常驻 goroutine，事件触发时调度

### 3.5 session 层

```go
package session

import (
    "time"
    "github.com/lufeijun/goTools/ws/conn"
)

// Session 是会话抽象，在 Conn + Pipeline 之上管理状态、心跳、重连
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

// Heartbeater 心跳接口，可替换实现
type Heartbeater interface {
    Start(s Session)
    Stop()
    SetOnTimeout(fn func())
}

// Reconnector 自动重连接口（客户端）
type Reconnector interface {
    Start(s Session)
    Stop()
}
```

### 3.6 hub 层

```go
package hub

import (
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/session"
)

// Hub 连接管理中心
type Hub interface {
    Register(s session.Session)       // 注册连接
    Unregister(id uint64)             // 注销连接
    Broadcast(msg conn.Message)       // 广播（非阻塞）
    Send(id uint64, msg conn.Message) // 定向发送
    Count() int                       // 连接数
    Get(id uint64) session.Session    // 按 ID 查找
}
```

**分片锁实现：**

v1 的单 goroutine + channel 模式在广播时成为瓶颈（遍历百万连接）。v2 改用**分片锁（sharded lock）**：

```go
type shardedHub struct {
    shardCount int
    shards     []*shard
}

type shard struct {
    mu    sync.RWMutex
    conns map[uint64]session.Session
}

func (h *shardedHub) shardIndex(id uint64) int {
    return int(id % uint64(h.shardCount))
}
```

- `shardCount` 默认 32（可配置），每个 shard 独立 `sync.RWMutex`
- `Register` / `Unregister`：写锁单个 shard，不影响其他 shard
- `Get`：读锁单个 shard，O(1)
- `Count`：遍历所有 shard 读锁求和
- `Broadcast`：并发遍历所有 shard（每个 shard 一个 goroutine），shard 内对每个 Session 非阻塞写入

**权衡：** 分片锁在 Register/Unregister 频繁时仍有竞争，但广播性能与 shard 数量成正比。百万连接下 32 个 shard 可将广播锁竞争降低 32 倍。

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
[EventLoop] 调度到对应 Conn 的 goroutine（或复用 EventLoop goroutine）
    │
    ▼
[conn.Read] 从 fd 读取原始字节到 ByteBuf
    │
    ▼
[pipeline.FireChannelRead] 触发 Inbound 链
    │
    ├── [FrameDecoder] ByteBuf → Frame（RFC 6455 解析）
    │   └── 如果是分片帧，内部缓冲等待 FIN
    │
    ├── [MaskDecoder] 客户端帧去掩码（服务端方向）
    │
    ├── [HeartbeatHandler] Ping 自动回 Pong，Pong 更新时间轮
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
    ├── [MaskEncoder] 服务端→客户端帧加掩码（客户端方向）
    │
    └── [FrameEncoder] Frame → ByteBuf（序列化到 ByteBuf）
    │
    ▼
[HeadContext.Write] 将 ByteBuf 加入 Conn 的发送队列
    │
    ▼
[eventloop] 注册 Write 事件，触发实际 TCP 发送
    │
    ▼
[conn.Write] ByteBuf → fd（write syscall）
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
    │   ├── 快连接：ByteBuf 进入发送队列，Retain() 引用计数 +1
    │   └── 慢连接：Pipeline 写队列满，丢弃（Release() 释放）
    ├── shard[1]: RLock → ...
    └── ...
```

---

## 5. 高性能设计要点

### 5.1 零 goroutine 空闲开销

v1 每个连接 2-3 个 goroutine，百万连接 = 200-300 万 goroutine ≈ 4-6GB 栈内存。

v2 的 epollConn：
- 连接建立时：0 个专属 goroutine
- 有数据可读时：EventLoop goroutine 直接处理，或提交到 goroutine 池
- 需要长时间计算的业务逻辑：通过 Pipeline Handler 提交到业务 goroutine 池
- **空闲连接零 goroutine 开销**

### 5.2 零拷贝

- `eventloop` 读数据时，直接写入 `ByteBuf`，不经过中间 buffer
- `FrameDecoder` 解析出的 Payload 通过 `ByteBuf.Slice()` 共享底层数组，不拷贝
- `FrameEncoder` 序列化时直接写入发送队列的 `ByteBuf`
- 广播时同一份 `ByteBuf` 通过 `Retain()` 增加引用计数，发送到多个连接，各连接 `Release()`

### 5.3 内存池化

- `ByteBuf` 使用 `sync.Pool` 分级回收：≤512B / ≤4096B / ≤65536B / 直接分配
- `Frame` 对象使用 `sync.Pool`
- `Event` 对象使用 `sync.Pool`

### 5.4 减少系统调用

- `WriteFrame` 合并 header + payload 到一个 `ByteBuf` 后单次 `write()`
- `eventloop` 批量处理同一轮 `Wait` 返回的所有事件，减少 `epoll_wait` 频率
- 写数据时先攒到发送缓冲区，达到一定量或遇到 `EAGAIN` 再 flush

### 5.5 TCP 优化

- 建连时默认 `TCP_NODELAY`（关闭 Nagle）
- 支持 `TCP_QUICKACK`（Linux）
- 支持 `SO_REUSEPORT`（多进程负载均衡，可选）

### 5.6 背压处理（Backpressure）

v2 在以下层面提供背压机制，防止快生产者拖垮慢消费者：

**Pipeline 层背压：**
- 每个 Conn 的发送队列（Outbound buffer）设上限（默认 256 个 Frame）
- 队列满时 `ctx.Write()` 返回 `ErrWriteBufferFull`，业务层可选择丢弃或阻塞
- `Flush()` 显式触发写入，业务层可攒批后统一 flush

**Hub 广播背压：**
- `Broadcast()` 对慢连接非阻塞写入，写满直接跳过（见 3.6 分片锁设计）
- 提供 `BroadcastWithTimeout(msg, timeout)` 可选接口，给慢连接一个短暂等待窗口

**EventLoop 层背压（V2.1）：**
- 当某 EventLoop 管理的连接整体发送缓冲区接近上限时，暂停读取新数据（暂停 `EPOLLIN`）
- 待发送缓冲区下降后恢复读取，天然实现 TCP 背压传播

**应用层背压（用户实现）：**
- 业务 Handler 可通过 `ctx.FireChannelRead()` 控制消费速率
- 提供 `RateLimitHandler` 作为可选内置 Handler，基于令牌桶限流

---

## 6. 跨平台策略

| 平台 | 事件驱动机制 | 实现文件 | 备注 |
|---|---|---|---|
| Linux | epoll | `eventloop/epoll_linux.go` | 主力平台，百万连接目标 |
| macOS / FreeBSD / OpenBSD | kqueue | `eventloop/kqueue_bsd.go` | 开发调试 |
| Windows | IOCP | `eventloop/iocp_windows.go` | 预留接口，V2.1 实现 |
| 其他 | 标准 net.Conn | `conn/netconn.go` | fallback，功能完整但性能受限 |

**编译约束：**
```go
// eventloop/epoll_linux.go
//go:build linux
// +build linux

// eventloop/kqueue_bsd.go
//go:build darwin || freebsd || openbsd
// +build darwin freebsd openbsd

// eventloop/iocp_windows.go
//go:build windows
// +build windows
```

**运行时选择：**
```go
func NewPoller() (Poller, error) {
    switch runtime.GOOS {
    case "linux":
        return newEpollPoller()
    case "darwin", "freebsd", "openbsd":
        return newKqueuePoller()
    case "windows":
        return newIOCPPoller()
    default:
        return nil, errors.New("unsupported platform")
    }
}
```

---

## 7. V1 → V2 迁移说明

v2 **不保证向后兼容**。关键变化：

| 变化项 | v1 | v2 |
|---|---|---|
| import 路径 | `github.com/lufeijun/goTools/ws` | 子包按需 import，或使用根包 re-export |
| API 风格 | Channel (`ReadChan()` / `WriteChan()`) | Pipeline Handler (`ChannelRead(ctx, msg)`) |
| 接收消息 | `for msg := range sess.ReadChan()` | 实现 `InboundHandler`，注册到 Pipeline |
| 发送消息 | `sess.WriteChan() <- msg` | `ctx.Write(msg)` 在 Handler 中发送 |
| 连接管理 | `srv.Hub().Register(sess)` | Bootstrap 配置自动注册，或手动调用 Hub.Register |
| 类型位置 | `ws.Session` 是类型别名 | `session.Session` 是直接类型（包已公开） |

**迁移示例（v1 → v2）：**

```go
// v1
for msg := range sess.ReadChan() {
    sess.WriteChan() <- msg
}

// v2
type EchoHandler struct{}
func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*frame.Frame); ok && m.Opcode == frame.OpcodeText {
        ctx.Write(m)
    }
}
```

---

## 8. 测试策略

| 层级 | 测试方式 | 目标 |
|---|---|---|
| eventloop | mock Poller，测试 EventLoop 调度逻辑 | 事件分发正确 |
| buf | 单元测试引用计数、Slice、Pool | 无内存泄漏、零拷贝正确 |
| frame | 单元测试，构造字节序列验证解析/序列化 | RFC 6455 合规 |
| pipeline | mock Handler，测试链式调用 | Inbound/Outbound 顺序正确 |
| conn | net.Pipe 测试 netConn；mock Poller 测试 epollConn | 读写循环正确 |
| session | mock Conn + Pipeline，测试状态/心跳/重连 | 状态机正确 |
| hub | mock Session，测试注册/注销/广播 | map 操作正确 |
| 集成 | 真实 Server + Client，端到端 | 功能完整 |
| 压力 | 10万+ 连接，监控 goroutine / 内存 / CPU | 确认性能目标 |

---

## 9. V2 演进路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 接口化重构 + Pipeline + 事件驱动（epoll/kqueue） | 十万连接 |
| V2.1 | Windows IOCP 支持 + 时间轮心跳 | 跨平台完整 |
| V2.2 | goroutine 池化（业务计算池 + I/O 池分离） | 五十万连接 |
| V2.3 | sendfile / splice 零拷贝、SO_REUSEPORT | 百万连接 |

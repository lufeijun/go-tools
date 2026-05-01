# ws v2 — 架构设计与高并发哲学

本文档面向希望深入理解 `ws` v2 内部实现和优化逻辑的开发者。它详细阐述了分层架构、核心设计决策、双 I/O 模式体系、Acceptor 模式、性能优化路径以及高并发编程哲学。

---

## 1. 设计哲学

### 1.1 面向接口编程

`ws` v2 的每一层只依赖下层接口，不依赖具体实现：

- `session` 依赖 `conn.Conn` 和 `pipeline.ChannelPipeline`，但不知道 `netConn` 或 `epollConn`
- `eventloop` 只依赖 `EventHandler` 接口，`conn` 层实现它
- `frame` 层不依赖任何上层包，可独立使用
- `server` 只依赖 `Acceptor` 接口，不知道 `netAcceptor` 或 `epollAcceptor`

这种设计允许**逐层替换**：你可以只重写 `conn` 层接入自定义传输，或只重写 `pipeline` 层改变数据处理模型，或只替换 `Acceptor` 改变连接接入方式，而不影响其他层。

### 1.2 高并发核心原则

v2 的所有设计决策围绕五个高并发原则展开：

| 原则 | 含义 | 在 ws 中的体现 |
|---|---|---|
| **零 goroutine 空闲开销** | 空闲连接不绑定任何 goroutine | epollConn + EventLoop，百万空闲连接 ≈ CPU 核数个 goroutine |
| **零拷贝** | 数据在内存中只移动指针，不复制字节 | ByteBuf.Slice、Peek+Skip 帧解析、Retain/Release 广播 |
| **内存池化** | 高频分配的对象从池获取，降低 GC 压力 | ByteBuf 分级 sync.Pool、frame 内部 buffer 池 |
| **减少系统调用** | 一次 syscall 处理更多数据 | bufio 批量预读、WriteFrame 合并 header+payload 单次 write |
| **无锁/少锁** | 消除或缩小临界区 | Pipeline 原子指针遍历、EventLoop copy-on-write、Hub 分片锁 |

### 1.3 模式抽象

**同一套业务代码，两种 I/O 模型。** `ws` v2 最核心的设计哲学之一是：I/O 模型是部署选择，不是业务约束。

```
ws.Config{Mode: ws.ModeNet}    → netConn + goroutine-per-conn     → 全平台开发调试
ws.Config{Mode: ws.ModeEpoll}  → epollConn + EventLoop Reactor    → Linux 生产环境
```

- 业务层代码（Pipeline + Handler）完全不知道底层用的是 `netConn` 还是 `epollConn`
- `Pipeline.AddLast("echo", &EchoHandler{})` 在两种模式下行为一致
- 通过 `Acceptor` 接口和 `build-tag` 工厂，模式切换对上层透明
- `ValidateMode()` 在编译期不可做的平台检查，在运行时第一时间报错

### 1.4 借鉴 Netty，适配 Go

v2 引入 Pipeline/Handler/ByteBuf/Reactor 模型，但用 Go 的 goroutine 和 channel 替代 Java NIO 的线程模型：

- **Java NIO**：一个线程一个 EventLoop，Handler 在 EventLoop 线程同步执行
- **Go 模型**：EventLoop 只做事件感知和 fd 映射，Handler 通过 goroutine pool 异步调度，充分利用 Go 的轻量协程

---

## 2. 架构分层

### 2.1 包结构

```
ws/
├── eventloop/          # 跨平台事件驱动层
│   ├── eventloop.go         # EventLoop / Poller / EventHandler 接口 + 默认实现 + workerPool
│   ├── group.go             # EventLoopGroup 接口 + roundRobinEventLoopGroup
│   ├── epoll_linux.go       # Linux epoll 实现（NewEpollPoller 已导出）
│   └── kqueue_bsd.go        # BSD kqueue 实现
│
├── buf/                # 引用计数 ByteBuf
│   ├── bytebuf.go           # ByteBuf 接口 + 默认实现
│   ├── pool.go              # ByteBuf 分级对象池
│   └── *_bench_test.go      # 性能基准测试
│
├── frame/              # 协议层（公有，可独立使用）
│   ├── frame.go             # Frame 结构、ReadFrame、WriteFrame、零拷贝路径
│   ├── parser.go            # IncrementalParser 增量帧解析器
│   ├── pool.go              # Frame 内部 buffer 池
│   ├── mask.go              # 掩码处理（含原地 XOR）
│   └── *_bench_test.go      # 性能基准测试
│
├── pipeline/           # 处理器链（Netty 风格）
│   ├── handler.go           # ChannelHandler / InboundHandler / OutboundHandler / Context 接口
│   └── pipeline.go          # ChannelPipeline 实现（预编译数组 + 原子指针）
│
├── conn/               # 连接层
│   ├── conn.go              # Conn / EventDrivenConn 接口、Message、NextConnID
│   ├── netconn.go           # 标准 net.Conn 实现
│   ├── epollconn.go         # 事件驱动 Conn 实现（Linux 生产可用）
│   ├── conn_writer.go       # ConnWriter — Pipeline 头部 OutboundHandler
│   ├── codec.go             # FrameCodec（OutboundHandler）
│   ├── handshake.go         # RFC 6455 握手（基于 net.Conn）
│   ├── handshake_fd.go      # HandshakeFD 公共声明
│   ├── handshake_fd_linux.go      # ServerHandshakeFD / ClientHandshakeFD（Linux 实现）
│   ├── handshake_fd_nonlinux.go   # 非 Linux 桩函数
│   ├── dial.go              # 公共拨号声明
│   ├── dial_linux.go        # DialNonBlock（Linux 实现）
│   ├── dial_nonlinux.go     # DialNonBlock（非 Linux 桩函数）
│   └── tcp.go               # TCP 参数设置（SO_REUSEPORT、TCP_NODELAY 等）
│
├── session/            # 会话层
│   ├── session.go           # Session 接口 + 状态机实现（pub/sub StateChan）
│   ├── heartbeat.go         # Heartbeater 接口 + per-conn 心跳实现
│   ├── timingwheel.go       # 共享时间轮（单 goroutine 管理所有心跳）
│   └── reconnect.go         # Reconnector 接口 + 实现
│
├── hub/                # 连接管理中心
│   ├── hub.go               # Hub 接口 + 分片锁实现 + CloseAll + 缓存行对齐 + 固定 worker pool
│   └── hub_bench_test.go    # 广播性能基准测试
│
├── server/             # 服务端 API
│   ├── server.go            # Server + Acceptor 集成 + serveConn + 优雅关闭
│   ├── acceptor.go          # Acceptor 接口 + netAcceptor 实现
│   ├── acceptor_epoll_linux.go    # epollAcceptor 实现（Linux only）
│   ├── acceptor_factory_linux.go  # newAcceptorForMode（Linux build tag）
│   └── acceptor_factory_nonlinux.go  # newAcceptorForMode（非 Linux build tag）
│
├── client/             # 客户端 API
│   ├── client.go            # Client + 握手 + 自动重连
│   ├── epoll_linux.go       # epoll 模式客户端连接（Linux only）
│   └── epoll_nonlinux.go    # 非 Linux 桩函数
│
└── ws.go               # 根包：WSError、Config、IOMode、ValidateMode、DefaultConfig
```

### 2.2 分层依赖规则

```
server / client  →  session  →  pipeline  →  conn  →  eventloop
                                           ↘  frame + buf
```

**严格规则：**
1. 上层只能依赖下层接口
2. 不能跨层调用（如 `server` 不能直接调用 `frame`）
3. 不能反向依赖（如 `buf` 不知道 `pipeline` 的存在）
4. 根包 `ws` 不依赖任何子包

### 2.3 循环依赖解决

`eventloop.EventLoop.Register` 原本需要 `conn.Conn`，但 `conn.EventDrivenConn.SetEventLoop` 又需要 `eventloop.EventLoop`。

**解决方案：**
1. `EventLoop.Register` 只依赖 `EventHandler` 接口（`conn` 实现它）
2. `EventDrivenConn.SetEventLoop(el interface{})` 使用 `interface{}` 避免直接依赖 `eventloop` 包
3. `conn.EventHandlerAdapter` 适配 `EpollConn` 到 `eventloop.EventHandler`

---

## 3. 核心设计详解

### 3.1 eventloop — 事件驱动基石

#### 主从 Reactor 模型

```
┌─────────────────┐
│ MainEventLoop   │  ← 1 个，负责 Accept 新连接
│ (epoll/kqueue)  │
└────────┬────────┘
         │ 新连接
         ▼
┌─────────────────┐     ┌─────────────────┐
│ SubEventLoop 0  │     │ SubEventLoop 1  │  ← N 个（默认 N = CPU 核数）
│ (epoll/kqueue)  │ ... │ (epoll/kqueue)  │     每个管理一组连接的 I/O
└─────────────────┘     └─────────────────┘
         │                       │
         ▼                       ▼
   [worker pool]          [worker pool]    ← 异步调度 Handler，避免慢逻辑阻塞 Loop
```

#### EventLoopGroup

`EventLoopGroup` 管理一组 `EventLoop`，以 round-robin 策略将新连接分配到不同的 `EventLoop`：

```go
type EventLoopGroup interface {
    Start() error
    Stop() error
    Next() EventLoop  // round-robin 选择下一个 EventLoop
    Count() int       // 返回 EventLoop 数量
}
```

**实现：**

```go
type roundRobinEventLoopGroup struct {
    loops  []EventLoop
    nextFd uint64  // 原子递增，用于 round-robin
}

func (g *roundRobinEventLoopGroup) Next() EventLoop {
    n := atomic.AddUint64(&g.nextFd, 1)
    return g.loops[(n-1)%uint64(len(g.loops))]
}
```

- `NewEventLoopGroup(workers, newPoller)` 创建指定数量的 `EventLoop`，每个使用 `newPoller()` 创建独立的 `Poller`
- 在 `epollAcceptor` 中，`EventLoopGroup` 作为 sub Reactor 池，新连接通过 `elg.Next()` 分配到负载最低的 EventLoop
- `Start()` 启动所有 EventLoop 的 `Run()` goroutine；`Stop()` 依次停止

#### copy-on-write handler 查找

```go
type defaultEventLoop struct {
    handlersVal atomic.Value // stores map[int]EventHandler
}
```

- `Register` / `Deregister` 时复制新 map 原子替换
- `Run` 循环中通过 `atomic.Value.Load()` 获取只读快照，**零锁竞争**
- 相比 `sync.RWMutex`，消除了事件分发路径上的所有锁开销

#### goroutine pool 异步调度

```go
// EventLoop 只做事件感知 + fd 映射
for _, e := range events {
    h, ok := el.loadHandlers()[e.FD]
    if ok {
        // 投递到 worker pool，不阻塞 Loop
        el.pool.submit(func() {
            h.OnEvent(e.FD, e.Events)
        })
    }
}
```

- pool 大小默认 `GOMAXPROCS`，每个 worker 一个 goroutine
- 慢 Handler 不影响其他连接的事件响应
- `Stop()` 时优雅关闭 pool

#### 跨平台实现

| 平台 | 机制 | 编译标签 | 特性 |
|---|---|---|---|
| Linux | epoll | `//go:build linux` | EPOLLET 边缘触发、`NewEpollPoller()` 已导出 |
| macOS/FreeBSD/OpenBSD | kqueue | `//go:build darwin \|\| freebsd \|\| openbsd` | EVFILT_READ/WRITE |
| Windows | IOCP | `//go:build windows` | V2.1 实现 |

---

### 3.2 buf — 引用计数 ByteBuf

#### 为什么不用 Go 原生 `[]byte`？

Go 的 `[]byte` 在高并发场景下有三个问题：
1. **频繁分配** — 每次 `make` 都触发 GC，百万连接下 GC 压力巨大
2. **无法零拷贝传递** — 切片 `data[:n]` 共享底层数组但无法管理生命周期
3. **读写边界混乱** — 需要手动维护已读/未读偏移

#### ByteBuf 设计

```go
type byteBuf struct {
    data         []byte   // 底层数组
    readerIndex  int      // 读指针
    writerIndex  int      // 写指针
    refCount     int32    // 引用计数（原子操作）
    pool         Pool     // 所属对象池
}
```

#### 引用计数规则

| 操作 | refCount | 说明 |
|---|---|---|
| `NewByteBuf()` / `Pool.Get()` | `1` | 创建时初始化 |
| `Retain()` | `+1` | 传递给其他 goroutine 前调用 |
| `Slice()` | 原始 `+1`，新视图 `1` | 新视图依赖原始 buf |
| `Release()` | `-1` | 归零时回收到 Pool |

**安全保护：**
- `Release()` 后 `refCount < 0` → `panic`（double-free 保护）
- `Retain()` 时 `refCount <= 1` → `panic`（在已释放 buffer 上操作）

#### 分级对象池

```go
func NewPool(smallSize, defaultSize, largeSize int) Pool
```

| 级别 | 容量范围 | 默认对象数 | 用途 |
|---|---|---|---|
| small | `<= 512B` | 4096 | 控制帧、心跳帧 |
| default | `<= 4096B` | 1024 | 普通文本/二进制消息 |
| large | `<= 65536B` | 256 | 大消息、批量数据 |
| 直接分配 | `> 65536B` | — | 超大消息，GC 回收 |

**对齐策略：** `EnsureWritable` 按 512/4096/65536 对齐扩容，提高 Pool 命中率。

---

### 3.3 frame — RFC 6455 协议层

#### 安全加固：MaxFrameSize

```go
func ReadFrameLimit(r io.Reader, maxPayload int) (Frame, error)
```

- 读取 payload 长度后立即校验，超过 `maxPayload` 返回 `MaxFrameSizeError`
- 分片重组时累加已读长度，超过限制立即报错
- **防止恶意客户端发送极大长度帧头导致 OOM**

#### 零拷贝路径

**1. 写方向：`WriteFrameTo(dst ByteBuf, f Frame)`**

- 直接序列化到传入的 `ByteBuf`，无堆分配
- 配合 Pool 使用可实现零 alloc 写帧

**2. 读方向：`ReadFrameFromBuf(bb ByteBuf, maxPayload int)`**

- 使用 `Peek` + `Skip` 零拷贝解析帧头
- Payload 直接引用 `ByteBuf` 底层数组，无需 `make` 临时 slice
- 不完整帧时自动 `rewind` reader index

**3. 读方向：`ReadFrameBuf(r io.Reader, pool Pool, maxPayload int)`**

- 从 Pool 获取 `ByteBuf`，payload 直接写入底层数组
- 配合 `bufio.Reader` 批量预读，syscall 从 3-5 次降至 1 次

#### 原地 XOR 掩码

```go
func applyMaskInPlace(payload []byte, maskKey [4]byte)
```

- 服务端 Inbound 路径使用 `applyMaskInPlace`，直接修改 payload 所在数组
- 相比返回新切片的 `applyMask`，**每条入站消息减少一次 payload 级别的 alloc**

#### IncrementalParser — 增量帧解析器

`IncrementalParser` 是专为事件驱动 I/O 设计的非阻塞帧解析器，解决 `ReadFrame` 只能用于阻塞 I/O 的问题。

**用途：** 事件驱动模型中，数据以不完整的切片到达（如 epoll ET 模式下的 `unix.Read`），需要增量解析——每次收到数据追加到内部缓冲区，尝试解析出完整的帧。

**核心 API：**

```go
// 创建增量解析器，maxPayload 限制单帧/消息最大 payload 大小
parser := frame.NewIncrementalParser(maxPayload int)

// 追加数据并尝试解析，返回所有完整帧
frames := parser.Feed(data []byte) []Frame

// 获取最近一次解析错误
if parser.Err() != nil { /* 处理协议错误 */ }

// 重置内部状态，可复用
parser.Reset()
```

**分片处理：**

`IncrementalParser` 通过内部 `fragState` 跟踪 WebSocket 分片消息：

```go
type fragmentState struct {
    opcode  Opcode // 第一帧的 opcode（Text/Binary）
    payload []byte // 累积的 payload
}
```

- 收到非 FIN 帧：初始化或继续 `fragState`，累积 payload
- 收到 FIN 帧且有 `fragState`：合并所有 payload，设置 opcode 为第一帧的 opcode，清除 `fragState`
- 分片重组时同样受 `maxPayload` 限制，累积长度超限立即返回 `MaxFrameSizeError`

**MaxFrameSize 强制：**

`Feed` 在解析帧头后、读取 payload 前检查长度；分片重组时在每次追加 continuation 帧后检查累积长度。双重检查防止恶意帧绕过大小限制。

**与 ReadFrame 对比：**

| 特性 | `ReadFrame` | `IncrementalParser` |
|---|---|---|
| I/O 模型 | 阻塞 I/O（`io.Reader`） | 事件驱动 I/O（非阻塞 `[]byte`） |
| 数据来源 | `bufio.Reader` / `net.Conn` | `unix.Read` 返回的切片 |
| 不完整数据 | 阻塞等待更多数据 | 缓冲，下次 `Feed` 继续解析 |
| 适用场景 | `netConn` + `serveConn` 读循环 | `epollConn` + `handleReadEvent` |
| 分片处理 | 内联处理 | `fragState` 状态跟踪 |

---

### 3.4 pipeline — 处理器链

#### 为什么不用 channel？

v1 使用 channel 传递消息，每个连接 2-3 个 goroutine。v2 的目标是让空闲连接零 goroutine 开销，channel 模型无法做到。

Pipeline 在连接建立时组装，运行时事件遍历**不加锁**。

#### 预编译 handler 数组与 prevOutbound 机制

早期实现每次事件遍历双向链表，O(N) 指针跳转且破坏 CPU cache locality。

**优化后：**

```go
type handlerContext struct {
    nextInbound  atomic.Pointer[handlerContext] // 预缓存下一个 InboundHandler
    prevOutbound atomic.Pointer[handlerContext] // 预缓存左侧最近的 OutboundHandler
}
```

- `AddFirst` / `AddLast` / `Remove` 时调用 `rebuild()`，遍历链表预计算 `nextInbound` / `prevOutbound`
- 事件分发时通过 `atomic.Pointer.Load()` 直接获取下一个 Handler，**O(1) 且零锁**
- 并发安全的：`rebuild` 在 `mu.Lock()` 中写指针，事件遍历无锁读原子指针

#### prevOutbound 的关键设计

**prevOutbound 不是只对 OutboundHandler 有效，而是对 ALL handler 都设置。** `rebuild()` 算法从头到尾遍历所有 handler，对于每个 handler，`prevOutbound` 指向其左侧最近的 `OutboundHandler`：

```go
func (p *defaultPipeline) rebuild() {
    // ... nextInbound 计算 ...

    var lastOutbound *handlerContext
    for ctx := p.head.next; ctx != p.tail; ctx = ctx.next {
        ctx.prevOutbound.Store(lastOutbound)  // 每个 handler 都设置
        if _, ok := ctx.handler.(OutboundHandler); ok {
            lastOutbound = ctx  // 更新最近 OutboundHandler
        }
    }
    p.tail.prevOutbound.Store(lastOutbound)
}
```

**含义：** 任何 handler 调用 `ctx.Write(msg)` 时，通过 `prevOutbound` 能正确找到左侧最近的 `OutboundHandler`，无论调用者本身是 `InboundHandler` 还是 `OutboundHandler`。

**示例：** Pipeline `[ConnWriter(Outbound) → FrameCodec(Outbound) → BizHandler(Inbound-only)]`

- `rebuild()` 后：`BizHandler.prevOutbound = FrameCodec`，`FrameCodec.prevOutbound = ConnWriter`
- `BizHandler` 中调用 `ctx.Write(msg)` → `FrameCodec.Write(ctx, msg)` → `ctx.Write(bb)` → `ConnWriter.Write(ctx, bb)`

#### 数据流向

```
Inbound:  eventloop 读数据 → ByteBuf → Head → ConnWriter(skip) → FrameCodec(skip) → BizHandler
Outbound: BizHandler.ctx.Write(Message) → FrameCodec.Write → ConnWriter.Write → 底层 I/O
```

- `FireChannelRead` 从 Head 向 Tail 遍历 InboundHandler
- `FireChannelWrite` 从 Tail 向 Head 遍历 OutboundHandler
- `ctx.Write(msg)` 从当前 Handler 位置通过 `prevOutbound` 往回走 Outbound 链

---

### 3.5 conn — 连接层

#### 两种实现

| 特性 | netConn | epollConn |
|---|---|---|
| 平台 | 全平台 | Linux 生产可用 |
| goroutine | serveConn 读循环一个 goroutine | 零常驻 goroutine（EventLoop 驱动） |
| 适用场景 | 开发调试、非 Linux fallback | Linux 生产环境高并发 |
| TCP 参数 | 支持 NODELAY/QUICKACK/REUSEPORT | 相同 |
| 帧解析 | `frame.ReadFrameLimit`（阻塞 I/O） | `frame.IncrementalParser`（非阻塞） |
| 写入方式 | `net.Conn.Write`（阻塞） | `unix.Write` + EPOLLOUT（非阻塞） |
| 回调 | 无 | `SetOnFrame` / `SetOnClose` |

#### ConnWriter — Pipeline 头部 OutboundHandler

`ConnWriter` 是连接建立时自动添加到 Pipeline 头部的 `OutboundHandler`，负责将 `ByteBuf` 写入底层连接：

```go
type ConnWriter struct {
    Conn Conn
}

func (cw *ConnWriter) Write(ctx pipeline.Context, msg interface{}) {
    if bb, ok := msg.(buf.ByteBuf); ok {
        _ = cw.Conn.Write(bb)  // ByteBuf → Conn.Write
        return
    }
    ctx.FireChannelWrite(msg)  // 非 ByteBuf 消息继续传递
}
```

**添加方式：** 服务端和客户端在 `initSession` 中均执行：

```go
sess.Conn().Pipeline().AddFirst("headWriter", &conn.ConnWriter{Conn: c})
```

**作用：** 与 `FrameCodec` 配合，形成完整的 Outbound 链：

```
BizHandler.ctx.Write(*Message)
    → FrameCodec.Write: Message → Frame → WriteFrameTo(ByteBuf) → ctx.Write(bb)
    → ConnWriter.Write: ByteBuf → Conn.Write(bb)
    → netConn.conn.Write 或 EpollConn.flushWrite
```

#### FrameCodec 重构

`FrameCodec` 不再持有 `Writer` 字段，改为使用可选的 `Pool`：

```go
type FrameCodec struct {
    IsClient bool
    Pool     buf.Pool // optional; if nil a temporary ByteBuf is allocated
}
```

**Write 方法流程：**

1. `*Message` → `frame.Frame`（设置 FIN、Opcode、Masked 等）
2. `Frame` → `frame.WriteFrameTo(bb, f)` 序列化到 `ByteBuf`
3. `ctx.Write(bb)` 将 `ByteBuf` 传入 Outbound 链
4. `ConnWriter` 接收 `ByteBuf`，调用 `Conn.Write(bb)` 写入底层

**关键变化：** FrameCodec 不直接写 `io.Writer`，而是将 `ByteBuf` 通过 `ctx.Write` 传递给下游 `OutboundHandler`（即 `ConnWriter`）。这实现了写路径的统一：无论 `netConn` 还是 `epollConn`，`FrameCodec` 的代码完全一样。

#### EpollConn 完整生命周期

**创建与注册：**

```go
// 在 epollAcceptor.handleNewConn 中
ec := conn.NewEpollConn(handshakeFd, false, conn.NextConnID())
el := a.elg.Next()           // round-robin 选择 EventLoop
ec.SetEventLoop(el)           // 绑定 EventLoop
el.Register(handshakeFd, &conn.EventHandlerAdapter{Conn: ec})  // 注册到 epoll
```

**回调设置：**

```go
// EventDrivenConn 接口扩展
type EventDrivenConn interface {
    Conn
    FD() int
    OnEvent(events uint32)
    SetEventLoop(el interface{})
    SetOnFrame(fn func(frame.Frame))  // 帧回调
    SetOnClose(fn func())             // 关闭回调
}
```

- `SetOnFrame`：每解析出一个完整帧时调用，服务端在此分发 Text/Binary/Ping/Close
- `SetOnClose`：连接关闭时调用，用于通知上层清理状态

**handleReadEvent — 非阻塞读循环：**

```go
func (c *EpollConn) handleReadEvent() {
    tmp := make([]byte, 4096)
    for {
        n, err := unix.Read(c.fd, tmp)
        if n > 0 {
            if c.onFrame != nil && c.frameParser != nil {
                frames := c.frameParser.Feed(tmp[:n])  // 增量解析
                for _, f := range frames {
                    c.onFrame(f)  // 回调上层
                }
                if c.frameParser.Err() != nil {
                    c.Close()  // 协议错误，断开
                    return
                }
            }
        }
        if err != nil {
            if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
                return  // 数据读完了，等下次 EPOLLIN
            }
            c.Close()
            return
        }
        if n == 0 {
            c.Close()  // 对端关闭
            return
        }
    }
}
```

**flushWrite — 非阻塞写循环：**

```go
func (c *EpollConn) flushWrite() {
    c.writeMu.Lock()
    defer c.writeMu.Unlock()

    for len(c.writeBuf) > 0 {
        bb := c.writeBuf[0]
        data := bb.Bytes()
        n, err := unix.Write(c.fd, data[c.writeOff:])
        if n > 0 { c.writeOff += n }
        if err != nil {
            if err == unix.EAGAIN {
                c.registerWriteEvent()  // 注册 EPOLLOUT，写就绪时再写
                return
            }
            c.closeLocked()
            return
        }
        if c.writeOff >= len(data) {
            bb.Release()               // 引用计数 -1
            c.writeBuf = c.writeBuf[1:]
            c.writeOff = 0
        }
    }

    // 全部写完，移除 EPOLLOUT
    if c.el != nil {
        _ = c.el.Mod(c.fd, eventloop.EventRead)
    }
}
```

**closeLocked — 关闭流程：**

```go
func (c *EpollConn) closeLocked() {
    c.closeOnce.Do(func() {
        atomic.StoreInt32(&c.active, 0)
        if c.el != nil {
            _ = c.el.Deregister(c.fd)  // 从 epoll 注销
        }
        for _, bb := range c.writeBuf {
            bb.Release()               // 释放未发送的 ByteBuf
        }
        c.writeBuf = nil
        unix.Close(c.fd)               // 关闭 fd
        if c.onClose != nil {
            c.onClose()                // 通知上层
        }
    })
}
```

**EventHandlerAdapter 适配：**

```go
type EventHandlerAdapter struct {
    Conn *EpollConn
}

func (a *EventHandlerAdapter) OnEvent(fd int, events uint32) {
    a.Conn.OnEvent(events)  // 适配 EpollConn 到 EventHandler 接口
}
```

#### DialNonBlock — 非阻塞拨号（Linux only）

```go
// Linux 实现
func DialNonBlock(addr string) (int, error) {
    fd, err := unix.Socket(unix.AF_INET|unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
    err = unix.Connect(fd, sa)  // 返回 EINPROGRESS 表示正在连接
    return fd, nil
}

// 非 Linux 桩函数
func DialNonBlock(addr string) (int, error) {
    return -1, errors.New("DialNonBlock is only supported on Linux")
}
```

客户端在 epoll 模式下使用 `DialNonBlock` 创建非阻塞连接，然后用 `waitForConnect` 通过临时 epoll 实例等待连接完成。

#### HandshakeFD — 基于 fd 的 WebSocket 握手（Linux only）

**ServerHandshakeFD：**

```go
func ServerHandshakeFD(fd int) (int, error)
```

核心流程：
1. 临时设置 fd 为阻塞模式（`unix.SetNonblock(fd, false)`）
2. **关键：`unix.Dup(fd)` 复制 fd** — 隔离原始 fd 与 `os.NewFile` 的 GC finalizer
3. `os.NewFile` + `net.FileConn` 获取 `net.Conn`（`net.FileConn` 内部再 dup 一次）
4. 立即关闭 `*os.File`，释放 dup 的 fd
5. 使用 `net.Conn` 完成 HTTP 升级握手
6. `defer` 恢复非阻塞模式，关闭 `net.Conn`
7. 返回**原始 fd**（仍然是非阻塞的）

**为什么需要 Dup？** Go 的 `os.NewFile` 会注册 GC finalizer，当 `*os.File` 被回收时会自动关闭 fd。如果不 Dup，GC 可能在握手期间或之后关闭我们正在使用的 fd。通过 Dup，`os.NewFile` 拿到的是副本，GC 关闭副本不影响原始 fd。

**ClientHandshakeFD：**

```go
func ClientHandshakeFD(fd int, rawURL string, headers http.Header) error
```

流程类似：临时阻塞 → Dup → `net.FileConn` → 发送 HTTP 升级请求 → 读取 101 响应 → 恢复非阻塞。

#### TCP 优化

```go
// ServerHandshake 后自动设置
conn.ApplyTCPOptions(nc, cfg.TCPNoDelay, cfg.TCPQuickAck)

// SO_REUSEPORT 支持多进程负载均衡
conn.ListenTCPWithReusePort(addr)
```

| 参数 | 作用 |
|---|---|
| `TCP_NODELAY` | 关闭 Nagle 算法，小帧立即发送 |
| `TCP_QUICKACK` | Linux 禁用延迟确认，降低 RTT |
| `SO_REUSEPORT` | 多进程监听同一端口，内核做负载均衡 |

---

### 3.6 session — 会话层

#### 状态机

```
初始: Disconnected

      Connect() 被调用
            │
            ▼
      Connecting ──失败──► Reconnecting ◄─────┐
            │                │   ▲             │
            │ 成功           │   │ 重试间隔后  │ 达到 MaxReconnect
            ▼                │   │             │
       Connected ──断开──►──┘   └─────────────┘
            │                                    │
            │ 调用 Close()                       ▼
            ▼                              Closed
      Disconnected
```

#### StateChan — 发布/订阅模型

v2 的 `StateChan()` 不再返回单一 channel，而是采用**发布/订阅模型**：

```go
type defaultSession struct {
    conn   conn.Conn
    state  State
    mu     sync.Mutex
    subs   map[chan State]struct{}  // 所有订阅者
}
```

- **每次调用 `StateChan()` 创建一个新的订阅者 channel**（cap 16）
- **`SetState` 广播到所有订阅者**：遍历 `subs`，向每个 channel 发送状态
- **`Close` 向所有订阅者发送 `StateClosed` 并关闭所有 channel**
- **新订阅者立即收到当前状态**：创建 channel 后，如果当前状态不是 `Disconnected`，立即发送

```go
func (s *defaultSession) StateChan() <-chan State {
    ch := make(chan State, 16)
    s.mu.Lock()
    s.subs[ch] = struct{}{}
    if s.state != StateDisconnected {
        select {
        case ch <- s.state:  // 立即投递当前状态
        default:
        }
    }
    s.mu.Unlock()
    return ch
}
```

**使用场景：** epoll 模式下，`serveConn` goroutine 和 `setupEpollFrameCallback` 各自订阅 `StateChan`，互不干扰。

#### 心跳：per-conn 心跳实现

当前实现为 `PerConnHeartbeater`，每个 Session 使用独立的 ticker goroutine。未来可切换到共享时间轮方案以进一步减少 goroutine 数量。

#### 自动重连

客户端断开后按退避策略重试：

```
第 1 次: ReconnectInterval (默认 5s)
第 2 次: 10s
第 3 次: 20s
...
最大: 60s
```

- 达到 `MaxReconnect` 后进入 `StateClosed`，不再重试
- `Close()` 可中断重连过程
- 重连时根据 `Config.Mode` 选择 `doConnectNet` 或 `doConnectEpoll`

---

### 3.7 hub — 连接管理中心

#### 分片锁 + 缓存行对齐

```go
type shard struct {
    mu    sync.RWMutex
    conns map[uint64]session.Session
    _     [cacheLineSize - int(unsafe.Sizeof(sync.RWMutex{})) - int(unsafe.Sizeof(map[uint64]session.Session{}))]byte
}
```

- 32 个 shard 分布在不同的 CPU cache line 上
- 高并发广播时，不同核访问不同 shard 不会因 cache line 共享产生 false sharing

#### 固定 broadcast worker pool

预置固定 worker pool，每个 worker 负责一个 shard：

- `Broadcast()` 将消息投递到 shard 的 buffered channel（256 msg）
- 无 goroutine 创建/销毁开销
- 支持背压：channel 满时直接跳过慢 shard

#### CloseAll — 优雅关闭

```go
func (h *shardedHub) CloseAll() {
    for _, sh := range h.shards {
        sh.mu.RLock()
        sessions := make([]session.Session, 0, len(sh.conns))
        for _, sess := range sh.conns {
            sessions = append(sessions, sess)
        }
        sh.mu.RUnlock()

        for _, sess := range sessions {
            sess.Close()
        }
    }
}
```

- 遍历所有 shard，逐个关闭 session
- 被 `Server.Stop()` 调用，确保所有连接在服务停止前被清理

#### 操作性能

| 操作 | 时间复杂度 | 锁范围 |
|---|---|---|
| `Register` | O(1) | 单个 shard 写锁 |
| `Unregister` | O(1) | 单个 shard 写锁 |
| `Get` | O(1) | 单个 shard 读锁 |
| `Count` | O(shards) | 所有 shard 读锁（非精确） |
| `Broadcast` | O(shards + conns) | 先读锁拷贝，再无锁发送 |
| `CloseAll` | O(shards + conns) | 先读锁拷贝，再逐个 Close |

---

## 4. 双 I/O 模式架构

### 4.1 模式定义

```go
// ws.go
type IOMode int

const (
    ModeNet   IOMode = iota // goroutine-per-conn，全平台（默认）
    ModeEpoll                // 事件驱动 Reactor，Linux only
)
```

- `ModeNet`（默认值 0）：使用 `netConn` + `net/http` + goroutine-per-conn，全平台可用
- `ModeEpoll`（值 1）：使用 `epollConn` + 原始 fd + epoll Reactor，仅 Linux

### 4.2 模式校验

```go
func (c Config) ValidateMode() error {
    if c.Mode == ModeEpoll && runtime.GOOS != "linux" {
        return fmt.Errorf("epoll mode is only supported on Linux, current OS: %s", runtime.GOOS)
    }
    return nil
}
```

- `NewServer(cfg)` 和 `NewClient(cfg)` 在创建时调用 `ValidateMode()`
- 在非 Linux 平台选择 `ModeEpoll` 立即返回错误，避免运行时崩溃

### 4.3 Config.Mode 字段

```go
type Config struct {
    // ...
    Mode IOMode  // 选择 I/O 模式
}
```

`DefaultConfig()` 中 `Mode` 为零值，即 `ModeNet`。

### 4.4 模式选择流转

```
Config.Mode
    │
    ▼
NewServer(cfg) / NewClient(cfg)
    │
    ├── cfg.ValidateMode() → 非法模式直接返回 error
    │
    ▼
newAcceptorForMode(cfg)  ← build-tag 工厂函数
    │
    ├── Linux: ModeEpoll → newEpollAcceptor(cfg)
    │          ModeNet   → newNetAcceptor(cfg)
    │
    └── 非 Linux: 始终 → newNetAcceptor(cfg)
```

**服务端：** Acceptor 类型决定连接建立方式：
- `netAcceptor`：`http.Server` + Hijack → `netConn`
- `epollAcceptor`：原始 fd + `ServerHandshakeFD` → `epollConn` + EventLoopGroup

**客户端：** `doConnect()` 根据 `Config.Mode` 分发：
- `ModeNet`：`conn.ClientHandshake` + `net.Conn` → `netConn`
- `ModeEpoll`：`conn.DialNonBlock` + `conn.ClientHandshakeFD` → `epollConn` + EventLoopGroup

---

## 5. Acceptor 模式

### 5.1 Acceptor 接口

```go
type Acceptor interface {
    Listen(addr string) error
    Accept() (conn.Conn, net.Conn, error)  // net.Conn 在 epoll 模式下为 nil
    Close() error
}
```

- `Accept()` 返回两个连接对象：`conn.Conn`（统一接口）和 `net.Conn`（net 模式专用，epoll 模式为 nil）
- `Close()` 停止接收新连接并释放资源

### 5.2 netAcceptor — 标准 HTTP 升级

```go
type netAcceptor struct {
    config   ws.Config
    server   *http.Server
    listener net.Listener
    connCh   chan *acceptedConn  // 缓冲 128
}
```

**工作流程：**

1. `Listen()`：创建 `http.Server`，注册 `handleUpgrade` 路由，启动 `Serve`
2. `handleUpgrade`：调用 `conn.ServerHandshake(w, r)` 完成 HTTP → WebSocket 升级
3. 创建 `netConn`，通过 `connCh` 传递给 `Accept()`
4. `Accept()`：从 `connCh` 取出连接，返回给 `acceptLoop`
5. `Close()`：关闭 `http.Server`，关闭 `connCh`

**特点：** 利用 Go 标准库的 `http.Hijack` 获取底层 `net.Conn`，全平台可用。

### 5.3 epollAcceptor — 原始 fd + 主从 Reactor（Linux only）

```go
type epollAcceptor struct {
    config    ws.Config
    listenFd  int
    addr      net.TCPAddr
    connCh    chan *acceptedConn
    elg       eventloop.EventLoopGroup    // sub Reactor 池
    mainLoop  eventloop.EventLoop          // main Reactor
    running   int32
    closeOnce sync.Once
}
```

**工作流程：**

1. `Listen()`：
   - `unix.Socket` 创建非阻塞 TCP socket
   - `unix.Bind` + `unix.Listen` 绑定监听
   - 创建 `EventLoopGroup`（sub Reactor 池）并启动
   - 创建独立的 `mainLoop`（main Reactor），注册 listenFd 的 `EPOLLIN`
   - 启动 `mainLoop.Run()` goroutine

2. `acceptHandler.OnEvent`（main Reactor 回调）：
   - 循环调用 `unix.Accept`（ET 模式需全部取完）
   - 对每个新 fd 调用 `handleNewConn`

3. `handleNewConn`：
   - `conn.ServerHandshakeFD(clientFd)` 完成 WebSocket 握手
   - 设置 TCP 参数（NODELAY/QUICKACK）
   - 创建 `EpollConn`
   - `elg.Next()` 选择 EventLoop，注册 fd
   - 通过 `connCh` 传递给 `Accept()`

4. `Close()`：停止 mainLoop、EventLoopGroup、关闭 listenFd

```
┌────────────────────┐
│   mainLoop         │  listenFd EPOLLIN → acceptHandler.OnEvent
│   (main Reactor)   │  → unix.Accept 循环 → handleNewConn
└─────────┬──────────┘
          │ 新连接 fd
          ▼
┌────────────────────┐
│   EventLoopGroup   │  elg.Next() round-robin 分配
│   (sub Reactor 池) │
│  ┌────┐ ┌────┐    │
│  │ EL0│ │ EL1│ ...│  每个 EL 管理一组连接的 I/O
│  └────┘ └────┘    │
└────────────────────┘
```

### 5.4 Build-tag 工厂

**acceptor_factory_linux.go：**

```go
//go:build linux

func newAcceptorForMode(cfg ws.Config) Acceptor {
    switch cfg.Mode {
    case ws.ModeEpoll:
        return newEpollAcceptor(cfg)
    default:
        return newNetAcceptor(cfg)
    }
}
```

**acceptor_factory_nonlinux.go：**

```go
//go:build !linux

func newAcceptorForMode(cfg ws.Config) Acceptor {
    return newNetAcceptor(cfg)  // 非 Linux 只有 netAcceptor
}
```

- 编译时根据平台选择工厂实现
- Linux 平台支持两种模式；非 Linux 平台只能使用 `netAcceptor`
- `ValidateMode()` 作为运行时安全网，防止非 Linux 平台选择 `ModeEpoll`

---

## 6. 性能优化全景

### 6.1 优化清单与实现状态

| 优先级 | 编号 | 优化项 | 状态 | 说明 |
|---|---|---|---|---|
| P0 | 4.1 | ReadFrame MaxFrameSize 校验 | ✅ | 防 DoS，恶意大帧直接拒绝 |
| P0 | 4.2 | 分片重组累积长度上限 | ✅ | 防止无限 continuation 帧耗尽内存 |
| P0 | 4.6 | Hub.Send / Broadcast 实现 | ✅ | 通过 Pipeline.FireChannelWrite 发送 |
| P0 | 5.1 | 心跳功能 | ✅ | PerConnHeartbeater 实现 |
| P0 | 3.1 | epollConn 非阻塞 I/O | ✅ | Linux 生产可用 |
| P0 | 3.2 | TCP 参数实际生效 | ✅ | NODELAY/QUICKACK/REUSEPORT |
| P1 | 4.3 | Handshake 超时 | ✅ | 读循环带 deadline |
| P1 | 4.4 | Close 帧 RFC 合规 | ✅ | 收到 Close 后回写 Close 帧再断开 |
| P1 | 4.5 | stateChan / serveConn 泄漏 | ✅ | Close() 关闭所有订阅者 channel |
| P1 | 3.3 | 读缓冲批量预读（bufio） | ✅ | serveConn 使用 64KB bufio.Reader |
| P1 | 1.1 | 心跳时间轮 | ✅ | 128 slots，1s tick |
| P1 | 2.1 | netConn.Read 池化 | 🔄 | V2.1 配合 ByteBuf 底层数组直接读 |
| P1 | 2.3 | frame 层 ByteBuf 化 | ✅ | ReadFrameBuf / WriteFrameTo |
| P2 | 5.2 | 移除 serveConn goroutine | ✅ | epoll 模式下已实现 |
| P2 | 5.3 | Hub shard false sharing | ✅ | cache line padding |
| P2 | 1.2 | Hub broadcast worker pool | ✅ | 固定 goroutine + buffered channel |
| P2 | 1.4 | EventLoop handler 查找优化 | ✅ | atomic.Value copy-on-write |
| P2 | 3.4 | 写合并 | 🔄 | V2.1 配合 EPOLLOUT 优化 |
| P2 | 5.4 | Pipeline ExceptionCaught | 🔄 | 默认异常传播链 V2.1 完善 |
| P2 | 5.5 | EventLoop.Stop 资源清理 | ✅ | Deregister 所有 fd + poller.Close |
| P2 | 5.6 | 客户端自动重连 | ✅ | 指数退避 5s→2x→60s |
| P3 | 1.3 | EventLoop 异步 dispatch | ✅ | goroutine pool |
| P3 | 1.5 | Pipeline 预编译数组 | ✅ | atomic.Pointer 缓存 next/prev |
| P3 | 2.2 | Peek+Skip 零拷贝帧解析 | ✅ | ReadFrameFromBuf |
| P3 | 2.4 | mask 原地 XOR | ✅ | applyMaskInPlace |
| P3 | 2.5 | ByteBuf 扩容对齐 | ✅ | 512/4096/65536 对齐 |
| P3 | 5.7 | benchmark 基线 | ✅ | frame/buf/hub/pipeline |
| P3 | 5.8 | ByteBuf 线程安全文档 | ✅ | 包注释明确声明 |

### 6.2 关键优化详解

#### 优化 1：Pipeline 预编译 handler 数组

**问题：** 每次事件遍历双向链表，O(N) 指针跳转，cache miss 严重。

**方案：** `rebuild()` 在 Add/Remove 时预计算每个节点的 `nextInbound` / `prevOutbound`，存储在 `atomic.Pointer` 中。

**收益：** 事件分发从链表遍历变为 O(1) 原子加载，零锁竞争。

#### 优化 2：EventLoop 异步 dispatch

**问题：** 慢 Handler 卡在 EventLoop 主 goroutine，影响其他连接。

**方案：** 引入固定 goroutine pool，`OnEvent` 投递到 pool 异步执行。

**收益：** 单个慢 Handler 不影响其他连接的事件响应。

#### 优化 3：Hub broadcast worker pool

**问题：** 每次 `Broadcast()` 创建 32 个 goroutine，高频广播时创建/销毁开销大。

**方案：** 预置固定 worker，每个负责一个 shard，消息投递到 buffered channel。

**收益：** 消除广播 goroutine 创建开销，支持背压。

#### 优化 4：心跳时间轮

**问题：** 10 万连接 = 10 万个 ticker goroutine。

**方案：** 128 slots 单级时间轮，1 个 goroutine 管理所有心跳超时。

**收益：** 10 万连接的心跳 goroutine 从 10 万降至 1 个。

#### 优化 5：frame 零拷贝

**问题：** `ReadFrame` 每次 `make` payload slice，`WriteFrame` 每次 `make` header buffer。

**方案：**
- `ReadFrameFromBuf`：Peek+Skip 零拷贝解析帧头
- `ReadFrameBuf`：从 Pool 获取 ByteBuf，payload 直接写入底层数组
- `WriteFrameTo`：直接序列化到传入的 ByteBuf
- `applyMaskInPlace`：原地 XOR，不分配新 slice

**收益：** 帧解析/序列化路径零堆分配（Pool 命中时）。

#### 优化 6：ByteBuf 扩容对齐

**问题：** `EnsureWritable` 翻倍扩容，频繁写入时多次触发 copy。

**方案：** 扩容按 512/4096/65536 对齐，利于 Pool 回收。

**收益：** 减少扩容 copy 次数，提高 Pool 命中率。

---

## 7. 数据流

### 7.1 服务端收消息 — net 模式（Inbound）

```
Client TCP 帧
    │
    ▼
[bufio.Reader 64KB] 批量预读到缓冲
    │
    ▼
[frame.ReadFrameLimit] 从缓冲解析帧（3-5 次内存读，0 次 syscall）
    │
    ▼
[server.serveConn] Text/Binary → conn.Message
    │
    ▼
[Pipeline.FireChannelRead] 触发 Inbound 链（原子指针 O(1) 遍历）
    │
    ├── [ConnWriter] Inbound → skip（不是 InboundHandler）
    │
    ├── [FrameCodec] Inbound → skip（不是 InboundHandler）
    │
    └── [BizHandler] 业务逻辑处理
```

### 7.2 服务端收消息 — epoll 模式（Inbound）

```
Client TCP 帧
    │
    ▼
[EventLoop epoll_wait EPOLLIN]
    │
    ▼
[EpollConn.OnEvent] → handleReadEvent
    │
    ▼
[非阻塞读循环 unix.Read] → IncrementalParser.Feed
    │
    ▼
[IncrementalParser] 返回完整 Frame
    │
    ▼
[onFrame 回调] Frame → conn.Message
    │
    ▼
[Pipeline.FireChannelRead] 触发 Inbound 链
    │
    └── [BizHandler] 业务逻辑处理
```

### 7.3 服务端发消息 — 两种模式统一（Outbound）

```
BizHandler.ctx.Write(*Message)
    │
    ▼
[FrameCodec.Write] Message → Frame → WriteFrameTo(ByteBuf) → ctx.Write(bb)
    │
    ▼
[ConnWriter.Write] ByteBuf → Conn.Write(bb)
    │
    ├── netConn:  conn.Write(bb) → net.Conn.Write
    │
    └── epollConn: Conn.Write(bb) → writeBuf 入队 → flushWrite
                        │
                        ├── unix.Write 成功 → ByteBuf.Release()
                        │
                        └── EAGAIN → 注册 EPOLLOUT → 写就绪后再 flushWrite
```

### 7.4 广播

```
Hub.Broadcast(Message)
    │
    ▼
[shardedHub] 遍历所有 worker
    │
    ├── worker[0]: 消息投递到 ch → worker goroutine → RLock 拷贝 → RUnlock → 逐个 FireChannelWrite
    ├── worker[1]: 消息投递到 ch → ...
    └── ...
```

### 7.5 优雅关闭

```
Server.Stop()
    │
    ▼
[Acceptor.Close()]
    ├── netAcceptor: http.Server.Close() + close(connCh)  → 停止接收新连接
    └── epollAcceptor: mainLoop.Stop() + elg.Stop() + close(listenFd) + close(connCh)
    │
    ▼
[Hub.CloseAll()]
    └── 遍历所有 shard → 逐个 sess.Close()  → 关闭所有已建立的连接
    │
    ▼
[sync.WaitGroup.Wait()]
    └── 等待所有 serveConn goroutine 退出
```

---

## 8. 安全与正确性

### 8.1 DoS 防护

| 攻击向量 | 防御措施 |
|---|---|
| 超大单帧 | `MaxFrameSize` 限制，超限返回错误并断开 |
| 无限分片 | 分片重组累积长度上限，超限断开（`IncrementalParser.Feed` 中检查） |
| 慢速握手 | handshake 后设置读 deadline |
| 慢速读 | serveConn 带 read deadline，心跳超时兜底 |
| 广播洪泛 | Hub 发送非阻塞，channel 满直接跳过 |

### 8.2 资源泄漏防护

| 资源 | 防护措施 |
|---|---|
| goroutine | 时间轮/PerConnHeartbeater 管理；EventLoop pool 固定大小 |
| channel | `Close()` 关闭所有 `StateChan` 订阅者；`Stop()` 关闭 `stopCh` |
| fd | `Stop()` Deregister 所有 fd + `poller.Close()`；`closeLocked` 关闭 fd |
| ByteBuf | 引用计数 + double-free panic；`closeLocked` 释放 writeBuf 中所有 ByteBuf |
| os.File finalizer | `HandshakeFD` 使用 `unix.Dup` 隔离原始 fd，防止 GC 关闭 |

---

## 9. 跨平台策略

| 平台 | 事件驱动 | 服务端 Acceptor | 客户端 | 状态 |
|---|---|---|---|---|
| Linux | epoll | netAcceptor + epollAcceptor | net + epoll 模式 | 主力平台，完整实现 |
| macOS / FreeBSD / OpenBSD | kqueue | netAcceptor | net 模式 | 开发调试，完整实现 |
| Windows | IOCP | netAcceptor | net 模式 | V2.1 实现 |
| 其他 | — | netAcceptor | net 模式 | 功能完整，性能受限 |

**build-tag 策略：**
- `//go:build linux` — epoll 实现（`epoll_linux.go`、`acceptor_epoll_linux.go`、`dial_linux.go`、`handshake_fd_linux.go`、`epoll_linux.go`）
- `//go:build !linux` — 桩函数/回退实现（返回错误或空实现）
- `//go:build darwin || freebsd || openbsd` — kqueue 实现

---

## 10. 测试策略

| 层级 | 方式 | 目标 |
|---|---|---|
| eventloop | mock Poller + race 检测 | 事件分发正确，无 data race |
| eventloop group | 单元测试 | round-robin 分配正确，Start/Stop 生命周期 |
| buf | 单元测试 + benchmark | 引用计数、Slice、Pool、性能基线 |
| frame | 单元测试 + benchmark | RFC 6455 合规、零拷贝路径、性能基线 |
| frame/parser | 单元测试 | IncrementalParser 增量解析、分片重组、MaxFrameSize |
| pipeline | mock Handler + race 检测 | 链式调用正确、prevOutbound 机制、并发 Add/Remove 安全 |
| conn | `net.Pipe` + 接口测试 | 读写、握手、codec、ConnWriter |
| conn/epollconn | Linux 专属测试 | 非阻塞读写、flushWrite、EPOLLOUT、closeLocked |
| conn/handshake_fd | Linux 专属测试 | ServerHandshakeFD/ClientHandshakeFD、Dup 隔离 |
| session | mock Conn + Pipeline | 状态机、StateChan pub/sub、心跳、重连 |
| hub | mock Session + benchmark | 分片锁正确、广播性能、CloseAll |
| server | 集成测试 | net/epoll 双模式功能、Accept 生命周期、优雅关闭 |
| 集成 | 真实 Server + Client | 功能完整 |

---

## 11. 演进路线

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 双 I/O 模式 + Acceptor 模式 + EpollConn 生产可用 + Pipeline/Handler + 时间轮心跳 + 优化清单 P0/P1/P2/P3 | 十万连接（epoll 模式） |
| V2.1 | Windows IOCP + kqueue-based Acceptor（BSD） + 写合并优化 + Pipeline ExceptionCaught 完善 | 跨平台完整 |
| V2.2 | goroutine 池化精细调优 + 业务计算池分离 + netConn.Read 池化 | 五十万连接 |
| V2.3 | 内核旁路（DPDK/AF_XDP）调研 | 百万连接 |

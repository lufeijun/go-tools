# ws v2 — 架构设计与高并发哲学

本文档面向希望深入理解 `ws` v2 内部实现和优化逻辑的开发者。它详细阐述了分层架构、核心设计决策、性能优化路径以及高并发编程哲学。

---

## 1. 设计哲学

### 1.1 面向接口编程

`ws` v2 的每一层只依赖下层接口，不依赖具体实现：

- `session` 依赖 `conn.Conn` 和 `pipeline.ChannelPipeline`，但不知道 `netConn` 或 `epollConn`
- `eventloop` 只依赖 `EventHandler` 接口，`conn` 层实现它
- `frame` 层不依赖任何上层包，可独立使用

这种设计允许**逐层替换**：你可以只重写 `conn` 层接入自定义传输，或只重写 `pipeline` 层改变数据处理模型，而不影响其他层。

### 1.2 高并发核心原则

v2 的所有设计决策围绕五个高并发原则展开：

| 原则 | 含义 | 在 ws 中的体现 |
|---|---|---|
| **零 goroutine 空闲开销** | 空闲连接不绑定任何 goroutine | epollConn + EventLoop，百万空闲连接 ≈ CPU 核数个 goroutine |
| **零拷贝** | 数据在内存中只移动指针，不复制字节 | ByteBuf.Slice、Peek+Skip 帧解析、Retain/Release 广播 |
| **内存池化** | 高频分配的对象从池获取，降低 GC 压力 | ByteBuf 分级 sync.Pool、frame 内部 buffer 池 |
| **减少系统调用** | 一次 syscall 处理更多数据 | bufio 批量预读、WriteFrame 合并 header+payload 单次 write |
| **无锁/少锁** | 消除或缩小临界区 | Pipeline 原子指针遍历、EventLoop copy-on-write、Hub 分片锁 |

### 1.3 借鉴 Netty，适配 Go

v2 引入 Pipeline/Handler/ByteBuf/Reactor 模型，但用 Go 的 goroutine 和 channel 替代 Java NIO 的线程模型：

- **Java NIO**：一个线程一个 EventLoop，Handler 在 EventLoop 线程同步执行
- **Go 模型**：EventLoop 只做事件感知和 fd 映射，Handler 通过 goroutine pool 异步调度，充分利用 Go 的轻量协程

---

## 2. 架构分层

### 2.1 包结构

```
ws/
├── eventloop/          # 跨平台事件驱动层
│   ├── eventloop.go         # EventLoop / Poller / EventHandler 接口 + 默认实现
│   ├── epoll_linux.go       # Linux epoll 实现
│   └── kqueue_bsd.go        # BSD kqueue 实现
│
├── buf/                # 引用计数 ByteBuf
│   ├── bytebuf.go           # ByteBuf 接口 + 默认实现
│   ├── pool.go              # ByteBuf 分级对象池
│   └── *_bench_test.go      # 性能基准测试
│
├── frame/              # 协议层（公有，可独立使用）
│   ├── frame.go             # Frame 结构、ReadFrame、WriteFrame、零拷贝路径
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
│   ├── epollconn.go         # 事件驱动 Conn 实现（V2.1 完善非阻塞读写）
│   ├── handshake.go         # RFC 6455 握手
│   ├── codec.go             # FrameCodec（OutboundHandler）
│   └── tcp.go               # TCP 参数设置（SO_REUSEPORT、TCP_NODELAY 等）
│
├── session/            # 会话层
│   ├── session.go           # Session 接口 + 状态机实现
│   ├── heartbeat.go         # Heartbeater 接口 + 时间轮实现
│   ├── timingwheel.go       # 共享时间轮（单 goroutine 管理所有心跳）
│   └── reconnect.go         # Reconnector 接口 + 实现
│
├── hub/                # 连接管理中心
│   ├── hub.go               # Hub 接口 + 分片锁实现（缓存行对齐 + 固定 worker pool）
│   └── hub_bench_test.go    # 广播性能基准测试
│
├── server/             # 服务端 API
│   └── server.go            # Server + 握手处理 + serveConn 读循环
│
├── client/             # 客户端 API
│   └── client.go            # Client + 握手 + 自动重连
│
└── ws.go               # 根包：WSError、Config、DefaultConfig
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
| Linux | epoll | `//go:build linux` | EPOLLET 边缘触发、eventfd 唤醒（V2.1） |
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

---

### 3.4 pipeline — 处理器链

#### 为什么不用 channel？

v1 使用 channel 传递消息，每个连接 2-3 个 goroutine。v2 的目标是让空闲连接零 goroutine 开销，channel 模型无法做到。

Pipeline 在连接建立时组装，运行时事件遍历**不加锁**。

#### 预编译 handler 数组

早期实现每次事件遍历双向链表，O(N) 指针跳转且破坏 CPU cache locality。

**优化后：**

```go
type handlerContext struct {
    nextInbound  atomic.Pointer[handlerContext] // 预缓存下一个 InboundHandler
    prevOutbound atomic.Pointer[handlerContext] // 预缓存上一个 OutboundHandler
}
```

- `AddFirst` / `AddLast` / `Remove` 时调用 `rebuild()`，遍历链表预计算 `nextInbound` / `prevOutbound`
- 事件分发时通过 `atomic.Pointer.Load()` 直接获取下一个 Handler，**O(1) 且零锁**
- 并发安全的：`rebuild` 在 `mu.Lock()` 中写指针，事件遍历无锁读原子指针

#### 数据流向

```
Inbound:  eventloop 读数据 → ByteBuf → Head → FrameCodec → BizHandler → Tail
Outbound: BizHandler.Write → FrameCodec → Head.Write → eventloop 写数据
```

- `FireChannelRead` 从 Head 向 Tail 遍历 InboundHandler
- `FireChannelWrite` 从 Tail 向 Head 遍历 OutboundHandler
- `ctx.Write(msg)` 从当前 Handler 位置往回走 Outbound 链

---

### 3.5 conn — 连接层

#### 两种实现

| 特性 | netConn | epollConn |
|---|---|---|
| 平台 | 全平台 | Linux（V2.1 完善） |
| goroutine | serveConn 读循环一个 goroutine | 零常驻 goroutine |
| 适用场景 | 开发调试、fallback | 生产环境高并发 |
| TCP 参数 | 支持 NODELAY/QUICKACK/REUSEPORT | 相同 |

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

#### FrameCodec

`FrameCodec` 是 `OutboundHandler`，将 `*conn.Message` 编码为 WebSocket 帧：

- 服务端 `Masked = false`
- 客户端 `Masked = true`，自动生成 `MaskKey`
- 写入完成后继续往前传（`FireChannelWrite`），供其他 OutboundHandler 处理

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

- `StateChan()` 容量 16，缓冲最近状态变化，满时丢弃旧状态
- `Close()` 直接设置 `StateClosed` 并关闭 channel，防止 goroutine 泄漏

#### 心跳：共享时间轮

**v1 问题：** 每个 Session 一个 `time.Ticker` goroutine，10 万连接 = 10 万个 ticker。

**v2 优化：** 单级时间轮（128 slots，1s tick），一个 goroutine 管理所有心跳：

```go
type TimingWheel struct {
    tickMs    time.Duration
    wheelSize int
    buckets   []bucket
    taskIDSeq uint64
}
```

- `Add(delay, callback)` 注册超时任务，返回 `taskID`
- `Cancel(taskID)` 取消任务
- 每个 Session 的 heartbeat 只注册/取消时间轮节点，不创建 goroutine
- **10 万连接的心跳 goroutine 从 10 万降至 1 个**

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

---

### 3.7 hub — 连接管理中心

#### 分片锁 + 缓存行对齐

```go
type shard struct {
    mu    sync.RWMutex
    _pad  [56]byte        // cache line padding（64 字节对齐）
    conns map[uint64]session.Session
}
```

- 32 个 shard 分布在不同的 CPU cache line 上
- 高并发广播时，不同核访问不同 shard 不会因 cache line 共享产生 false sharing

#### 固定 broadcast worker pool

早期实现每次 `Broadcast()` 创建 32 个临时 goroutine。

**优化后：** 预置固定 worker pool，每个 worker 负责一个 shard：

- `Broadcast()` 将消息投递到 shard 的 buffered channel（256 msg）
- 无 goroutine 创建/销毁开销
- 支持背压：channel 满时直接跳过慢 shard

#### 操作性能

| 操作 | 时间复杂度 | 锁范围 |
|---|---|---|
| `Register` | O(1) | 单个 shard 写锁 |
| `Unregister` | O(1) | 单个 shard 写锁 |
| `Get` | O(1) | 单个 shard 读锁 |
| `Count` | O(shards) | 所有 shard 读锁（非精确） |
| `Broadcast` | O(shards + conns) | 先读锁拷贝，再无锁发送 |

---

## 4. 性能优化全景

### 4.1 优化清单与实现状态

| 优先级 | 编号 | 优化项 | 状态 | 说明 |
|---|---|---|---|---|
| P0 | 4.1 | ReadFrame MaxFrameSize 校验 | ✅ | 防 DoS，恶意大帧直接拒绝 |
| P0 | 4.2 | 分片重组累积长度上限 | ✅ | 防止无限 continuation 帧耗尽内存 |
| P0 | 4.6 | Hub.Send / Broadcast 实现 | ✅ | 通过 Pipeline.FireChannelWrite 发送 |
| P0 | 5.1 | 心跳功能（时间轮） | ✅ | 单 goroutine 管理所有心跳 |
| P0 | 3.1 | epollConn 非阻塞 I/O | 🔄 | V2.1 交付 |
| P0 | 3.2 | TCP 参数实际生效 | ✅ | NODELAY/QUICKACK/REUSEPORT |
| P1 | 4.3 | Handshake 超时 | ✅ | 读循环带 deadline |
| P1 | 4.4 | Close 帧 RFC 合规 | ✅ | 收到 Close 后回写 Close 帧再断开 |
| P1 | 4.5 | stateChan / serveConn 泄漏 | ✅ | Close() 关闭 channel，读循环带 deadline |
| P1 | 3.3 | 读缓冲批量预读（bufio） | ✅ | serveConn 使用 64KB bufio.Reader |
| P1 | 1.1 | 心跳时间轮 | ✅ | 128 slots，1s tick |
| P1 | 2.1 | netConn.Read 池化 | 🔄 | V2.1 配合 ByteBuf 底层数组直接读 |
| P1 | 2.3 | frame 层 ByteBuf 化 | ✅ | ReadFrameBuf / WriteFrameTo |
| P2 | 5.2 | 移除 serveConn goroutine | 🔄 | 需 epollConn 完成后移除 |
| P2 | 5.3 | Hub shard false sharing | ✅ | cache line padding |
| P2 | 1.2 | Hub broadcast worker pool | ✅ | 固定 goroutine + buffered channel |
| P2 | 1.4 | EventLoop handler 查找优化 | ✅ | atomic.Value copy-on-write |
| P2 | 3.4 | 写合并 | 🔄 | V2.1 配合 EPOLLOUT 实现 |
| P2 | 5.4 | Pipeline ExceptionCaught | 🔄 | 默认异常传播链 V2.1 完善 |
| P2 | 5.5 | EventLoop.Stop 资源清理 | ✅ | Deregister 所有 fd + poller.Close |
| P2 | 5.6 | 客户端自动重连 | ✅ | 指数退避 1s→2x→60s |
| P3 | 1.3 | EventLoop 异步 dispatch | ✅ | goroutine pool |
| P3 | 1.5 | Pipeline 预编译数组 | ✅ | atomic.Pointer 缓存 next/prev |
| P3 | 2.2 | Peek+Skip 零拷贝帧解析 | ✅ | ReadFrameFromBuf |
| P3 | 2.4 | mask 原地 XOR | ✅ | applyMaskInPlace |
| P3 | 2.5 | ByteBuf 扩容对齐 | ✅ | 512/4096/65536 对齐 |
| P3 | 5.7 | benchmark 基线 | ✅ | frame/buf/hub/pipeline |
| P3 | 5.8 | ByteBuf 线程安全文档 | ✅ | 包注释明确声明 |

### 4.2 关键优化详解

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

## 5. 数据流

### 5.1 服务端收消息（Inbound）

```
客户端 TCP 帧
    │
    ▼
[bufio.Reader] 批量预读到 64KB 缓冲
    │
    ▼
[frame.ReadFrameLimit] 从缓冲解析帧（3-5 次内存读，0 次 syscall）
    │
    ▼
[server.serveConn] Text/Binary → conn.Message
    │
    ▼
[pipeline.FireChannelRead] 触发 Inbound 链（原子指针 O(1) 遍历）
    │
    ├── [FrameCodec] 已提前注册，无操作
    │
    └── [BizHandler] 业务逻辑处理
```

### 5.2 服务端发消息（Outbound）

```
业务逻辑调用 ctx.Write(Message)
    │
    ▼
[pipeline.FireChannelWrite] 触发 Outbound 链（从尾到头）
    │
    ├── [FrameCodec] Message → Frame → 调用 frame.WriteFrameTo(ByteBuf)
    │
    └── [HeadContext.Write] 将 ByteBuf 写入 net.Conn
    │
    ▼
[ByteBuf.Release] 引用计数 -1，归零时回收到 Pool
```

### 5.3 广播

```
Hub.Broadcast(Message)
    │
    ▼
[shardedHub] 遍历所有 shard
    │
    ├── shard[0]: RLock → 拷贝 Session 引用 → RUnlock → 逐个 FireChannelWrite
    ├── shard[1]: RLock → ...
    └── ...
```

---

## 6. 安全与正确性

### 6.1 DoS 防护

| 攻击向量 | 防御措施 |
|---|---|
| 超大单帧 | `MaxFrameSize` 限制，超限返回错误并断开 |
| 无限分片 | 分片重组累积长度上限，超限断开 |
| 慢速握手 | handshake 后设置读 deadline |
| 慢速读 | serveConn 带 read deadline，心跳超时兜底 |
| 广播洪泛 | Hub 发送非阻塞，channel 满直接跳过 |

### 6.2 资源泄漏防护

| 资源 | 防护措施 |
|---|---|
| goroutine | 时间轮替代 per-conn ticker；EventLoop pool 固定大小 |
| channel | `Close()` 关闭 `stateChan`；`Stop()` 关闭 `stopCh` |
| fd | `Stop()` Deregister 所有 fd + `poller.Close()` |
| ByteBuf | 引用计数 + double-free panic |

---

## 7. 跨平台策略

| 平台 | 事件驱动 | 状态 |
|---|---|---|
| Linux | epoll | 主力平台，完整实现 |
| macOS / FreeBSD / OpenBSD | kqueue | 开发调试，完整实现 |
| Windows | IOCP | V2.1 实现 |
| 其他 | netConn fallback | 功能完整，性能受限 |

---

## 8. 测试策略

| 层级 | 方式 | 目标 |
|---|---|---|
| eventloop | mock Poller + race 检测 | 事件分发正确，无 data race |
| buf | 单元测试 + benchmark | 引用计数、Slice、Pool、性能基线 |
| frame | 单元测试 + benchmark | RFC 6455 合规、零拷贝路径、性能基线 |
| pipeline | mock Handler + race 检测 | 链式调用正确，并发 Add/Remove/Remove 安全 |
| conn | `net.Pipe` + 接口测试 | 读写、握手、codec |
| session | mock Conn + Pipeline | 状态机、心跳、重连 |
| hub | mock Session + benchmark | 分片锁正确、广播性能 |
| 集成 | 真实 Server + Client | 功能完整 |

---

## 9. 演进路线

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 接口化重构 + Pipeline + 事件驱动 + 时间轮心跳 + 优化清单 P0/P1/P2/P3 | 十万连接 |
| V2.1 | epollConn 完整非阻塞读写 + Windows IOCP + sendfile/splice | 跨平台完整 |
| V2.2 | goroutine 池化精细调优 + 业务计算池分离 + 写合并 | 五十万连接 |
| V2.3 | 内核旁路（DPDK/AF_XDP）调研 | 百万连接 |

# ws v2 — 架构设计

本文档面向希望理解 `ws` v2 内部设计的开发者，阐述分层架构、核心组件、双 I/O 模式体系和安全策略。

---

## 1. 设计哲学

### 1.1 面向接口编程

每一层只依赖下层接口，不依赖具体实现：

- `session` 依赖 `conn.Conn` 和 `pipeline.ChannelPipeline`，不知道 `netConn` 或 `epollConn`
- `eventloop` 只依赖 `EventHandler` / `Poller` 接口，`conn` 层实现它们
- `frame` 层不依赖任何上层包，可独立使用
- `server` 只依赖 `Acceptor` 接口，不知道 `netAcceptor` 或 `epollAcceptor`

这种设计允许**逐层替换**——你可以只重写 `conn` 层接入自定义传输，或只替换 `Acceptor` 改变连接接入方式，而不影响其他层。

### 1.2 高并发五原则

| 原则 | 含义 | 在 ws 中的体现 |
|---|---|---|
| **零 goroutine 空闲开销** | 空闲连接不绑定任何 goroutine | epollConn + EventLoop，百万空闲连接 ≈ CPU 核数个 goroutine |
| **零拷贝** | 数据在内存中只移动指针，不复制字节 | ByteBuf.Slice、Peek+Skip 帧解析、Retain/Release 广播 |
| **内存池化** | 高频分配的对象从池获取，降低 GC 压力 | ByteBuf 分级 sync.Pool |
| **减少系统调用** | 一次 syscall 处理更多数据 | bufio 批量预读、WriteFrame 合并 header+payload 单次 write |
| **无锁/少锁** | 消除或缩小临界区 | Pipeline 原子指针遍历、EventLoop copy-on-write、Hub 分片锁 |

### 1.3 模式抽象 — I/O 模型是部署选择，不是业务约束

```
ws.Config{Mode: ws.ModeNet}   → netConn + goroutine-per-conn    → 全平台开发调试
ws.Config{Mode: ws.ModeEpoll} → epollConn + EventLoop Reactor   → Linux 生产环境
```

- 业务层（Pipeline + Handler）完全不知道底层是 `netConn` 还是 `epollConn`
- `Pipeline.AddLast("echo", &EchoHandler{})` 在两种模式下行为一致
- 通过 `Acceptor` 接口和 build-tag 工厂，模式切换对上层透明

### 1.4 借鉴 Netty，适配 Go

引入 Pipeline/Handler/ByteBuf/Reactor 模型，用 Go 的 goroutine 和 channel 替代 Java NIO 的线程模型：

- **Java NIO**：一个线程一个 EventLoop，Handler 在 EventLoop 线程同步执行
- **Go 模型**：EventLoop 只做事件感知和 fd 映射，Handler 通过 goroutine pool 异步调度

---

## 2. 分层架构

### 2.1 包结构

```
ws/
├── eventloop/      # 跨平台事件驱动层（epoll / kqueue）
├── buf/            # 引用计数 ByteBuf + 分级对象池
├── frame/          # RFC 6455 协议层（可独立使用）
├── pipeline/       # Netty 风格 Handler 链
├── conn/           # 连接层（netConn / epollConn / 握手 / FrameCodec / ConnWriter）
├── session/        # 会话层（状态机、心跳、时间轮、重连）
├── hub/            # 连接管理中心（分片锁、广播）
├── server/         # 服务端 API（Acceptor 集成、优雅关闭）
├── client/         # 客户端 API（握手、自动重连）
└── ws.go           # 根包：Config、WSError、IOMode、ValidateMode
```

### 2.2 依赖规则

```
server / client  →  session  →  pipeline  →  conn  →  eventloop
                                           ↘  frame + buf
```

1. 上层只能依赖下层接口，不能依赖具体实现
2. 不能跨层调用（如 `server` 不能直接调用 `frame`）
3. 不能反向依赖（如 `buf` 不知道 `pipeline` 的存在）
4. 根包 `ws` 不依赖任何子包

### 2.3 循环依赖解决

`eventloop.Register` 需要 handler，而 `conn.EventDrivenConn` 需要绑定 EventLoop。

- `EventLoop.Register` 只依赖 `EventHandler` 接口（`conn` 实现它）
- `EventDrivenConn.SetEventLoop(el interface{})` 使用 `interface{}` 避免直接依赖 `eventloop` 包

---

## 3. 核心组件设计

### 3.1 eventloop — 事件驱动基石

#### Main/Sub Reactor 模型

```
┌─────────────────┐
│ MainEventLoop   │  ← 1 个，负责 Accept 新连接
│ (epoll/kqueue)  │
└────────┬────────┘
         │ 新连接
         ▼
┌─────────────────┐     ┌─────────────────┐
│ SubEventLoop 0  │     │ SubEventLoop 1  │  ← N 个（默认 = CPU 核数）
│ (epoll/kqueue)  │ ... │ (epoll/kqueue)  │     每个管理一组连接 I/O
└─────────────────┘     └─────────────────┘
         │                       │
         ▼                       ▼
   [worker pool]          [worker pool]    ← 异步调度 Handler，避免慢逻辑阻塞 Loop
```

#### 关键机制

| 机制 | 实现 | 收益 |
|---|---|---|
| **EventLoopGroup** | round-robin 原子分配新连接到 EventLoop | 连接负载均衡 |
| **copy-on-write handler 查找** | `atomic.Value` 存储 handlers map 快照 | 事件分发路径零锁竞争 |
| **goroutine pool 异步调度** | 固定大小 goroutine pool，EventLoop 只做事件感知+fd 映射 | 慢 Handler 不影响其他连接 |
| **Wake 机制** | Linux eventfd / BSD pipe，写入唤醒 `epoll_wait` | 跨 goroutine 即时中断 poller 等待 |
| **Stop 资源清理** | 遍历 Deregister 所有 fd + 关闭 poller + 关闭 wake fd | 无 fd 泄漏 |

#### 跨平台

| 平台 | 机制 | 特性 |
|---|---|---|
| Linux | epoll | EPOLLET 边缘触发 |
| macOS / FreeBSD / OpenBSD | kqueue | EVFILT_READ/WRITE |
| Windows | IOCP | V2.1 实现 |

---

### 3.2 buf — 引用计数 ByteBuf

使用引用计数 ByteBuf 替代 Go 原生 `[]byte`，解决三个问题：频繁分配导致 GC 压力、无法零拷贝传递、读写边界混乱。

#### 引用计数规则

| 操作 | refCount | 说明 |
|---|---|---|
| `NewByteBuf()` / `Pool.Get()` | `1` | 创建时初始化 |
| `Retain()` | `+1` | 传递给其他 goroutine 前调用 |
| `Slice()` | 原始 `+1`，新视图 `1` | 新视图依赖原始 buffer |
| `Release()` | `-1` | 归零时回收到 Pool，< 0 时 panic（double-free 保护） |

#### 分级对象池

| 级别 | 容量范围 | 用途 |
|---|---|---|
| small | ≤ 512B | 控制帧、心跳帧 |
| default | ≤ 4096B | 普通消息 |
| large | ≤ 65536B | 大消息 |
| 直接分配 | > 65536B | 超大消息，走 GC |

`EnsureWritable` 按 512/4096/65536 对齐扩容，提高 Pool 命中率。

---

### 3.3 frame — RFC 6455 协议层

#### 安全加固

- `ReadFrameLimit(r, maxPayload)`：读取 payload 长度后立即校验，超限返回 `MaxFrameSizeError`
- 分片重组时累加已读长度，防止无限 continuation 帧耗尽内存
- continuation 帧 opcode 必须为 0x0（RFC 6455 §5.4），阻塞和非阻塞路径均校验

#### 零拷贝路径

| API | 方向 | 说明 |
|---|---|---|
| `ReadFrameLimit(r, max)` | 读（阻塞） | 从 `io.Reader` 解析 |
| `ReadFrameBuf(r, pool, max)` | 读（阻塞） | 从 Pool 获取 ByteBuf，payload 直接写入底层数组 |
| `ReadFrameFromBuf(bb, max)` | 读（零拷贝） | Peek+Skip 从 ByteBuf 解析，payload 引用 ByteBuf 底层数组 |
| `WriteFrame(w, f)` | 写（阻塞） | 合并 header+payload 一次性 write |
| `WriteFrameTo(dst, f)` | 写（零拷贝） | 直接序列化到 ByteBuf，配合 Pool 使用可零 alloc |
| `applyMaskInPlace(payload, key)` | — | 原地 XOR 去掩码，服务端 Inbound 路径不额外分配 |

#### IncrementalParser — 增量帧解析器

专为事件驱动 I/O 设计的非阻塞解析器，解决 `ReadFrame` 只能用于阻塞 I/O 的问题。

| 特性 | `ReadFrame`（阻塞） | `IncrementalParser`（非阻塞） |
|---|---|---|
| I/O 模型 | `io.Reader` 阻塞等待 | 非阻塞 `[]byte` 切片输入 |
| 不完整数据 | 阻塞等待更多 | 缓冲，下次 `Feed` 继续 |
| 适用场景 | `netConn` + `serveConn` 读循环 | `epollConn` + `handleReadEvent` |
| 分片跟踪 | 内联循环 | `fragState` 状态机 |

---

### 3.4 pipeline — Handler 链

连接建立时组装 Handler 链，运行时事件遍历**不加锁**。

#### prevOutbound 机制

Pipeline 维护两条遍历路径：

- **Inbound 链**（Head → Tail）：每个 handler 的 `nextInbound` 指向右侧最近的 `InboundHandler`
- **Outbound 链**（Tail → Head）：**每个** handler（不管类型）的 `prevOutbound` 指向左侧最近的 `OutboundHandler`

`AddFirst` / `AddLast` / `Remove` 时调用 `rebuild()` 预计算所有原子指针。事件分发时通过 `atomic.Pointer.Load()` 直接获取下一个 handler——**O(1) 且零锁**。

#### 数据流向

```
Inbound:  网络数据 → ByteBuf → Head → ConnWriter(skip) → FrameCodec(skip) → BizHandler
Outbound: BizHandler.ctx.Write(msg) → FrameCodec.Write → ConnWriter.Write → 底层 I/O
```

- `FireChannelRead`：从 Head 向 Tail 遍历 InboundHandler
- `ctx.Write(msg)`：通过 `prevOutbound` 往回走 Outbound 链
- 并发安全：rebuild 在 `mu.Lock()` 中写指针，事件遍历无锁读原子指针

---

### 3.5 conn — 连接层

#### 两种实现

| 特性 | netConn | epollConn |
|---|---|---|
| 平台 | 全平台 | Linux 生产可用 |
| goroutine | serveConn 读循环一个 | 零常驻 goroutine（EventLoop 驱动） |
| 帧解析 | `ReadFrameLimit`（阻塞 I/O） | `IncrementalParser`（非阻塞 `Feed`） |
| 写入 | `net.Conn.Write`（阻塞） | `unix.Write` + EPOLLOUT 队列（非阻塞） |
| TCP 参数 | NODELAY / QUICKACK / REUSEPORT | 相同 |
| 回调 | 无 | `SetOnFrame` / `SetOnClose` |

#### ConnWriter

连接建立时自动添加到 Pipeline 头部的 `OutboundHandler`：
- 接收 `ByteBuf`，调用 `Conn.Write(bb)` 写入底层
- 非 `ByteBuf` 消息透传给下一个 handler

#### FrameCodec

`OutboundHandler`，不持有 `Writer`，可选持有 `Pool`：
- `Message` → `Frame` → `WriteFrameTo(ByteBuf)` → `ctx.Write(bb)` 传入 Outbound 链
- `netConn` 和 `epollConn` 的 `FrameCodec` 代码完全一样——写路径统一

#### HandshakeFD — 基于 fd 的 WebSocket 握手（Linux）

- `ServerHandshakeFD` / `ClientHandshakeFD`：先用 `unix.Dup(fd)` 隔离原始 fd → `os.NewFile` + `net.FileConn` 完成 HTTP 握手 → 关闭 dup fd，返回原始 fd
- **为什么要 Dup？** Go 的 `os.NewFile` 会注册 GC finalizer。如果不 Dup，GC 可能关闭我们正在使用的原始 fd

#### DialNonBlock（Linux）

- 非阻塞 `socket(AF_INET/AF_INET6, SOCK_STREAM|SOCK_NONBLOCK)` → `connect` → 返回 `EINPROGRESS`
- 客户端用临时 epoll 实例等待连接完成

---

### 3.6 session — 会话层

#### 状态机

```
Disconnected → Connecting → Connected
                    │            │
                    ▼            ▼
               Reconnecting ←── 断开
                    │            │
                    ▼            ▼
                 Closed      Disconnected
```

#### StateChan — 发布/订阅模型

- 每次调用 `StateChan()` 创建独立订阅者 channel（cap 16）
- `SetState` 广播到所有订阅者
- `Close()` 向所有订阅者发送 `StateClosed` 并关闭 channel
- 新订阅者立即收到当前状态，不丢失最新状态

#### 心跳

- `PerConnHeartbeater`：通过全局 `TimingWheel` 调度 ping，不创建额外 goroutine
- `Reset()` 用于在有数据活动时重置心跳定时器
- `SetOnTimeout()` 设置超时回调（通常关闭连接）

#### 共享时间轮

- 128 slots，1s tick，单 goroutine 管理所有超时
- `Add(delay, callback)` 返回 taskID，`Cancel(taskID)` 取消
- 10 万连接的心跳 goroutine 从 10 万降至 1 个

#### 自动重连

- 指数退避：初始间隔 → 2× 倍 → 上限 60s
- 达到 `MaxReconnect` 上限后进入 `StateClosed`
- `Close()` / `Stop()` 可中断重连过程

---

### 3.7 hub — 连接管理中心

#### 分片锁 + 缓存行对齐

- 32 个 shard，每个独立 `sync.RWMutex`，减少锁竞争
- 结构体尾部 padding 到 64 字节避免 false sharing

#### 固定 broadcast worker pool

- 每个 shard 一个 worker goroutine + buffered channel（256 msg）
- `Broadcast()` 投递消息到 channel，worker 异步写入各连接的 Pipeline
- 无 goroutine 创建/销毁开销；channel 满时跳过慢 shard（支持背压）

#### 操作复杂度

| 操作 | 复杂度 | 锁范围 |
|---|---|---|
| `Register` | O(1) | 单 shard 写锁 |
| `Unregister` | O(1) | 单 shard 写锁 |
| `Get` | O(1) | 单 shard 读锁 |
| `Count` | O(shards) | 所有 shard 读锁 |
| `Broadcast` | O(shards + conns) | 先读锁拷贝，再无锁发送 |
| `CloseAll` | O(shards + conns) | 先读锁拷贝，再逐个 Close |

---

### 3.8 server / client

#### initSession 流程

1. 创建 `Session`，绑定 `Conn`
2. 创建 `PerConnHeartbeater`，设置超时回调和 `SetHeartbeater`
3. 设置 `StateConnecting` → `StateConnected`
4. 启动心跳
5. 注册到 `Hub`
6. 添加 `ConnWriter`（Pipeline 头部）+ `FrameCodec`（Pipeline 尾部）
7. 若为 epoll 模式，设置 `SetOnFrame` / `SetOnClose` 回调
8. 触发用户 `OnConnect` 回调

#### serveConn（双模式）

- **net 模式**：`bufio.Reader` 批量预读 → `ReadFrameLimit` → `Pipeline.FireChannelRead`
- **epoll 模式**：监听 `StateChan`，等待 `StateClosed/Disconnected` 后退出

#### 优雅关闭

```
Server.Stop()
  → Acceptor.Close()      — 停止接收新连接
  → Hub.CloseAll()        — 关闭所有已建立连接，触发 serveConn 退出
  → sync.WaitGroup.Wait() — 等待所有 serveConn goroutine 退出
  → Hub.Close()           — 关闭 broadcast worker goroutine
```

---

### 3.9 Acceptor 模式

#### Acceptor 接口

```go
type Acceptor interface {
    Listen(addr string) error
    Accept() (conn.Conn, net.Conn, error)  // net.Conn 在 epoll 模式下为 nil
    Close() error
}
```

#### 两种实现

| 特性 | netAcceptor | epollAcceptor |
|---|---|---|
| 平台 | 全平台 | Linux only |
| 实现 | `http.Server` + Hijack → `netConn` | 原始 fd + `ServerHandshakeFD` → `epollConn` |
| Reactor | 无（Go 标准库 netpoller） | Main Reactor（accept）+ Sub Reactor 池（I/O） |

#### epollAcceptor 工作流程

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

#### Build-tag 工厂

- **Linux** (`acceptor_factory_linux.go`)：`ModeEpoll` → `newEpollAcceptor`，否则 → `newNetAcceptor`
- **非 Linux** (`acceptor_factory_nonlinux.go`)：始终 → `newNetAcceptor`
- `ValidateMode()` 作为运行时安全网：非 Linux 平台选择 `ModeEpoll` 立即返回错误

---

## 4. 数据流

### 4.1 服务端收消息 — net 模式（Inbound）

```
Client TCP 帧
    │
    ▼
[bufio.Reader 64KB] 批量预读
    │
    ▼
[frame.ReadFrameLimit] 从缓冲解析帧
    │
    ▼
[serveConn] Text/Binary → conn.Message
    │
    ▼
[Pipeline.FireChannelRead] Inbound 链（原子指针 O(1) 遍历）
    │
    ├── [ConnWriter]        → skip（不是 InboundHandler）
    ├── [FrameCodec]        → skip（不是 InboundHandler）
    └── [BizHandler]        业务逻辑处理
```

### 4.2 服务端收消息 — epoll 模式（Inbound）

```
Client TCP 帧
    │
    ▼
[EventLoop epoll_wait EPOLLIN]
    │
    ▼
[EpollConn.handleReadEvent] 非阻塞读循环 → IncrementalParser.Feed
    │
    ▼
[IncrementalParser] 返回完整 Frame
    │
    ▼
[onFrame 回调] Frame → conn.Message → Pipeline.FireChannelRead
    │
    └── [BizHandler] 业务逻辑处理
```

### 4.3 服务端发消息 — 两种模式统一（Outbound）

```
BizHandler.ctx.Write(*Message)
    │
    ▼
[FrameCodec.Write] Message → Frame → WriteFrameTo(ByteBuf) → ctx.Write(bb)
    │
    ▼
[ConnWriter.Write] ByteBuf → Conn.Write(bb)
    │
    ├── netConn:   net.Conn.Write
    │
    └── epollConn: writeBuf 入队 → flushWrite
                       │
                       ├── unix.Write 成功 → ByteBuf.Release()
                       └── EAGAIN → 注册 EPOLLOUT → 写就绪后继续 flushWrite
```

### 4.4 广播

```
Hub.Broadcast(Message)
    │
    ▼
遍历所有 worker → 消息投递到 buffered channel
    │
    ├── worker[0] → RLock 拷贝 → RUnlock → 逐个 FireChannelWrite
    ├── worker[1] → ...
    └── ...
```

---

## 5. 安全与正确性

### DoS 防护

| 攻击向量 | 防御措施 |
|---|---|
| 超大单帧 | `MaxFrameSize` 限制，超限返回错误并断开 |
| 无限分片 | 分片累积长度上限 + continuation opcode 校验 |
| 慢速握手 | handshake 后设置读 deadline |
| 慢速读 | serveConn 带 deadline，心跳超时兜底 |
| 广播洪泛 | Hub 发送非阻塞，channel 满直接跳过 |

### 资源泄漏防护

| 资源 | 防护措施 |
|---|---|
| goroutine | 时间轮管理心跳；EventLoop pool 固定大小 |
| channel | `Close()` 关闭所有 StateChan 订阅者；`Hub.Close()` 关闭 worker channel |
| fd | `Stop()` Deregister 所有 fd + `poller.Close()` + 关闭 wake fd |
| ByteBuf | 引用计数 + double-free panic；`closeLocked` 释放 writeBuf |
| os.File GC finalizer | `HandshakeFD` 使用 `unix.Dup` 隔离原始 fd |

---

## 6. 跨平台策略

| 平台 | 事件驱动 | 服务端 Acceptor | 客户端 | 状态 |
|---|---|---|---|---|
| Linux | epoll | netAcceptor + epollAcceptor | net + epoll 模式 | 主力平台 |
| macOS / FreeBSD / OpenBSD | kqueue | netAcceptor | net 模式 | 开发调试 |
| Windows | IOCP | netAcceptor | net 模式 | V2.1 |

**build-tag 策略：**
- `//go:build linux` — epoll 实现（`epollconn.go`、`epoll_linux.go`、`acceptor_epoll_linux.go`、`dial_linux.go`、`handshake_fd_linux.go`）
- `//go:build !linux` — 桩函数/回退实现
- `//go:build darwin || freebsd || openbsd` — kqueue 实现

---

## 7. 演进路线

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 双 I/O 模式 + Acceptor 模式 + EpollConn 生产可用 + Pipeline/Handler + 时间轮心跳 | 十万连接（epoll 模式） |
| V2.1 | Windows IOCP + kqueue-based Acceptor（BSD） + 写合并 + netConn.Read 池化 | 跨平台完整 |
| V2.2 | goroutine 池化精细调优 + 业务计算池分离 | 五十万连接 |
| V2.3 | 内核旁路（DPDK/AF_XDP）调研 | 百万连接 |

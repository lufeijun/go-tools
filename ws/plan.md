# ws v2 性能优化计划

> 基于 v2.0 代码现状，从并发编程逻辑、内存高效管理、网络编程三个维度梳理可落地优化点。

---

## 一、并发编程逻辑优化

### 1.1 心跳：per-conn goroutine → 全局时间轮

- **问题**：`session/heartbeat.go:33` 为每个 Session 启动独立 goroutine + `time.Ticker`。10 万连接 = 10 万个 ticker goroutine。
- **优化**：引入 Timing Wheel（分层时间轮），单 goroutine 管理所有心跳超时。`perConnHeartbeater` 只注册/取消超时节点。
- **文件**：`session/heartbeat.go`，新增 `session/timingwheel.go`
- **收益**：10 万连接的心跳 goroutine 从 10 万降至 1 个

### 1.2 Hub 广播：per-shard 临时 goroutine → 固定 worker pool

- **问题**：`hub/hub.go:83-100` 每次 `Broadcast()` 创建 32 个 goroutine，高频广播时创建/销毁开销大。
- **优化**：预置固定 worker pool，每个 worker 负责一个 shard。Broadcast 将消息投递到 shard 的无锁 ring buffer。
- **文件**：`hub/hub.go`
- **收益**：消除广播 goroutine 创建开销，支持背压

### 1.3 EventLoop handler 调度：同步执行 → 异步 dispatch

- **问题**：`eventloop/eventloop.go:107-113` 在 EventLoop 主 goroutine 同步调用 `h.OnEvent()`，慢 Handler 卡住整个 Loop。
- **优化**：EventLoop 只做"事件感知 + fd 映射"，将 `OnEvent()` 投递到 goroutine pool（I/O pool + 业务池分离）。
- **文件**：`eventloop/eventloop.go`
- **收益**：单个慢 Handler 不影响其他连接事件响应

### 1.4 EventLoop handler 查找：全局 RLock → copy-on-write

- **问题**：`eventloop/eventloop.go:108` 每处理一个事件都要对全局 `handlers map` 加 `RLock`。
- **优化**：使用 `atomic.Value` 存储只读 handlers map 快照，注册/注销时复制新 map 原子替换。
- **文件**：`eventloop/eventloop.go`
- **收益**：消除事件分发路径上的锁竞争

### 1.5 Pipeline 遍历：链表动态查找 → 预编译数组

- **问题**：`pipeline/pipeline.go:68-84` 每次事件都遍历双向链表找下一个 Handler，破坏 CPU cache locality。
- **优化**：Pipeline 构建完成后，将 Inbound/Outbound 序列预编译为两个数组，直接按索引遍历。
- **文件**：`pipeline/pipeline.go`
- **收益**：减少指针跳转，提升 cache hit rate

---

## 二、内存高效管理优化

### 2.1 netConn.Read：每次 make tmp buffer → sync.Pool 复用

- **问题**：`conn/netconn.go:44` `tmp := make([]byte, 4096)` 每次 Read 都堆上分配 4KB。
- **优化**：使用 `sync.Pool` 管理读缓冲数组，Read 前 Get，Read 后 Put。
- **文件**：`conn/netconn.go`
- **收益**：消除读路径高频堆分配，降低 GC 压力

### 2.2 byteBuf.ReadBytes：必分配新 slice → 推广 Peek + Skip 模式

- **问题**：`buf/bytebuf.go:56-63` `ReadBytes(n)` 内部 `make([]byte, n)` 并 copy。
- **优化**：帧解析等场景改用 `Peek(n)`（零拷贝查看底层数组）+ `Skip(n)`（移动读指针），避免 alloc。
- **文件**：`buf/bytebuf.go`，各调用方
- **收益**：帧解析时避免大量小对象分配

### 2.3 frame.ReadFrame：每次 make payload → 从 ByteBuf 直接解析

- **问题**：`frame/frame.go:144` 每帧 `make([]byte, payloadLen)`；分片重组时多次 `append` copy；`WriteFrame` 每帧 `make` 新 buffer。
- **优化**：提供 `ReadFrameInto(r, dst buf.ByteBuf)` 直接解析到传入的 ByteBuf；`WriteFrame` 从 pool 取 buffer。
- **文件**：`frame/frame.go`，`conn/codec.go`，`buf/bytebuf.go`
- **收益**：帧解析/序列化路径零堆分配（pool 命中时）

### 2.4 frame mask：创建新 slice → 原地 XOR

- **问题**：`frame/mask.go:10-17` `applyMask` 返回新 slice，服务端去掩码时不必要。
- **优化**：提供 `applyMaskInPlace(payload, maskKey)` 原地修改；服务端 Inbound 路径使用 in-place。
- **文件**：`frame/mask.go`，`frame/frame.go:149`
- **收益**：每条入站消息减少一次 payload 级别的 alloc

### 2.5 ByteBuf EnsureWritable：翻倍扩容 → 预分配 + 对齐

- **问题**：`buf/bytebuf.go:108-114` 扩容策略 `cap * 2`，频繁写入时多次触发 copy。
- **优化**：写入前 `EnsureWritable(needed)` 一次性预分配；按 512/4096 对齐，利于池化回收。
- **文件**：`buf/bytebuf.go`
- **收益**：减少扩容 copy 次数，提高 pool 命中率

---

## 三、网络编程优化

### 3.1 epollConn：完成非阻塞 I/O 实现

- **问题**：`conn/epollconn.go:41-68` `Read/Write/OnEvent/Close` 全部是 TODO stub。这是 V2.0 → V2.1 最关键的 gap。
- **优化**：
  - `Read`：非阻塞 `syscall.Read`，配合 `EPOLLET` 循环读到 `EAGAIN`
  - `Write`：非阻塞 `syscall.Write`，写不下时注册 `EPOLLOUT`，等事件再继续
  - `Close`：`EventLoop.Deregister(fd)` + `syscall.Close(fd)`
- **文件**：`conn/epollconn.go`，`eventloop/eventloop.go`
- **收益**：实现真正的零 goroutine 空闲开销，支撑百万连接的前提

### 3.2 TCP 参数：配置已定义但未生效

- **问题**：`ws.go:60-62` 定义了 `TCPNoDelay`、`TCPQuickAck`、`SOReusePort`，但 `handshake.go` / `netconn.go` 中未调用 `setsockopt`。
- **优化**：`ServerHandshake` / `ClientHandshake` 返回 conn 后，设置 `TCP_NODELAY`、`TCP_QUICKACK`；`server.Start` 前设置 `SO_REUSEPORT`。
- **文件**：`conn/handshake.go`，`server/server.go`，`client/client.go`
- **收益**：Nagle 关闭降低小帧延迟；QUICKACK 减少延迟确认；REUSEPORT 支持多进程负载均衡

### 3.3 frame.ReadFrame：多次 io.ReadFull → 批量预读

- **问题**：`frame/frame.go:109-148` 解析一帧需要 3-5 次 `io.ReadFull`，每次可能触发 syscall。
- **优化**：`conn` 层维护读缓冲（如 `bufio.Reader` 或自定义 ByteBuf 缓冲），网络数据先批量读到缓冲，frame 解析从缓冲消费。
- **文件**：`conn/netconn.go`，`frame/frame.go`
- **收益**：小帧解析 syscall 从 3-5 次降至 1 次

### 3.4 WriteFrame：每帧独立 Write → 写合并（Write Coalescing）

- **问题**：`FrameCodec.Write` 每收到一个业务消息就触发一次 TCP Write，高并发小消息场景包头开销大。
- **优化**：`netConn`/`epollConn` 维护发送缓冲队列，消息先入队，满足阈值（MTU/定时/显式 Flush）时批量 flush。`OutboundHandler.Flush()` 已预留接口。
- **文件**：`conn/netconn.go`，`conn/epollconn.go`，`pipeline/handler.go`
- **收益**：小消息合并发送，减少 TCP 包头和 syscall 次数

### 3.5 EventLoop.Wake()：未实现 → eventfd / pipe

- **问题**：`eventloop/eventloop.go:80-82` `Wake()` 为空实现。业务 goroutine 想写数据时无法唤醒正在 `epoll_wait` 的 EventLoop。
- **优化**：Linux 下用 `eventfd` 加入 epoll，外部 goroutine 写 eventfd 唤醒 EventLoop；BSD 用 `pipe`。
- **文件**：`eventloop/eventloop.go`，`eventloop/epoll_linux.go`，`eventloop/kqueue_bsd.go`
- **收益**：支持跨 goroutine 安全触发写事件，完成事件驱动闭环

---

## 优先级排序

| 优先级 | 优化项 | 原因 |
|---|---|---|
| P0 | 3.1 epollConn 非阻塞 I/O | V2.1 核心交付物，没有它 eventloop 无法发挥价值 |
| P0 | 3.2 TCP 参数实际生效 | 配置已定义但代码未使用，低垂果实 |
| P1 | 1.1 心跳时间轮 | 10万+连接 goroutine 开销最大来源 |
| P1 | 2.1 netConn.Read 池化 | 高频 alloc，改动小收益大 |
| P1 | 2.3 frame 层 ByteBuf 化 | 帧解析是数据路径 alloc 最密集处 |
| P2 | 1.2 Hub broadcast worker pool | 高频广播 goroutine 开销 |
| P2 | 1.4 EventLoop handler 查找优化 | 消除锁竞争 |
| P2 | 3.3 读缓冲批量预读 | 减少 syscall |
| P2 | 3.4 写合并 | 提升小消息吞吐 |
| P3 | 1.3 EventLoop 异步 dispatch | 需引入 goroutine pool，复杂度较高 |
| P3 | 1.5 Pipeline 预编译数组 | micro-optimization |
| P3 | 2.4 mask 原地 XOR | 有收益但需 careful 处理 ownership |

---

## 验证方式

1. 单元测试：每个优化点配套测试
2. benchmark：frame 解析、ByteBuf 读写、Hub 广播优化前后对比
3. 端到端：demo/server + demo/client Echo 场景验证
4. 压力测试：10万连接下监控 goroutine 数、GC 频率、CPU/内存

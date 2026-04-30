# ws v2 性能优化计划

> 基于 v2.0 代码现状，从并发编程逻辑、内存高效管理、网络编程、安全与正确性、架构与工程化五个维度梳理可落地优化点。

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

- **问题**：`buf/bytebuf.go:108-114` 扩容策略 `cap * 2`，频繁写入时多次触发 copy；且 `newData := make([]byte, len(b.data), newCap)` 逻辑不够干净。
- **优化**：扩容时全新分配 `make([]byte, newCap)` 并 copy；写入前 `EnsureWritable(needed)` 一次性预分配；按 512/4096 对齐，利于池化回收。
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

## 四、安全与正确性

### 4.1 ReadFrame 缺少 MaxFrameSize 校验 — DoS 漏洞

- **问题**：`frame.ReadFrame` 不检查 `payloadLen`。恶意客户端发送极大长度帧头，`make([]byte, payloadLen)` 直接 OOM。
- **优化**：读取 payload 长度后立即校验，超过 `Config.MaxFrameSize`（默认 64MB）返回 `WSError{Code: ErrCodeMessageTooBig}`。
- **文件**：`frame/frame.go`，`ws/ws.go`
- **收益**：防止单连接耗尽服务端内存

### 4.2 分片重组无累积长度上限

- **问题**：`frame.ReadFrame:155-172` 处理 `FIN=false` 时循环 `append` 累积 continuation 帧 payload，无上限。
- **优化**：分片重组时累加已读长度，超过 `MaxFrameSize` 立即返回错误。
- **文件**：`frame/frame.go`
- **收益**：防止恶意客户端通过无限 continuation 帧耗尽内存

### 4.3 Handshake 无超时 — 慢速攻击/资源泄漏

- **问题**：`ClientHandshake` 使用 `net.Dial` + `http.ReadResponse`，无任何超时。恶意服务端挂起握手导致客户端 goroutine 永久阻塞。
- **优化**：`ClientHandshake` 中改用 `net.DialTimeout` / `tls.DialWithDialer`，并为 HTTP Upgrade 响应读取设置 deadline。
- **文件**：`conn/handshake.go`
- **收益**：防止握手阶段 goroutine/连接泄漏

### 4.4 Close 帧未按 RFC 6455 回复

- **问题**：`server.go:131` 和 `client.go:125` 收到 `OpcodeClose` 直接 `return`，没有回写 Close 帧。RFC 6455 要求**必须**发送 Close 响应。
- **优化**：收到 Close 帧后，回写 `frame.NewCloseFrame(code, reason)`，然后关闭底层 TCP。
- **文件**：`server/server.go`，`client/client.go`
- **收益**：协议合规，避免对端进入 TIME_WAIT 延迟或异常断开

### 4.5 stateChan / serveConn goroutine 泄漏

- **问题**：
  - `session/session.go:84` `Close()` 不关闭 `stateChan`，监听它的 goroutine 永远阻塞。
  - `serveConn` 中 `ReadFrame` 在 TCP 半开连接场景下可能永远不返回。
- **优化**：
  - `Close()` 中 `close(s.stateChan)`。
  - `serveConn` 配合 `SetReadDeadline`，或用心跳超时兜底关闭连接。
- **文件**：`session/session.go`，`server/server.go`，`client/client.go`
- **收益**：防止连接关闭后 goroutine 和 channel 泄漏

### 4.6 Hub.Send / Broadcast 是空实现

- **问题**：`hub/hub.go:95-109` `Broadcast` 和 `Send` 目前都是 `_ = msg // TODO: write to pipeline (V2.1)`，实际发不出任何消息。
- **优化**：实现真正的消息投递逻辑，通过 `sess.Conn().Pipeline().FireChannelWrite(msg)` 发送。
- **文件**：`hub/hub.go`
- **收益**：修复核心功能缺失

---

## 五、架构与工程化

### 5.1 心跳功能当前完全未工作

- **问题**：`session/heartbeat.go:54` 只有 `// TODO: send ping via conn pipeline (V2.1)`，ticker 触发后什么都不做。服务端/客户端均无法通过心跳检测连接存活，TCP 半开连接永不清理。
- **优化**：在 `perConnHeartbeater.run()` 中调用 `sess.Conn().Pipeline().FireChannelWrite(&conn.Message{Type: 0x9})` 发送 Ping 帧。
- **文件**：`session/heartbeat.go`
- **收益**：恢复连接保活能力，是 Pong 超时检测的前提

### 5.2 serveConn 每个连接一个 goroutine — 与 V2 核心目标矛盾

- **问题**：`server.go:108` 和 `client.go:98` 均为每个连接启动 `go s.serveConn(...)`，当前实现实质上是"goroutine-per-conn"。这与 V2 "零 goroutine 空闲开销" 的设计目标直接冲突。
- **修复路径**：
  - V2.1：epollConn 完成后，`OnEvent` 直接处理 Read 事件并触发 `FireChannelRead`，彻底移除 `serveConn` goroutine。
  - V2.0 过渡期：`netConn` 场景下用 `bufio.Reader` 减少 syscall 次数，作为妥协方案。
- **文件**：`server/server.go`，`client/client.go`，`conn/epollconn.go`
- **收益**：实现 V2 设计目标，支撑百万连接

### 5.3 Hub shard false sharing

- **问题**：`hub/hub.go:40-43` `shard` 结构体很小（约 32 字节），32 个 shard 挤在 2-3 个 CPU cache line（64 字节）中。高并发广播时不同核访问不同 shard 的锁，因 cache line 共享产生 false sharing。
- **优化**：给 `shard` 加 padding，确保每个 shard 独占一个 cache line。
- **文件**：`hub/hub.go`
- **收益**：高并发场景下减少缓存同步开销

### 5.4 Pipeline FireExceptionCaught 空实现

- **问题**：`pipeline/pipeline.go:165` `FireExceptionCaught(err)` 为空，Handler 中抛出的异常被静默吞掉，连接死亡时无日志、无回调。
- **优化**：实现默认异常传播链：记录日志 → 触发 `FireChannelInactive` → 关闭连接；支持用户自定义 `ExceptionHandler`。
- **文件**：`pipeline/pipeline.go`
- **收益**：提升可调试性，避免异常静默

### 5.5 EventLoop.Stop 不清理已注册 fd

- **问题**：`eventloop/eventloop.go:119-125` 只设置 `running=0` 和关闭 `stopCh`，没有遍历已注册 fd 调用 `Deregister`，也没有 `poller.Close()`。
- **优化**：`Stop()` 中遍历 `handlers` 全部 `Deregister`，然后调用 `poller.Close()`，清空内部状态。
- **文件**：`eventloop/eventloop.go`
- **收益**：防止 EventLoop 重启时残留 fd 和未关闭的 epoll/kqueue fd

### 5.6 客户端自动重连 TODO

- **问题**：`client/client.go:90` 标记 `// TODO: auto-reconnect (V2.1)`，当前客户端断开后不会自动恢复连接。
- **优化**：在 `serveConn` 退出或心跳超时后，按退避策略（1s → 2s → 4s ...）重新 `ClientHandshake` 并重建 Session。
- **文件**：`client/client.go`，`session/reconnect.go`
- **收益**：提升客户端可用性

### 5.7 缺少 benchmark 基线

- **问题**：plan.md 提到"验证方式"但未建立基准数据，优化前后无法量化对比。
- **优化**：为以下路径添加 `BenchmarkXxx`：
  - `frame.ReadFrame` / `WriteFrame`
  - `buf.ByteBuf` 读写（含 pool 命中/未命中）
  - `Hub.Broadcast`（模拟 1K/10K/100K 连接）
  - `pipeline.FireChannelRead`（10 个 Handler 链）
- **文件**：各包 `*_test.go`
- **收益**：建立量化基线，指导后续优化方向

### 5.8 ByteBuf 线程安全性未文档化

- **问题**：`ByteBuf` 的 `readerIndex`/`writerIndex` 没有任何同步机制。如果在多个 goroutine 间共享会 data race，但当前无文档说明。
- **优化**：在 `buf/bytebuf.go` 包注释或接口文档中明确声明"ByteBuf 不是线程安全的，跨 goroutine 传递需通过 Retain/Release 管理所有权，且同一时间只应有一个 goroutine 读写"。
- **文件**：`buf/bytebuf.go`
- **收益**：防止误用导致 race condition

---

## 优先级排序

| 优先级 | 编号 | 优化项 | 原因 |
|---|---|---|---|
| P0 | 4.1 | ReadFrame MaxFrameSize 校验 | 安全漏洞（DoS），一行代码修复 |
| P0 | 4.2 | 分片重组累积长度上限 | 安全漏洞（内存耗尽） |
| P0 | 4.6 | Hub.Send / Broadcast 空实现 | 核心功能缺失 |
| P0 | 5.1 | 心跳功能未工作 | 功能缺失，TCP 半开连接永不清理 |
| P0 | 3.1 | epollConn 非阻塞 I/O | V2.1 核心交付物，支撑百万连接的前提 |
| P0 | 3.2 | TCP 参数实际生效 | 配置已定义但代码未使用，低垂果实 |
| P1 | 4.3 | Handshake 超时 | 健壮性，防止 goroutine 泄漏 |
| P1 | 4.4 | Close 帧 RFC 合规 | 协议合规，避免异常断开 |
| P1 | 4.5 | stateChan / serveConn 泄漏 | goroutine/channel 泄漏 |
| P1 | 3.3 | 读缓冲批量预读（bufio） | 低改动高回报，syscall 从 5 次降至 1 次 |
| P1 | 1.1 | 心跳时间轮 | 10 万+连接 goroutine 开销最大来源 |
| P1 | 2.1 | netConn.Read 池化 | 高频 alloc，改动小收益大 |
| P1 | 2.3 | frame 层 ByteBuf 化 | 帧解析是数据路径 alloc 最密集处 |
| P2 | 5.2 | 移除 serveConn goroutine | V2 核心目标，需配合 epollConn |
| P2 | 5.3 | Hub shard false sharing | 高并发优化 |
| P2 | 1.2 | Hub broadcast worker pool | 高频广播 goroutine 开销 |
| P2 | 1.4 | EventLoop handler 查找优化 | 消除锁竞争 |
| P2 | 3.4 | 写合并 | 提升小消息吞吐 |
| P2 | 5.4 | Pipeline ExceptionCaught 实现 | 调试体验 |
| P2 | 5.5 | EventLoop.Stop 资源清理 | 资源泄漏 |
| P2 | 5.6 | 客户端自动重连 | 客户端可用性 |
| P3 | 1.3 | EventLoop 异步 dispatch | 需引入 goroutine pool，复杂度较高 |
| P3 | 1.5 | Pipeline 预编译数组 | micro-optimization |
| P3 | 2.4 | mask 原地 XOR | 有收益但需 careful 处理 ownership |
| P3 | 2.5 | ByteBuf 扩容对齐 | 微优化 |
| P3 | 5.7 | benchmark 基线 | 工程化 |
| P3 | 5.8 | ByteBuf 线程安全文档 | 工程化 |

---

## 验证方式

1. **单元测试**：每个优化点配套测试
2. **benchmark**：frame 解析、ByteBuf 读写、Hub 广播优化前后对比
3. **端到端**：demo/server + demo/client Echo 场景验证
4. **压力测试**：10 万+ 连接监控 goroutine/内存/CPU
5. **安全测试**：构造超大帧、分片轰炸、慢速握手等异常输入验证防御能力

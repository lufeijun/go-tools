# WebSocket 库开发问题记录

开发过程中遇到的问题及解决方案，持续更新。

---

## Q1: 客户端发消息服务端延迟 3-7 秒才收到

**现象：** 客户端发送消息后，服务端 3-7 秒才解析到，远超正常毫秒级延迟。

**根因有两层：**

### 1. 使用层面：Duration 类型误传

`ServerConfig.PingInterval` 的类型是 `time.Duration`（int64 纳秒），demo 中误传 `30` 而非 `30 * time.Second`，实际值为 **30 纳秒**。心跳 goroutine 以极高频率空转，CPU 被占满，其他 goroutine 调度严重延迟。

**修复：**
- demo/server.go 改为 `30 * time.Second`
- 库层面加保护：`< time.Second` 的值视为误传，重置为默认值

### 2. 库层面：TCP 写入性能缺陷

- **并发写 `net.Conn` 导致帧损坏** — `readLoop`、`writeLoop`、`Close()` 三处同时写 `c.conn`，`net.Conn` 非并发写安全，帧字节交错后接收方 `io.ReadFull` 阻塞等待
- **`WriteFrame` 分两次 Write** — header + payload 两次 `w.Write()`，触发 Nagle + Delayed ACK 交互延迟
- **未设 TCP_NODELAY** — 默认 Nagle 算法缓存小包，对实时小帧不利

**修复：**
- 所有写操作统一走 `writeChan`，只有 `writeLoop` 写 `net.Conn`
- `WriteFrame` 合并为单次 `Write()`，header + payload 拼到一个 buf 一次性写出
- 建连时调用 `tc.SetNoDelay(true)` 关闭 Nagle

**修复后延迟：** 50-200μs

**涉及文件：** `demo/server/server.go`、`ws/server.go`、`ws/client.go`、`ws/internal/conn/conn.go`、`ws/frame/frame.go`

---

## Q2: Session 类型第三方无法引用

**现象：** README 示例中 `*ws.Session` 实际指向 `ws/internal/session.Session`，Go 的 `internal` 机制阻止第三方 import，用户无法在外部代码中使用 `*ws.Session`。

**根因：** `internal` 包下的类型只对父包及子包可见，外部包无法 import。

**修复：** 在 `ws/types.go` 中添加类型别名 `type Session = session.Session`，所有公开 API 使用 `*Session` 而非 `*session.Session`。同步修改 `hub.go`、`server.go`、`client.go` 及测试文件。

**涉及文件：** `ws/types.go`、`ws/hub.go`、`ws/server.go`、`ws/client.go`、`ws/hub_test.go`、`ws/ws_test.go`

---

## Q3: 心跳消费 ReadChan 导致用户收不到消息

**现象：** 心跳检测从 `ReadChan()` 读取 Pong 帧来判断超时，但 ReadChan 是用户读取消息的同一通道，心跳消费后用户就收不到了。

**根因：** V1 心跳设计直接消费 ReadChan，与用户读取冲突。

**修复：** V1 心跳改为只发送 Ping，不消费 ReadChan。Pong 超时检测推迟到 V2（用 time wheel 方案在 conn 层拦截 Pong，不经过 ReadChan）。

**涉及文件：** `ws/internal/session/heartbeat.go`

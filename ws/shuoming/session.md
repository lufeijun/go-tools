# ws/session — 会话层

`ws/session` 在 `Conn` + `Pipeline` 之上管理连接的状态、心跳和自动重连，是业务代码直接交互的主要抽象之一。

---

## 设计定位

连接层（`conn`）只关心 I/O，而会话层关心**连接的生命周期**：

- **状态机**：连接从建立到关闭的完整状态流转
- **心跳保活**：通过共享时间轮定时发送 Ping 帧，检测连接是否存活
- **自动重连**：客户端断线后按指数退避策略自动重试

---

## 核心接口

### Session

```go
type Session interface {
    Conn() conn.Conn
    State() State
    StateChan() <-chan State
    SetState(State)
    Close() error
}
```

| 方法 | 说明 |
|---|---|
| `Conn()` | 获取底层连接对象 |
| `State()` | 获取当前状态 |
| `StateChan()` | 创建一个新的状态订阅通道（只读），每次调用创建独立订阅 |
| `SetState()` | 设置新状态，并向所有订阅者广播 |
| `Close()` | 关闭会话，状态变为 Closed，关闭所有订阅通道 |

> **重要：** `StateChan()` 是**发布/订阅模式**，不是单一共享 channel。每次调用创建独立订阅。详见下文。

### State（状态枚举）

```go
type State int

const (
    StateDisconnected State = iota  // 初始/已断开
    StateConnecting                 // 正在连接
    StateConnected                  // 已连接
    StateReconnecting               // 正在重连
    StateClosed                     // 已关闭（不再重连）
)
```

状态流转图：

```
初始: Disconnected

      Connect() 被调用
            |
            v
      Connecting --失败--> Reconnecting <------+
            |                |   ^              | 达到 MaxReconnect
            | 成功           |   | 重试间隔后  |
            v                |   |              |
       Connected --断开---->+   +--------------+
            |                                    |
            | 调用 Close()                       v
            v                              Closed
      Disconnected
```

---

## 默认实现：defaultSession

### 结构体

```go
type defaultSession struct {
    conn   conn.Conn
    config Config
    state  State
    closed int32         // 原子标志，保证 Close 只执行一次

    mu       sync.Mutex
    subs     map[chan State]struct{}   // 所有订阅者
}
```

**关键变化（v2 最新）：**

- 旧版：`stateChan chan State` — 单一共享 channel，只有一个 goroutine 能监听
- 新版：`subs map[chan State]struct{}` — **发布/订阅模式**，支持多个 goroutine 独立监听

### 构造

```go
func NewSession(c conn.Conn, cfg Config) Session {
    return &defaultSession{
        conn:   c,
        config: cfg,
        subs:   make(map[chan State]struct{}),
    }
}
```

- 初始化时 `subs` 为空 map，没有任何订阅者
- 调用 `StateChan()` 时才创建订阅通道并注册

### StateChan()：创建独立订阅

```go
func (s *defaultSession) StateChan() <-chan State {
    ch := make(chan State, 16)       // 创建新的订阅通道，容量 16
    s.mu.Lock()
    s.subs[ch] = struct{}{}          // 注册到订阅者列表
    // 立即向新订阅者投递当前状态，避免错过最新状态
    if s.state != StateDisconnected {
        select {
        case ch <- s.state:
        default:
        }
    }
    s.mu.Unlock()
    return ch
}
```

**行为详解：**

1. **每次调用都创建新的 `chan State`** — 不同 goroutine 各自调用 `StateChan()` 得到独立通道
2. **容量 16** — 缓冲最近 16 个状态变化，满时丢弃，避免阻塞发送方
3. **立即投递当前状态** — 新订阅者不会错过在订阅前已经发生的状态变化
4. **互不影响** — 一个 goroutine 消费慢不影响其他 goroutine 的接收

> **StateChan 是发布/订阅模式，不是单一共享 channel。每次调用创建独立订阅。**

**典型用法：**

```go
// goroutine A：监听状态变化，打印日志
go func() {
    for state := range sess.StateChan() {
        log.Println("状态变化:", state)
    }
}()

// goroutine B：监听状态变化，触发业务逻辑
go func() {
    for state := range sess.StateChan() {
        if state == session.StateConnected {
            // 重新发送订阅等恢复逻辑
        }
    }
}()
```

两个 goroutine 各自拥有独立的订阅通道，互不干扰。

### SetState()：广播到所有订阅者

```go
func (s *defaultSession) SetState(st State) {
    if atomic.LoadInt32(&s.closed) == 1 {
        return   // 已关闭的 session 不再接受状态变化
    }
    s.state = st
    s.mu.Lock()
    for ch := range s.subs {
        select {
        case ch <- st:     // 非阻塞发送
        default:
                        // 通道满时丢弃，不阻塞
        }
    }
    s.mu.Unlock()
}
```

**行为详解：**

1. **遍历所有订阅者** — 将新状态发送到每一个已注册的通道
2. **非阻塞发送** — 使用 `select + default`，如果某个通道满了就跳过
3. **已关闭则忽略** — 通过 `atomic.LoadInt32(&s.closed)` 检查，避免在 Close 后继续操作

### Close()：关闭所有订阅者

```go
func (s *defaultSession) Close() error {
    if atomic.CompareAndSwapInt32(&s.closed, 0, 1) {
        s.state = StateClosed
        s.mu.Lock()
        for ch := range s.subs {
            // 向每个订阅者发送 StateClosed，确保他们能收到关闭通知
            select {
            case ch <- StateClosed:
            default:
            }
            close(ch)   // 关闭通道，range 循环会退出
        }
        s.subs = make(map[chan State]struct{})  // 清空订阅者列表
        s.mu.Unlock()
    }
    return s.conn.Close()
}
```

**行为详解：**

1. **先发送 StateClosed** — 确保所有订阅者收到关闭信号
2. **再关闭通道** — `close(ch)` 使得 `for range ch` 循环正常退出
3. **清空 subs** — 防止残留引用
4. **CAS 保护** — `CompareAndSwapInt32` 保证 Close 只执行一次
5. **关闭底层 conn** — 最后调用 `s.conn.Close()`

> **监听 StateChan 的 goroutine 如何退出：** 当 `Close()` 被调用后，通道被关闭，`for range ch` 循环会在读完缓冲数据后自然退出。

---

## onClose 回调与 epoll 模式

在 epoll 模式下，`EpollConn` 提供了 `onClose` 回调机制：

```go
// server.go 中的 setupEpollFrameCallback
func (s *defaultServer) setupEpollFrameCallback(c conn.Conn, sess session.Session) {
    if edc, ok := c.(conn.EventDrivenConn); ok {
        edc.SetOnClose(func() {
            sess.SetState(session.StateDisconnected)
        })
        // ...
    }
}
```

**触发链路：**

1. `EpollConn` 检测到连接断开（收到 EPOLLHUP/EPOLLERR 事件，或 read 返回 0）
2. `EpollConn.closeLocked()` 被调用，执行 `c.onClose()`
3. `onClose` 回调触发 `sess.SetState(session.StateDisconnected)`
4. `SetState` 将 `StateDisconnected` 广播到所有 `StateChan()` 订阅者
5. `serveConn` 的 `for range sess.StateChan()` 循环收到 `StateDisconnected`，退出

**为什么需要 onClose：** 在 epoll 模式下，`serveConn` 不再阻塞在 `frame.ReadFrame` 上，而是等待 `StateChan` 的信号。`onClose` 回调是 EventLoop 通知 Session 层连接已断开的桥梁。

---

## 心跳：Heartbeater

### 接口

```go
type Heartbeater interface {
    Start(s Session)
    Stop()
    SetOnTimeout(fn func())
}
```

### v1 问题

v1 为每个 Session 启动独立 `time.Ticker` goroutine。10 万连接 = 10 万个 ticker goroutine。

### v2 优化：共享时间轮

引入单级时间轮（128 slots，1s tick），**一个 goroutine 管理所有心跳超时**：

```go
// timingwheel.go
type TimingWheel struct {
    tickMs    time.Duration
    wheelSize int
    buckets   []bucket
    taskIDSeq uint64
}

type bucket struct {
    mu    sync.Mutex
    tasks map[uint64]*task
}

type task struct {
    expireAt time.Time
    callback func()
}
```

**API：**

```go
func (tw *TimingWheel) Add(delay time.Duration, callback func()) uint64
func (tw *TimingWheel) Cancel(taskID uint64)
```

- `Add` 注册超时任务，返回 `taskID`
- `Cancel` 取消任务
- 每个 Session 的 heartbeat 只注册/取消时间轮节点，不创建 goroutine
- **10 万连接的心跳 goroutine 从 10 万降至 1 个**

### 默认实现：perConnHeartbeater

```go
type perConnHeartbeater struct {
    pingInterval time.Duration
    pongTimeout  time.Duration
    onTimeout    func()
}
```

**运行逻辑：**

```go
func (h *perConnHeartbeater) Start(s Session) {
    // 注册时间轮任务：定时发送 Ping 帧
    // 注册时间轮任务：Pong 超时检测
}
```

- 发送 Ping：`sess.Conn().Pipeline().FireChannelWrite(&conn.Message{Type: 0x9})`
- Pong 超时：调用 `onTimeout()`，通常关闭连接

**超时回调：**

```go
hb.SetOnTimeout(func() {
    sess.SetState(session.StateDisconnected)
    c.Close()
})
```

---

## 自动重连：Reconnector

### 接口

```go
type Reconnector interface {
    Start(s Session)
    Stop()
}
```

### 默认实现：reconnector

```go
type reconnector struct {
    interval   time.Duration
    maxRetries int
    dial       func() (conn.Conn, error)
    stopChan   chan struct{}
}
```

**构造：**

```go
func NewReconnector(interval time.Duration, maxRetries int, dial func() (conn.Conn, error)) Reconnector
```

**重连逻辑（指数退避）：**

```go
func (r *reconnector) Start(s Session) {
    s.SetState(StateReconnecting)
    backoff := r.interval
    for i := 0; i < r.maxRetries; i++ {
        select {
        case <-r.stopChan:
            return
        case <-time.After(backoff):
        }

        s.SetState(StateConnecting)
        c, err := r.dial()
        if err != nil {
            s.SetState(StateReconnecting)
            // 指数退避：1s -> 2s -> 4s ... 最大 60s
            if backoff < 60*time.Second {
                backoff *= 2
            }
            continue
        }
        _ = c
        s.SetState(StateConnected)
        return
    }
    s.SetState(StateClosed)
}
```

- 初始间隔 `interval`（默认 5s）
- 每次失败间隔翻倍，最大 60s
- 达到 `maxRetries` 后进入 `StateClosed`，不再重试
- `Stop()` 可中断重连过程

### 客户端集成

`client.go` 的 `maybeReconnect()` 方法在 `serveConn` 退出后自动触发：

```go
func (c *defaultClient) maybeReconnect() {
    c.mu.Lock()
    if c.closed {
        c.mu.Unlock()
        return
    }
    c.mu.Unlock()

    go func() {
        backoff := c.config.ReconnectInterval
        for i := 0; i < c.config.MaxReconnect; i++ {
            time.Sleep(backoff)
            // ... 检查 closed，尝试 doConnect
            if err := c.doConnect(); err == nil {
                return
            }
            if backoff < 60*time.Second {
                backoff *= 2
            }
        }
        if c.sess != nil {
            c.sess.SetState(session.StateClosed)
        }
    }()
}
```

---

## Session Config

```go
type Config struct {
    PingInterval      time.Duration
    PongTimeout       time.Duration
    ReconnectInterval time.Duration
    MaxReconnect      int
}
```

---

## 使用示例：监听状态变化

### 基本用法

```go
package main

import (
    "fmt"
    "github.com/lufeijun/goTools/ws/client"
    "github.com/lufeijun/goTools/ws/session"
)

func main() {
    cfg := ws.DefaultConfig()
    cfg.Addr = "ws://localhost:8080/"

    c, err := client.NewClient(cfg)
    if err != nil {
        log.Fatal(err)
    }
    if err := c.Connect(); err != nil {
        log.Fatal(err)
    }

    sess := c.Session()

    // 监听状态变化 — 每次调用 StateChan() 创建独立订阅
    go func() {
        for state := range sess.StateChan() {
            switch state {
            case session.StateConnected:
                fmt.Println("【状态】已连接")
            case session.StateDisconnected:
                fmt.Println("【状态】已断开")
            case session.StateReconnecting:
                fmt.Println("【状态】正在重连...")
            case session.StateClosed:
                fmt.Println("【状态】连接已关闭")
                return
            }
        }
    }()

    select {}
}
```

### 多个 goroutine 各自监听

```go
// 日志 goroutine
go func() {
    for state := range sess.StateChan() {  // 创建订阅 A
        log.Println("[日志] 状态变化:", state)
        if state == session.StateClosed {
            return
        }
    }
}()

// 业务恢复 goroutine
go func() {
    for state := range sess.StateChan() {  // 创建订阅 B（独立通道）
        if state == session.StateConnected {
            // 重连成功，恢复业务
            resubscribe()
        }
        if state == session.StateClosed {
            return
        }
    }
}()
```

**关键点：** 两次 `StateChan()` 调用返回不同的通道，两个 goroutine 互不影响。一个通道消费慢不会导致另一个通道阻塞或丢失消息。

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `session.go` | `Session` 接口、`State` 枚举、`defaultSession` 实现（发布/订阅模式） |
| `heartbeat.go` | `Heartbeater` 接口、`perConnHeartbeater` 实现（基于时间轮） |
| `timingwheel.go` | 共享单级时间轮（128 slots，1s tick） |
| `reconnect.go` | `Reconnector` 接口、`reconnector` 实现（指数退避） |
| `session_test.go` | 状态流转、心跳启动、重连耗尽测试 |

---

## 注意事项

1. **V2.0 心跳基于共享时间轮** — 不再是 per-conn ticker goroutine，10 万连接的心跳 goroutine 从 10 万降至 1 个
2. **StateChan 是发布/订阅模式，不是单一共享 channel** — 每次调用创建独立订阅，多个 goroutine 可以各自监听而不互相影响
3. **StateChan 容量 16** — 状态变化过快时会丢弃旧状态，业务监听应尽早读取
4. **SetState 广播到所有订阅者** — 新状态会发送到每一个已注册的 StateChan 通道，满时丢弃
5. **Close 关闭所有订阅通道** — 调用后先发送 StateClosed，再关闭所有通道，监听者可正常退出
6. **重连不自动恢复 Pipeline** — V2.1 会补充重连后重新组装 Pipeline 的逻辑
7. **Session.Close() 同时关闭 Conn 和所有 StateChan** — 调用后连接不可再用，监听 StateChan 的 goroutine 会收到通道关闭信号
8. **心跳与重连均为 per-Session** — 但心跳 goroutine 已共享（时间轮），重连 goroutine 按需创建
9. **epoll 模式下 onClose 回调触发 SetState(StateDisconnected)** — EpollConn.onClose 是 EventLoop 通知 Session 的桥梁

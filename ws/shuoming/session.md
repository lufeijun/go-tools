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
| `StateChan()` | 状态变化通知通道（只读） |
| `SetState()` | 设置新状态，并通知 StateChan |
| `Close()` | 关闭会话，状态变为 Closed，关闭 stateChan |

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

---

## 默认实现：defaultSession

```go
type defaultSession struct {
    conn      conn.Conn
    config    Config
    stateChan chan State
    state     State
    closed    atomic.Bool
}
```

**构造：**

```go
func NewSession(c conn.Conn, cfg Config) Session
```

**状态通知：**

```go
func (s *defaultSession) SetState(st State) {
    s.state = st
    select {
    case s.stateChan <- st:  // 非阻塞写入
    default:
    }
}
```

- `stateChan` 容量为 16，缓冲最近的状态变化
- 满时丢弃旧状态，避免阻塞发送方

**Close：**

```go
func (s *defaultSession) Close() error {
    if s.closed.CompareAndSwap(false, true) {
        s.state = StateClosed
        close(s.stateChan)
    }
    if s.conn != nil {
        return s.conn.Close()
    }
    return nil
}
```

- 直接设置 `StateClosed` 并关闭 `stateChan`，防止监听它的 goroutine 永远阻塞
- 同时关闭底层 `conn`

---

## 心跳：Heartbeater（P0 优化 5.1 + P1 优化 1.1）

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

## 自动重连：Reconnector（P2 优化 5.6）

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
            // 指数退避：1s → 2s → 4s ... 最大 60s
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

```go
package main

import (
    "fmt"
    "github.com/lufeijun/goTools/ws/client"
    "github.com/lufeijun/goTools/ws/session"
)

func main() {
    c := client.NewClient(cfg)
    c.Connect()

    sess := c.Session()
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

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `session.go` | `Session` 接口、`State` 枚举、`defaultSession` 实现 |
| `heartbeat.go` | `Heartbeater` 接口、`perConnHeartbeater` 实现（基于时间轮） |
| `timingwheel.go` | 共享单级时间轮（128 slots，1s tick） |
| `reconnect.go` | `Reconnector` 接口、`reconnector` 实现（指数退避） |
| `session_test.go` | 状态流转、心跳启动、重连耗尽测试 |

---

## 注意事项

1. **V2.0 心跳基于共享时间轮** — 不再是 per-conn ticker goroutine，10 万连接的心跳 goroutine 从 10 万降至 1 个
2. **StateChan 容量 16** — 状态变化过快时会丢弃旧状态，业务监听应尽早读取
3. **重连不自动恢复 Pipeline** — V2.1 会补充重连后重新组装 Pipeline 的逻辑
4. **Session.Close() 同时关闭 Conn 和 stateChan** — 调用后连接不可再用，监听 StateChan 的 goroutine 会收到通道关闭信号
5. **心跳与重连均为 per-Session** — 但心跳 goroutine 已共享（时间轮），重连 goroutine 按需创建

# ws/session — 会话层

`ws/session` 在 `Conn` + `Pipeline` 之上管理连接的状态、心跳和自动重连，是业务代码直接交互的主要抽象之一。

---

## 设计定位

连接层（`conn`）只关心 I/O，而会话层关心**连接的生命周期**：

- **状态机**：连接从建立到关闭的完整状态流转
- **心跳保活**：定时发送 Ping 帧，检测连接是否存活
- **自动重连**：客户端断线后按策略自动重试

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
| `Close()` | 关闭会话，状态变为 Closed |

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

### 默认实现：perConnHeartbeater

```go
type perConnHeartbeater struct {
    pingInterval time.Duration
    pongTimeout  time.Duration
    stopChan     chan struct{}
    onTimeout    func()
}
```

**构造：**

```go
func NewPerConnHeartbeater(pingInterval, pongTimeout time.Duration) Heartbeater
```

**运行逻辑：**

```go
func (h *perConnHeartbeater) run(s Session) {
    ticker := time.NewTicker(h.pingInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            // TODO: send ping via conn pipeline (V2.1)
        case <-h.stopChan:
            return
        }
    }
}
```

当前状态：
- V2.0 中定时器已运行，但**实际 Ping 帧发送逻辑在 V2.1 完善**
- Pong 超时检测推迟到 V2.1（用时间轮在 conn 层拦截 Pong）
- 心跳设计改为**只发 Ping 不消费 ReadChan**，避免心跳与用户读消息冲突

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

**重连逻辑：**

```go
func (r *reconnector) Start(s Session) {
    s.SetState(StateReconnecting)
    for i := 0; i < r.maxRetries; i++ {
        select {
        case <-r.stopChan:
            return
        case <-time.After(r.interval):
        }

        s.SetState(StateConnecting)
        c, err := r.dial()
        if err != nil {
            s.SetState(StateReconnecting)
            continue
        }
        _ = c
        s.SetState(StateConnected)
        return
    }
    s.SetState(StateClosed)
}
```

- 每次重试间隔 `interval`
- 达到 `maxRetries` 后进入 `StateClosed`，不再重试
- `Stop()` 可中断重连过程

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
| `heartbeat.go` | `Heartbeater` 接口、`perConnHeartbeater` 实现 |
| `reconnect.go` | `Reconnector` 接口、`reconnector` 实现 |
| `session_test.go` | 状态流转、心跳启动、重连耗尽测试 |

---

## 注意事项

1. **V2.0 心跳为 stub** — 定时器运行，但实际 Ping 帧发送在 V2.1 完善
2. **StateChan 容量 16** — 状态变化过快时会丢弃旧状态，业务监听应尽早读取
3. **重连不自动恢复 Pipeline** — V2.1 会补充重连后重新组装 Pipeline 的逻辑
4. **Session.Close() 同时关闭 Conn** — 调用后连接不可再用
5. **心跳与重连均为 per-Session** — 每个 Session 有自己的 ticker goroutine，V2.2 可能改为共享时间轮

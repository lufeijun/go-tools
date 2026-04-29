# ws/hub — 连接管理中心

`ws/hub` 是所有 WebSocket 连接的集中管理中心，负责连接的注册、注销、广播和定向发送。v2 使用**分片锁（sharded lock）**替代 v1 的单 goroutine + channel 模型，解决百万连接下的广播瓶颈。

---

## 设计背景

v1 的 Hub 使用单 goroutine + channel 模式，所有操作串行化。当连接数达到百万级时：

- `Register` / `Unregister` 排队延迟
- `Broadcast` 需要遍历百万连接，单 goroutine 成为绝对瓶颈

v2 的分片锁方案：

- 将连接分散到 N 个 shard（默认 32）
- 每个 shard 独立 `sync.RWMutex`
- `Register` / `Unregister` / `Get` 只锁单个 shard
- `Broadcast` 并发遍历所有 shard（每个 shard 一个 goroutine）

---

## 核心接口

```go
type Hub interface {
    Register(s session.Session)
    Unregister(id uint64)
    Broadcast(msg conn.Message)
    Send(id uint64, msg conn.Message)
    Count() int
    Get(id uint64) session.Session
}
```

| 方法 | 说明 | 线程安全 |
|---|---|---|
| `Register(s)` | 注册新连接到 Hub | 是 |
| `Unregister(id)` | 按连接 ID 移除 | 是 |
| `Broadcast(msg)` | 给所有连接发消息 | 是（非阻塞） |
| `Send(id, msg)` | 给指定连接发消息 | 是 |
| `Count()` | 当前在线人数 | 是 |
| `Get(id)` | 按 ID 查找连接 | 是 |

---

## 默认实现：shardedHub

```go
type shardedHub struct {
    shardCount int
    shards     []*shard
}

type shard struct {
    mu    sync.RWMutex
    conns map[uint64]session.Session
}
```

### 分片算法

```go
func (h *shardedHub) shardIndex(id uint64) int {
    return int(id % uint64(h.shardCount))
}
```

- 按连接 ID 取模分配到 shard
- 分布均匀（`NextConnID()` 全局原子递增）

### 构造

```go
func NewHub(shardCount int) Hub
```

- `shardCount <= 0` 时默认使用 32
- 一般 32 或 64 就够了，百万连接场景可考虑 128

---

## 操作详解

### Register

```go
func (h *shardedHub) Register(s session.Session) {
    id := s.Conn().ID()
    sh := h.shards[h.shardIndex(id)]
    sh.mu.Lock()
    sh.conns[id] = s
    sh.mu.Unlock()
}
```

- 写锁单个 shard，不影响其他 shard 的读写

### Unregister

```go
func (h *shardedHub) Unregister(id uint64) {
    sh := h.shards[h.shardIndex(id)]
    sh.mu.Lock()
    delete(sh.conns, id)
    sh.mu.Unlock()
}
```

### Get

```go
func (h *shardedHub) Get(id uint64) session.Session {
    sh := h.shards[h.shardIndex(id)]
    sh.mu.RLock()
    defer sh.mu.RUnlock()
    return sh.conns[id]
}
```

- 读锁单个 shard，O(1) 查找

### Count

```go
func (h *shardedHub) Count() int {
    var total int
    for _, sh := range h.shards {
        sh.mu.RLock()
        total += len(sh.conns)
        sh.mu.RUnlock()
    }
    return total
}
```

- 遍历所有 shard 读锁求和
- 非精确实时值（各 shard 锁时间点不同），适用于监控统计

### Broadcast

```go
func (h *shardedHub) Broadcast(msg conn.Message) {
    var wg sync.WaitGroup
    for _, sh := range h.shards {
        wg.Add(1)
        go func(s *shard) {
            defer wg.Done()
            s.mu.RLock()
            sessions := make([]session.Session, 0, len(s.conns))
            for _, sess := range s.conns {
                sessions = append(sessions, sess)
            }
            s.mu.RUnlock()

            for _, sess := range sessions {
                // TODO: write to pipeline (V2.1)
            }
        }(sh)
    }
    wg.Wait()
}
```

**关键设计：**

1. **每个 shard 一个 goroutine** — 32 个 shard 并发广播，锁竞争降低 32 倍
2. **先拷贝再解锁** — `RLock` 期间只拷贝 Session 引用，不执行 I/O，减少锁持有时间
3. **非阻塞写入** — 对慢连接直接跳过，避免广播 goroutine 阻塞在单个慢连接上
4. `wg.Wait()` 等待所有 shard 完成

### Send（定向发送）

```go
func (h *shardedHub) Send(id uint64, msg conn.Message) {
    s := h.Get(id)
    if s == nil {
        return
    }
    // TODO: write to pipeline (V2.1)
}
```

---

## 使用示例：聊天室

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/hub"
    "github.com/lufeijun/goTools/ws/server"
)

func main() {
    h := hub.NewHub(32)
    srv := server.NewServer(ws.Config{Addr: ":8080"})

    go func() {
        ticker := time.NewTicker(10 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            if count := h.Count(); count > 0 {
                fmt.Printf("当前在线: %d\n", count)
            }
        }
    }()

    log.Println("聊天室启动，监听 :8080 ...")
    srv.Start()

    // 广播系统公告
    h.Broadcast(conn.Message{
        Type: 0x1,
        Data: []byte("系统公告：欢迎来到聊天室！"),
    })

    // 给指定用户发私信
    h.Send(42, conn.Message{
        Type: 0x1,
        Data: []byte("这是给你的私信"),
    })
}
```

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `hub.go` | `Hub` 接口 + `shardedHub` 分片锁实现 |
| `hub_test.go` | Register/Unregister/Get/Count/Broadcast 测试 |

---

## 注意事项

1. **Broadcast 在 V2.0 为 stub** — 已并发遍历 shard，但实际 Pipeline 写入在 V2.1 完善
2. **shardCount 选择权衡** — 越大并发性能越好，但内存占用越大（每个 shard 一个 map）
3. **Register/Unregister 频繁时仍有竞争** — 分片锁将竞争限制在单个 shard 内，但同一 shard 的写操作仍串行
4. **Session 引用在 Broadcast 时拷贝** — 防止 RLock 期间执行 I/O 导致锁持有时间过长
5. **Count() 非精确值** — 遍历多个锁的瞬时值之和，适合监控，不适合严格一致性判断

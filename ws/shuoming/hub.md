# ws/hub — 连接管理中心

`ws/hub` 是所有 WebSocket 连接的集中管理中心，负责连接的注册、注销、广播和定向发送。v2 使用**分片锁（sharded lock）+ 缓存行对齐 + 固定 worker pool** 替代 v1 的单 goroutine + channel 模型，解决百万连接下的广播瓶颈。

---

## 设计背景

v1 的 Hub 使用单 goroutine + channel 模式，所有操作串行化。当连接数达到百万级时：

- `Register` / `Unregister` 排队延迟
- `Broadcast` 需要遍历百万连接，单 goroutine 成为绝对瓶颈

v2 的分片锁方案：

- 将连接分散到 N 个 shard（默认 32）
- 每个 shard 独立 `sync.RWMutex`
- `Register` / `Unregister` / `Get` 只锁单个 shard
- `Broadcast` 通过固定 worker pool 并发发送

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

### 缓存行对齐（P2 优化 5.3）

```go
type shardedHub struct {
    shardCount int
    shards     []*shard
}

type shard struct {
    mu    sync.RWMutex
    _pad  [56]byte        // cache line padding（64 字节对齐）
    conns map[uint64]session.Session
}
```

- 32 个 shard 分布在不同的 CPU cache line 上
- 高并发广播时，不同核访问不同 shard 不会因 cache line 共享产生 false sharing
- `_pad [56]byte` 确保 `mu`（约 8 字节）独占一个 64 字节 cache line

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

### Broadcast（P2 优化 1.2）

**原始问题：** 每次 `Broadcast()` 创建 32 个临时 goroutine，高频广播时创建/销毁开销大。

**优化方案：** 预置固定 worker pool，每个 worker 负责一个 shard：

```go
func (h *shardedHub) Broadcast(msg conn.Message) {
    for _, sh := range h.shards {
        select {
        case sh.msgCh <- msg:  // 投递到 shard 的 buffered channel
        default:
            // channel 满，跳过慢 shard（背压）
        }
    }
}
```

每个 shard 的 worker：

```go
func (sh *shard) run() {
    for msg := range sh.msgCh {
        sh.mu.RLock()
        sessions := make([]session.Session, 0, len(sh.conns))
        for _, sess := range sh.conns {
            sessions = append(sessions, sess)
        }
        sh.mu.RUnlock()

        for _, sess := range sessions {
            sess.Conn().Pipeline().FireChannelWrite(msg)
        }
    }
}
```

**关键设计：**

1. **固定 worker goroutine** — 启动时创建，运行时无 goroutine 创建/销毁开销
2. **buffered channel（256 msg）** — `Broadcast()` 非阻塞投递，channel 满时直接跳过（背压）
3. **先拷贝再解锁** — `RLock` 期间只拷贝 Session 引用，不执行 I/O，减少锁持有时间
4. **实际发送通过 Pipeline** — 调用 `FireChannelWrite(msg)`，由 FrameCodec 编码并发送

### Send（定向发送）

```go
func (h *shardedHub) Send(id uint64, msg conn.Message) {
    s := h.Get(id)
    if s == nil {
        return
    }
    s.Conn().Pipeline().FireChannelWrite(msg)
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

    srv.OnConnect(func(sess session.Session) {
        h.Register(sess)
    })

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

## Benchmark 基线

```go
BenchmarkHub_Broadcast_1K   // 1000 连接广播
BenchmarkHub_Broadcast_10K  // 10000 连接广播
```

运行：`go test ./ws/hub -bench=. -benchmem`

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `hub.go` | `Hub` 接口 + `shardedHub` 分片锁实现（缓存行对齐 + 固定 worker pool） |
| `hub_test.go` | Register/Unregister/Get/Count/Broadcast 测试 |
| `hub_bench_test.go` | 广播性能基准测试（1K/10K 连接） |

---

## 注意事项

1. **Broadcast 已实现消息投递** — 通过 `sess.Conn().Pipeline().FireChannelWrite(msg)` 发送
2. **shardCount 选择权衡** — 越大并发性能越好，但内存占用越大（每个 shard 一个 map + worker）
3. **Register/Unregister 频繁时仍有竞争** — 分片锁将竞争限制在单个 shard 内，但同一 shard 的写操作仍串行
4. **Session 引用在 Broadcast 时拷贝** — 防止 RLock 期间执行 I/O 导致锁持有时间过长
5. **Count() 非精确值** — 遍历多个锁的瞬时值之和，适合监控，不适合严格一致性判断
6. **Broadcast 支持背压** — 慢 shard 的 channel 满时直接跳过，避免广播被拖住

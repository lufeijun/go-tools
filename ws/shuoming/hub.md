# ws/hub — 连接管理中心

`ws/hub` 是所有 WebSocket 连接的集中管理中心，负责连接的注册、注销、广播、定向发送和批量关闭。v2 使用**分片锁（sharded lock）+ 缓存行对齐 + 固定 worker pool** 替代 v1 的单 goroutine + channel 模型，解决百万连接下的广播瓶颈。

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
    CloseAll()
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
| `CloseAll()` | 关闭所有已注册的连接 | 是 |

---

## 默认实现：shardedHub

### 结构体

```go
type shardedHub struct {
    shardCount int
    shards     []*shard
    workers    []*broadcastWorker   // 每个 shard 对应一个广播 worker
}
```

**关键变化（v2 最新）：**

- 新增 `workers []*broadcastWorker` — 每个 shard 对应一个常驻广播 worker goroutine
- 新增 `CloseAll()` 方法 — 用于 Server 优雅关闭时批量关闭所有连接

### broadcastWorker 结构

```go
type broadcastWorker struct {
    shard *shard
    ch    chan conn.Message   // 消息投递通道（容量 256）
}
```

- 每个 worker 绑定一个 shard，负责将收到的消息发送给该 shard 中的所有 Session
- `ch` 是缓冲容量为 256 的 channel，`Broadcast()` 非阻塞投递

### 缓存行对齐

```go
type shard struct {
    mu    sync.RWMutex
    conns map[uint64]session.Session
    // Pad to a full cache line to prevent false sharing between shards.
    _ [cacheLineSize - int(unsafe.Sizeof(sync.RWMutex{})) - int(unsafe.Sizeof(map[uint64]session.Session{}))]byte
}
```

- 32 个 shard 分布在不同的 CPU cache line 上
- 高并发广播时，不同核访问不同 shard 不会因 cache line 共享产生 false sharing
- `_pad` 确保每个 shard 的 `mu` 和 `conns` 独占一个 64 字节 cache line

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
func NewHub(shardCount int) Hub {
    if shardCount <= 0 {
        shardCount = 32
    }
    h := &shardedHub{
        shardCount: shardCount,
        shards:     make([]*shard, shardCount),
        workers:    make([]*broadcastWorker, shardCount),
    }
    for i := range h.shards {
        h.shards[i] = &shard{conns: make(map[uint64]session.Session)}
        h.workers[i] = &broadcastWorker{shard: h.shards[i], ch: make(chan conn.Message, broadcastQueueSize)}
        go h.workers[i].run()   // 启动常驻 worker goroutine
    }
    return h
}
```

- `shardCount <= 0` 时默认使用 32
- 一般 32 或 64 就够了，百万连接场景可考虑 128
- 每个 shard 创建时同步启动对应的 broadcast worker goroutine

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

### Broadcast（基于 worker pool）

**原始问题：** 每次 `Broadcast()` 创建 32 个临时 goroutine，高频广播时创建/销毁开销大。

**优化方案：** 预置固定 worker pool，每个 worker 负责一个 shard：

```go
func (h *shardedHub) Broadcast(msg conn.Message) {
    for _, w := range h.workers {
        select {
        case w.ch <- msg:    // 投递到 worker 的 buffered channel
        default:
            // channel 满，跳过慢 shard（背压）
        }
    }
}
```

每个 worker 的运行逻辑：

```go
func (w *broadcastWorker) run() {
    for msg := range w.ch {
        w.shard.mu.RLock()
        // 先拷贝 Session 引用，释放锁后再发送
        sessions := make([]session.Session, 0, len(w.shard.conns))
        for _, sess := range w.shard.conns {
            sessions = append(sessions, sess)
        }
        w.shard.mu.RUnlock()

        for _, sess := range sessions {
            sess.Conn().Pipeline().FireChannelWrite(&msg)
        }
    }
}
```

**关键设计：**

1. **固定 worker goroutine** — 启动时创建（在 `NewHub` 中），运行时无 goroutine 创建/销毁开销
2. **buffered channel（256 msg）** — `Broadcast()` 非阻塞投递，channel 满时直接跳过（背压）
3. **先拷贝再解锁** — `RLock` 期间只拷贝 Session 引用，不执行 I/O，减少锁持有时间
4. **实际发送通过 Pipeline** — 调用 `FireChannelWrite(&msg)`，由 FrameCodec 编码并发送
5. **消息值传递** — `Broadcast` 投递 `msg`，worker 内用 `&msg` 传递指针，避免对每个连接拷贝

### Send（定向发送）

```go
func (h *shardedHub) Send(id uint64, msg conn.Message) {
    s := h.Get(id)
    if s == nil {
        return
    }
    s.Conn().Pipeline().FireChannelWrite(&msg)
}
```

### CloseAll（批量关闭）

```go
func (h *shardedHub) CloseAll() {
    for _, sh := range h.shards {
        // 第一步：在读锁内拷贝所有 Session 引用
        sh.mu.RLock()
        sessions := make([]session.Session, 0, len(sh.conns))
        for _, sess := range sh.conns {
            sessions = append(sessions, sess)
        }
        sh.mu.RUnlock()

        // 第二步：在锁外逐个关闭 Session
        for _, sess := range sessions {
            sess.Close()
        }
    }
}
```

**为什么先拷贝再关闭：**

1. **Close 可能耗时** — 调用 `sess.Close()` 会触发 `conn.Close()`，可能涉及 I/O 操作（发送 Close 帧、等待对端 ACK 等）
2. **不能在锁内做 I/O** — 如果在 `RLock` 期间调用 `sess.Close()`，锁持有时间不可控，影响其他操作
3. **用 RLock 而非 Lock** — 拷贝期间允许其他 goroutine 继续操作（如 Unregister），不阻塞并发操作

**使用场景：** `Server.Stop()` 调用 `CloseAll()` 关闭所有已注册的连接，实现优雅关闭：

```go
func (s *defaultServer) Stop() error {
    if err := s.acceptor.Close(); err != nil {
        return err
    }
    // 关闭所有已注册的连接，让 serveConn goroutine 可以退出
    s.hub.CloseAll()
    s.wg.Wait()   // 等待所有 serveConn goroutine 退出
    return nil
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
    "github.com/lufeijun/goTools/ws/session"
)

func main() {
    cfg := ws.DefaultConfig()
    cfg.Addr = ":8080"

    srv, err := server.NewServer(cfg)
    if err != nil {
        log.Fatal(err)
    }

    h := hub.NewHub(32)

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
| `hub.go` | `Hub` 接口 + `shardedHub` 分片锁实现（缓存行对齐 + 固定 worker pool + CloseAll） |
| `hub_test.go` | Register/Unregister/Get/Count/Broadcast/CloseAll 测试 |
| `hub_bench_test.go` | 广播性能基准测试（1K/10K 连接） |

---

## 注意事项

1. **Broadcast 已实现消息投递** — 通过 `sess.Conn().Pipeline().FireChannelWrite(&msg)` 发送
2. **shardCount 选择权衡** — 越大并发性能越好，但内存占用越大（每个 shard 一个 map + worker）
3. **Register/Unregister 频繁时仍有竞争** — 分片锁将竞争限制在单个 shard 内，但同一 shard 的写操作仍串行
4. **Session 引用在 Broadcast 时拷贝** — 防止 RLock 期间执行 I/O 导致锁持有时间过长
5. **Count() 非精确值** — 遍历多个锁的瞬时值之和，适合监控，不适合严格一致性判断
6. **Broadcast 支持背压** — 慢 shard 的 channel 满时直接跳过，避免广播被拖住
7. **CloseAll 必须先拷贝再关闭** — 在锁外调用 sess.Close()，避免锁持有时间不可控
8. **CloseAll 由 Server.Stop() 调用** — 用于优雅关闭，关闭所有连接后等待 serveConn goroutine 退出
9. **Worker 常驻 goroutine** — 每个 shard 启动时创建一个 worker goroutine，生命周期与 Hub 相同

# ws/eventloop — 跨平台事件驱动层

`ws/eventloop` 是 v2 的高并发基石，负责将操作系统底层的 I/O 多路复用机制（epoll/kqueue）封装为统一的 Go 接口，实现**主从 Reactor 模型** + **goroutine pool 异步调度**。

---

## 设计背景

v1 采用 goroutine-per-conn 模型，每个连接 2-3 个 goroutine。百万连接 = 200-300 万 goroutine = 4-6GB 栈内存，Go 调度器不堪重负。

v2 的事件驱动目标：

- **空闲连接零 goroutine 开销** — 连接建立后不绑定专属 goroutine
- **有数据时 EventLoop goroutine 处理** — 或提交到业务 goroutine 池
- **跨平台统一抽象** — Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP 预留)
- **慢 Handler 不阻塞 Loop** — 异步 dispatch 到 worker pool

---

## 核心接口

### EventHandler

由 `conn` 层实现，eventloop 只操作 fd 和回调：

```go
type EventHandler interface {
    OnEvent(fd int, events uint32)
}
```

### EventLoop

每个 EventLoop 绑定一个 goroutine，管理一组连接的 I/O：

```go
type EventLoop interface {
    Register(fd int, handler EventHandler) error  // 将 fd 注册到事件循环
    Deregister(fd int) error                      // 注销 fd
    Mod(fd int, events uint32) error              // 修改 fd 的事件掩码（如注册 EPOLLOUT）
    Wake()                                        // 唤醒事件循环（跨 goroutine 写）
    Run() error                                   // 阻塞运行事件循环
    Stop() error                                  // 停止事件循环
}
```

### Poller

底层系统调用抽象，平台相关：

```go
type Poller interface {
    Open() error
    Close() error
    Add(fd int, events uint32) error
    Mod(fd int, events uint32) error
    Del(fd int) error
    Wait(timeoutMs int) ([]Event, error)
}
```

### Event

```go
type Event struct {
    FD     int
    Events uint32
}

const (
    EventRead  uint32 = 1 << iota
    EventWrite
    EventError
    EventHup
)
```

---

## 主从 Reactor 模型

```
+------------------+
| MainEventLoop    |  <-- 1 个，只负责 Accept 新连接
| (epoll/kqueue)   |
+--------+---------+
         | 新连接
         v
+------------------+     +------------------+
| SubEventLoop 0   |     | SubEventLoop 1   |  <-- N 个（默认 N = CPU 核数）
| (epoll/kqueue)   | ... | (epoll/kqueue)   |     每个管理一组连接的 I/O
| + worker pool    |     | + worker pool    |     Handler 异步调度，不阻塞 Loop
+------------------+     +------------------+
```

- **MainEventLoop**：1 个，监听 Listen Socket 的 `Accept` 事件
- **SubEventLoopGroup**：N 个，每个 SubEventLoop 管理一组已建立连接的 Read/Write 事件
- **负载均衡**：新连接通过轮询（round-robin）分配到某个 SubEventLoop

---

## EventLoopGroup

`EventLoopGroup` 管理一组 EventLoop，提供统一的生命周期管理和负载均衡接口。

### 接口

```go
type EventLoopGroup interface {
    Start() error       // 启动所有 EventLoop
    Stop() error        // 停止所有 EventLoop
    Next() EventLoop    // 返回下一个 EventLoop（负载均衡）
    Count() int         // 返回 EventLoop 数量
}
```

### 默认实现：roundRobinEventLoopGroup

```go
type roundRobinEventLoopGroup struct {
    loops  []EventLoop
    nextFd uint64   // 原子计数器，用于 round-robin
}
```

**构造函数：**

```go
func NewEventLoopGroup(workers int, newPoller func() Poller) EventLoopGroup {
    if workers <= 0 {
        workers = 1
    }
    loops := make([]EventLoop, workers)
    for i := range loops {
        p := newPoller()                          // 每个循环创建独立的 Poller
        loops[i] = NewEventLoopWithPool(p, 1)     // 每个 EventLoop 带 1 个 worker
    }
    return &roundRobinEventLoopGroup{
        loops:  loops,
        nextFd: 0,
    }
}
```

**要点：**

1. **每个 EventLoop 有自己的 Poller** — 实例独立的 epoll/kqueue fd，避免单 fd 瓶颈
2. **newPoller 工厂函数** — 由调用方决定创建哪种 Poller，解耦平台依赖
3. **workers 默认为 1** — 每个 EventLoop 的 worker pool 大小为 1，避免与外层 worker 池冲突
4. **NewEventLoopWithPool 内部调用 poller.Open()** — Poller 在构造时就完成初始化，`Run()` 不再调用 `Open()`

**Start — 启动所有循环：**

```go
func (g *roundRobinEventLoopGroup) Start() error {
    for _, el := range g.loops {
        go el.Run()     // 每个 EventLoop 在独立 goroutine 中运行
    }
    return nil
}
```

**Stop — 停止所有循环：**

```go
func (g *roundRobinEventLoopGroup) Stop() error {
    for _, el := range g.loops {
        _ = el.Stop()
    }
    return nil
}
```

**Next — round-robin 负载均衡：**

```go
func (g *roundRobinEventLoopGroup) Next() EventLoop {
    n := atomic.AddUint64(&g.nextFd, 1)
    return g.loops[(n-1)%uint64(len(g.loops))]
}
```

- 使用 `atomic.AddUint64` 实现无锁 round-robin
- `n-1` 确保首次调用返回 `loops[0]`
- 对 `len(loops)` 取模实现均匀分布

**Count — 返回循环数量：**

```go
func (g *roundRobinEventLoopGroup) Count() int {
    return len(g.loops)
}
```

---

## 默认实现：defaultEventLoop

`eventloop.go` 中的 `defaultEventLoop` 是标准实现。

```go
type defaultEventLoop struct {
    poller      Poller
    handlersVal atomic.Value // stores map[int]EventHandler — copy-on-write
    running     int32
    stopCh      chan struct{}
    pool        *workerPool   // 固定 goroutine pool 异步调度 Handler
}
```

### 构造函数

```go
func NewEventLoop(p Poller) EventLoop {
    return NewEventLoopWithPool(p, runtime.GOMAXPROCS(0))
}

func NewEventLoopWithPool(p Poller, workers int) EventLoop {
    el := &defaultEventLoop{
        poller: p,
        stopCh: make(chan struct{}),
        pool:   newWorkerPool(workers),
    }
    el.handlersVal.Store(make(map[int]EventHandler))
    el.poller.Open()    // 在构造时调用 Open()，初始化 poller
    return el
}
```

**重要：** `poller.Open()` 在构造函数中调用，**不在 `Run()` 中调用**。这意味着：

- EventLoop 创建后 Poller 就已经就绪
- 可以在 `Run()` 之前调用 `Register()` 注册 fd
- `Run()` 只负责启动事件循环

### copy-on-write handler 查找

**原始问题：** 每处理一个事件都要对全局 `handlers map` 加 `RLock`。

**优化方案：** 使用 `atomic.Value` 存储只读 handlers map 快照：

```go
func (el *defaultEventLoop) loadHandlers() map[int]EventHandler {
    return el.handlersVal.Load().(map[int]EventHandler)
}

func (el *defaultEventLoop) storeHandler(fd int, handler EventHandler) {
    old := el.loadHandlers()
    newMap := make(map[int]EventHandler, len(old)+1)
    for k, v := range old {
        newMap[k] = v
    }
    newMap[fd] = handler
    el.handlersVal.Store(newMap)
}
```

- `Register` / `Deregister` 时复制新 map 原子替换
- `Run` 循环中通过 `atomic.Value.Load()` 获取只读快照，**零锁竞争**
- 相比 `sync.RWMutex`，消除了事件分发路径上的所有锁开销

### goroutine pool 异步调度

**原始问题：** 在 EventLoop 主 goroutine 同步调用 `h.OnEvent()`，慢 Handler 卡住整个 Loop。

**优化方案：** 引入固定 goroutine pool，将 `OnEvent()` 投递到 pool 异步执行：

```go
for _, e := range events {
    h, ok := el.loadHandlers()[e.FD]
    if ok {
        el.pool.submit(func() {
            h.OnEvent(e.FD, e.Events)
        })
    }
}
```

**pool 实现：**

```go
type workerPool struct {
    taskCh chan func()
    stopCh chan struct{}
}

func newWorkerPool(workers int) *workerPool {
    if workers <= 0 {
        workers = runtime.GOMAXPROCS(0)
    }
    p := &workerPool{
        taskCh: make(chan func(), 1024),
        stopCh: make(chan struct{}),
    }
    for i := 0; i < workers; i++ {
        go p.run()
    }
    return p
}
```

- pool 大小默认 `GOMAXPROCS`，每个 worker 一个 goroutine
- 任务队列长度 1024，支持一定背压
- 慢 Handler 不影响其他连接的事件响应
- `Stop()` 时优雅关闭 pool

### Run 循环

```go
func (el *defaultEventLoop) Run() error {
    if !atomic.CompareAndSwapInt32(&el.running, 0, 1) {
        return errors.New("already running")
    }
    defer atomic.StoreInt32(&el.running, 0)

    // 注意：poller.Open() 已在构造函数中调用，这里不再调用

    for {
        select {
        case <-el.stopCh:
            return nil
        default:
        }

        events, err := el.poller.Wait(100)
        if err != nil {
            return err
        }

        for _, e := range events {
            h, ok := el.loadHandlers()[e.FD]
            if ok {
                el.pool.submit(func() {
                    h.OnEvent(e.FD, e.Events)
                })
            }
        }
    }
}
```

**关键点：**

1. **poller.Open() 在构造时调用** — `Run()` 不负责初始化 Poller
2. **单次入口** — `CompareAndSwapInt32` 保证一个 EventLoop 只能被 Run 一次
3. **非阻塞检测停止信号** — `select + default` 每轮循环检查 `stopCh`
4. **Wait 超时 100ms** — 兼顾响应速度和 CPU 空闲开销
5. **事件通过 worker pool 异步分发** — 避免慢 Handler 阻塞 EventLoop

### Stop 资源清理

```go
func (el *defaultEventLoop) Stop() error {
    if !atomic.CompareAndSwapInt32(&el.running, 1, 0) {
        return errors.New("not running")
    }
    close(el.stopCh)
    el.pool.stop()

    // Deregister all fds and close the poller
    handlers := el.loadHandlers()
    fds := make([]int, 0, len(handlers))
    for fd := range handlers {
        fds = append(fds, fd)
    }
    el.handlersVal.Store(make(map[int]EventHandler))

    for _, fd := range fds {
        _ = el.poller.Del(fd)
    }
    _ = el.poller.Close()
    return nil
}
```

- 遍历所有已注册 fd 调用 `Deregister`
- 调用 `poller.Close()` 关闭 epoll/kqueue fd
- 清空 handlers map，防止残留状态

---

## 平台实现

### Linux：epoll（epoll_linux.go）

```go
//go:build linux

type epollPoller struct {
    epfd int  // epoll 文件描述符
}
```

- `Open()`：`EpollCreate1(EPOLL_CLOEXEC)`
- `Add/Mod/Del`：`EpollCtl` 对应 `EPOLL_CTL_ADD/MOD/DEL`
- `Wait()`：`EpollWait`，每次最多处理 1024 个事件
- **边缘触发（EPOLLET）**：减少事件重复通知
- `Mod()` 支持：动态注册/注销 `EPOLLOUT` 写事件

**导出函数 NewEpollPoller：**

```go
// NewEpollPoller creates a new epoll-based Poller.
// 导出供 Acceptor 和 Client 直接使用。
func NewEpollPoller() Poller {
    return newEpollPoller()
}
```

> 之前 `newEpollPoller()` 是包内私有函数，现在导出为 `NewEpollPoller()`，供 `epollAcceptor` 和客户端 epoll 模式直接创建 Poller。

事件转换：

```go
func epollEvents(events uint32) uint32 {
    var e uint32
    if events&EventRead != 0  { e |= unix.EPOLLIN }
    if events&EventWrite != 0 { e |= unix.EPOLLOUT }
    if events&EventError != 0 { e |= unix.EPOLLERR }
    if events&EventHup != 0   { e |= unix.EPOLLHUP | unix.EPOLLRDHUP }
    return e | unix.EPOLLET
}
```

### BSD：kqueue（kqueue_bsd.go）

```go
//go:build darwin || freebsd || openbsd

type kqueuePoller struct {
    kqfd int  // kqueue 文件描述符
}
```

- `Open()`：`Kqueue()`
- `Add()`：`Kevent` 注册 `EV_ADD`
- `Mod()`：kqueue 用相同 ident 覆盖，内部调用 `Add`
- `Del()`：`Kevent` 注册 `EV_DELETE`
- `Wait()`：`Kevent`，按 fd 聚合 `EVFILT_READ` / `EVFILT_WRITE`

事件聚合逻辑：

```go
fdEvents := make(map[int]uint32)
for i := 0; i < n; i++ {
    ev := kevents[i]
    fd := int(ev.Ident)
    switch ev.Filter {
    case unix.EVFILT_READ:  fdEvents[fd] |= EventRead
    case unix.EVFILT_WRITE: fdEvents[fd] |= EventWrite
    }
    if ev.Flags&unix.EV_ERROR != 0 { fdEvents[fd] |= EventError }
    if ev.Flags&unix.EV_EOF != 0   { fdEvents[fd] |= EventHup }
}
```

### Windows：IOCP（预留）

V2.1 实现，当前仅预留接口位置。

---

## Acceptor 交互

在 epoll 模式下，`epollAcceptor` 使用 EventLoopGroup 实现主从 Reactor 模型：

### 创建流程

```go
func (a *epollAcceptor) Listen(addr string) error {
    // ... 创建 listen fd, bind, listen ...

    // 创建 SubEventLoopGroup（从 Reactor）
    workers := a.config.EventLoopWorkerCount()
    a.elg = eventloop.NewEventLoopGroup(workers, func() eventloop.Poller {
        return eventloop.NewEpollPoller()
    })
    a.elg.Start()   // 启动所有 sub-loop

    // 创建 MainEventLoop（主 Reactor）
    mainPoller := eventloop.NewEpollPoller()
    a.mainLoop = eventloop.NewEventLoop(mainPoller)
    a.mainLoop.Register(fd, &acceptHandler{acceptor: a})

    go a.mainLoop.Run()   // 启动 main-loop
    return nil
}
```

### 连接分发

```
MainEventLoop (Accept)
    |
    | 新连接到达
    v
acceptHandler.OnEvent()
    |
    | Accept 新 fd
    v
epollAcceptor.handleNewConn(clientFd)
    |
    | WebSocket 握手 + 创建 EpollConn
    v
el := a.elg.Next()          // round-robin 选择一个 SubEventLoop
ec.SetEventLoop(el)          // 绑定到选中的 loop
el.Register(handshakeFd, &conn.EventHandlerAdapter{Conn: ec})
    |
    v
SubEventLoop (连接 I/O)
```

**关键点：**

1. **MainEventLoop 只负责 Accept** — 监听 listen fd 的可读事件
2. **每个新连接分配到 SubEventLoop** — 通过 `elg.Next()` round-robin 选择
3. **EpollConn 绑定到对应的 SubEventLoop** — 后续所有 I/O 事件由该 loop 处理
4. **SubEventLoop 的 worker pool 处理业务逻辑** — 慢 Handler 不阻塞 loop

---

## 循环依赖解决

`eventloop.EventLoop.Register` 原本需要 `conn.Conn`，但 `conn.EventDrivenConn.SetEventLoop` 又需要 `eventloop.EventLoop`，形成循环依赖。

**解决方案：**

1. `EventLoop.Register` 只依赖 `EventHandler` 接口（conn 实现它）
2. `EventDrivenConn.SetEventLoop(el interface{})` 使用 `interface{}` 避免直接依赖 `eventloop` 包

```go
type EventDrivenConn interface {
    Conn
    FD() int
    OnEvent(events uint32)
    SetEventLoop(el interface{})  // 实际类型为 eventloop.EventLoop
    SetOnFrame(fn func(frame.Frame))
    SetOnClose(fn func())
}
```

---

## 跨平台编译

| 平台 | 编译标签 | 实现文件 |
|---|---|---|
| Linux | `//go:build linux` | `epoll_linux.go` |
| macOS / FreeBSD / OpenBSD | `//go:build darwin \|\| freebsd \|\| openbsd` | `kqueue_bsd.go` |
| Windows | `//go:build windows` | 预留 |

运行时选择（V2.1）：

```go
func NewPoller() (Poller, error) {
    switch runtime.GOOS {
    case "linux":
        return newEpollPoller()
    case "darwin", "freebsd", "openbsd":
        return newKqueuePoller()
    default:
        return nil, errors.New("unsupported platform")
    }
}
```

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `eventloop.go` | `EventLoop`、`Poller`、`EventHandler` 接口 + `defaultEventLoop` 实现（copy-on-write + worker pool） |
| `group.go` | `EventLoopGroup` 接口 + `roundRobinEventLoopGroup` 实现（round-robin 负载均衡） |
| `epoll_linux.go` | Linux epoll `Poller` 实现（含 `NewEpollPoller` 导出 + Mod 支持） |
| `epoll_linux_test.go` | epoll 生命周期、Add/Del/Mod 测试 |
| `kqueue_bsd.go` | BSD kqueue `Poller` 实现 |
| `kqueue_bsd_test.go` | kqueue 生命周期、Add/Del 测试 |
| `eventloop_test.go` | EventLoop 注册/注销、事件分发、Mock Poller 测试（含 race 检测） |

---

## 注意事项

1. **poller.Open() 在构造时调用** — `NewEventLoopWithPool` 中调用，`Run()` 不再调用。可以在 `Run()` 之前 `Register()` fd
2. **NewEpollPoller 已导出** — Acceptor 和 Client 可以直接使用 `eventloop.NewEpollPoller()` 创建 Poller
3. **epollConn 在 V2.1 完善** — V2.0 中 `epollConn.Read`/`Write` 使用非阻塞 syscall 的完整实现尚未交付，当前生产环境使用 `netConn` fallback
4. **Wake() 尚未实现** — 跨 goroutine 唤醒 EventLoop 需要 eventfd / pipe，V2.1 补充
5. **EINTR 处理** — `EpollWait`/`Kevent` 被信号中断时返回 nil，继续下一轮循环
6. **maxEvents = 1024** — 单次 `Wait` 最多处理 1024 个事件，超大并发下需确保 EventLoop 足够快（worker pool 已解决慢 Handler 阻塞问题）
7. **Mod() 用于动态注册写事件** — epollConn 写不下时注册 `EPOLLOUT`，等事件再继续写
8. **EventLoopGroup 使用 atomic round-robin** — 无锁分发，高并发下性能好
9. **每个 SubEventLoop worker pool 大小为 1** — Group 层面已经分散了连接，单 worker 足够

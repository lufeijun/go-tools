# ws/conn — 连接层

conn 包是 WebSocket 连接层的核心抽象，提供了连接的接口定义、两种实现（标准 net.Conn 和事件驱动 epoll）、RFC 6455 握手协议、帧编解码器，以及 TCP 参数优化。它是整个 ws 库承上启下的关键层：

- **向上**：为 `session` 层提供 `Conn` 接口，隐藏底层 I/O 差异
- **向下**：对接 `eventloop`（事件驱动）或标准 `net.Conn`（fallback）
- **横向**：每个 `Conn` 自带 `ChannelPipeline`，数据在 Pipeline 上流动

---

## 核心接口

### Conn 接口

`Conn` 是所有 WebSocket 连接的统一抽象。无论是基于标准 `net.Conn` 还是基于原始 fd 的 epoll 连接，都实现了这个接口。上层代码（如 `session`、`hub`）只依赖 `Conn` 接口，不需要关心底层实现。

```go
type Conn interface {
    ID() uint64
    Pipeline() pipeline.ChannelPipeline
    Read(b buf.ByteBuf) error
    Write(b buf.ByteBuf) error
    RemoteAddr() net.Addr
    LocalAddr() net.Addr
    IsClient() bool
    Close() error
    Active() bool
}
```

各方法说明：

| 方法 | 返回值 | 说明 |
|---|---|---|
| `ID()` | `uint64` | 连接的唯一标识符，由全局原子计数器生成，保证整个进程内唯一 |
| `Pipeline()` | `pipeline.ChannelPipeline` | 每个连接独立的处理器链，用于入站/出站数据流经各 Handler |
| `Read(b)` | `error` | 从连接读取数据到 ByteBuf。在事件驱动模型中，数据已被 EventLoop 预读到内部缓冲区，此方法不会阻塞 |
| `Write(b)` | `error` | 将 ByteBuf 数据写入连接。对 netConn 是同步写；对 EpollConn 是非阻塞写，写不完时注册 EPOLLOUT |
| `RemoteAddr()` | `net.Addr` | 对端地址（IP + 端口） |
| `LocalAddr()` | `net.Addr` | 本地地址（IP + 端口） |
| `IsClient()` | `bool` | 是否为客户端发起的连接。影响帧掩码行为：客户端必须掩码，服务端不掩码 |
| `Close()` | `error` | 关闭连接。内部使用 `sync.Once` 保证只关闭一次 |
| `Active()` | `bool` | 连接是否仍活跃。通过原子标志判断，线程安全 |

### EventDrivenConn 接口

`EventDrivenConn` 扩展了 `Conn` 接口，专为事件驱动（epoll）连接设计。只有 `EpollConn` 实现了这个接口。

```go
type EventDrivenConn interface {
    Conn
    FD() int
    OnEvent(events uint32)
    SetEventLoop(el interface{})
    SetOnFrame(fn func(frame.Frame))
    SetOnClose(fn func())
}
```

各方法说明：

| 方法 | 说明 |
|---|---|
| `FD()` | 返回底层原始文件描述符（file descriptor）。EventLoop 通过 fd 来注册/修改/取消 epoll 事件 |
| `OnEvent(events)` | 由 EventLoop 在 fd 就绪时调用。`events` 是 epoll 事件位掩码（EPOLLIN / EPOLLOUT / EPOLLERR / EPOLLHUP），方法内部根据事件类型分发到 `handleReadEvent` / `handleWriteEvent` / `Close` |
| `SetEventLoop(el)` | 设置 EventLoop 引用。参数类型为 `interface{}` 而非 `eventloop.EventLoop`，这是为了**避免循环依赖**——conn 包不能直接导入 eventloop 包的类型。内部会做类型断言 |
| `SetOnFrame(fn)` | 注册帧回调函数。当 IncrementalParser 解析出一个完整帧时，调用此回调。典型用途：服务端在回调中触发 Pipeline 的 `FireChannelRead` |
| `SetOnClose(fn)` | 注册关闭回调函数。当连接关闭时调用。典型用途：服务端在回调中设置 Session 状态为 Disconnected |

### Message 结构

`Message` 是业务层使用的高阶消息结构。用户通过 Pipeline 操作的是 `*conn.Message`，而不是底层的 `frame.Frame`。FrameCodec 负责在两者之间转换。

```go
type Message struct {
    Type   byte   // Opcode：0x1=Text, 0x2=Binary, 0x8=Close, 0x9=Ping, 0xA=Pong
    Data   []byte // 消息负载
    Status uint16 // 仅 Close 帧使用，表示关闭状态码
}
```

各字段详解：

- **Type**：WebSocket 操作码（opcode）。常用值：
  - `0x1`：文本帧（Text）
  - `0x2`：二进制帧（Binary）
  - `0x8`：关闭帧（Close）
  - `0x9`：Ping 帧
  - `0xA`：Pong 帧
- **Data**：消息负载数据。对于文本帧，这是 UTF-8 编码的字符串；对于二进制帧，这是任意字节数据
- **Status**：仅在 Type 为 `0x8`（Close）时有意义，表示关闭状态码（如 1000 表示正常关闭，1006 表示异常关闭）

### NextConnID

```go
var connIDSeq uint64

func NextConnID() uint64 {
    return atomic.AddUint64(&connIDSeq, 1)
}
```

- 全局原子递增计数器，保证每次调用返回唯一且递增的 ID
- 线程安全，多个 goroutine 可以并发调用
- `netConn` 和 `EpollConn` 在构造时都调用此函数获取 ID

---

## 两种实现

### netConn — 标准 net.Conn 实现

**文件**：`netconn.go`

netConn 是基于 Go 标准库 `net.Conn` 的实现，**全平台可用**，是当前生产环境的默认实现。

#### 结构体

```go
type netConn struct {
    id        uint64
    conn      net.Conn
    isClient  bool
    pipeline  pipeline.ChannelPipeline
    active    int32             // 原子标志：1=活跃，0=已关闭
    closeOnce sync.Once         // 保证 Close 只执行一次
}
```

#### 构造

```go
func NewNetConn(nc net.Conn, isClient bool, id uint64) *netConn
```

- `nc`：底层标准网络连接，通常来自 `ServerHandshake` 或 `ClientHandshake` 返回的 `net.Conn`
- `isClient`：是否为客户端连接，影响帧掩码
- `id`：通常由 `NextConnID()` 生成
- 构造时自动创建一个新的 `pipeline.NewPipeline()`

#### Read 实现

```go
func (c *netConn) Read(b buf.ByteBuf) error {
    if !c.Active() {
        return ErrConnClosed
    }
    // 从 sync.Pool 获取 4KB 临时缓冲区
    tmpPtr := netConnReadPool.Get().(*[]byte)
    tmp := *tmpPtr
    n, err := c.conn.Read(tmp)
    if n > 0 {
        b.Write(tmp[:n])   // 写入 ByteBuf
    }
    netConnReadPool.Put(tmpPtr)  // 归还缓冲区
    return err
}
```

**关键设计**：

- 使用 `sync.Pool` 复用 4KB 临时缓冲区，避免每次 Read 都分配新内存，减少 GC 压力
- 读取流程：从标准 `net.Conn` 读取数据到临时缓冲区 → 写入 ByteBuf
- 如果连接已关闭（`Active()` 返回 false），直接返回 `ErrConnClosed`

#### Write 实现

```go
func (c *netConn) Write(b buf.ByteBuf) error {
    if !c.Active() {
        return ErrConnClosed
    }
    data := b.ReadAll()
    for len(data) > 0 {
        n, err := c.conn.Write(data)
        if err != nil {
            return err
        }
        data = data[n:]    // 处理部分写入（short write）
    }
    return nil
}
```

**关键设计**：

- 调用 `b.ReadAll()` 获取所有可读数据
- 使用循环处理部分写入（short write）：TCP 缓冲区可能已满，`conn.Write` 可能只写入部分数据
- 每次循环将已写入的部分跳过，继续写剩余数据，直到全部写完或出错

#### Close 实现

```go
func (c *netConn) Close() error {
    var err error
    c.closeOnce.Do(func() {
        atomic.StoreInt32(&c.active, 0)   // 先标记为不活跃
        err = c.conn.Close()               // 再关闭底层连接
    })
    return err
}
```

**关键设计**：

- `sync.Once` 保证 `conn.Close()` 只被调用一次，避免重复关闭导致的错误
- 先原子地将 `active` 设为 0，再关闭连接，确保其他 goroutine 通过 `Active()` 能及时感知

---

### EpollConn — 事件驱动实现（仅 Linux）

**文件**：`epollconn.go`

EpollConn 是基于原始文件描述符（fd）的事件驱动实现，配合 EventLoop 使用，实现非阻塞 I/O。这是 V2 的核心目标——让空闲连接零 goroutine 开销。

#### 结构体

```go
type EpollConn struct {
    id        uint64
    fd        int
    isClient  bool
    pipeline  pipeline.ChannelPipeline
    active    int32                 // 原子标志：1=活跃，0=已关闭
    closeOnce sync.Once

    el        eventloop.EventLoop   // 所属的 EventLoop

    readBuf   *bytes.Buffer         // 回退读取缓冲区（onFrame 为 nil 时使用）
    readMu    sync.Mutex            // 保护 readBuf
    maxFrame  int                   // 最大帧大小

    writeMu   sync.Mutex            // 保护写缓冲区
    writeBuf  []buf.ByteBuf         // 待写入的 ByteBuf 队列
    writeOff  int                   // 当前正在写入的 ByteBuf 的偏移量

    frameParser *frame.IncrementalParser  // 增量帧解析器
    onFrame     func(frame.Frame)         // 帧回调
    onClose     func()                    // 关闭回调
}
```

各字段详解：

- **id**：连接唯一 ID，由 `NextConnID()` 生成
- **fd**：底层原始文件描述符，通过 epoll 监听其 I/O 事件
- **isClient**：是否为客户端连接，影响帧掩码
- **pipeline**：连接的处理器链
- **active**：原子标志，1=活跃，0=已关闭。通过 `atomic.LoadInt32` / `atomic.StoreInt32` 操作，线程安全
- **closeOnce**：保证 Close 逻辑只执行一次
- **el**：所属的 EventLoop，用于注册/修改/取消 epoll 事件
- **readBuf**：回退模式的读取缓冲区。当 `onFrame` 回调未设置时，`handleReadEvent` 将数据写入此缓冲区，后续通过 `Read()` 方法读取
- **readMu**：保护 `readBuf` 的互斥锁，因为 `handleReadEvent`（EventLoop 线程）和 `Read`（业务线程）可能并发访问
- **maxFrame**：最大允许的帧大小，默认 64MB
- **writeMu**：保护写缓冲区的互斥锁
- **writeBuf**：待写入的 ByteBuf 队列。非阻塞写入可能一次写不完，未写完的数据暂存在此
- **writeOff**：当前正在写入的 ByteBuf 中已写入的字节偏移量
- **frameParser**：增量帧解析器，负责将 TCP 字节流逐步解析为完整的 WebSocket 帧
- **onFrame**：帧回调函数，当 `frameParser` 解析出一个完整帧时调用
- **onClose**：关闭回调函数，当连接关闭时调用

#### 构造：NewEpollConn

```go
func NewEpollConn(fd int, isClient bool, id uint64) *EpollConn {
    unix.SetNonblock(fd, true)   // 确保非阻塞模式
    return &EpollConn{
        id:          id,
        fd:          fd,
        isClient:    isClient,
        pipeline:    pipeline.NewPipeline(),
        active:      1,
        readBuf:     bytes.NewBuffer(nil),
        maxFrame:    64 * 1024 * 1024,
        frameParser: frame.NewIncrementalParser(64 * 1024 * 1024),
    }
}
```

**关键步骤**：

1. 调用 `unix.SetNonblock(fd, true)` 确保 fd 处于非阻塞模式。这是事件驱动 I/O 的前提——如果 fd 是阻塞的，`unix.Read` / `unix.Write` 会阻塞当前线程
2. 初始化 `IncrementalParser`，默认最大帧大小 64MB
3. 初始化 `readBuf` 为空缓冲区
4. 自动创建一个新的 Pipeline

#### SetOnFrame

```go
func (c *EpollConn) SetOnFrame(fn func(frame.Frame)) {
    c.onFrame = fn
}
```

- 注册帧回调函数。当 `IncrementalParser` 解析出一个完整的 WebSocket 帧时，调用此回调
- **典型用法**：服务端在 `OnConnect` 回调中设置 `onFrame`，在回调内将帧解码为 `*conn.Message`，然后触发 `Pipeline.FireChannelRead`
- 如果 `onFrame` 为 nil，则 `handleReadEvent` 会回退到 `readBuf` 模式

#### SetOnClose

```go
func (c *EpollConn) SetOnClose(fn func()) {
    c.onClose = fn
}
```

- 注册关闭回调函数。当连接关闭时（`closeLocked` 中）调用此回调
- **典型用法**：服务端在创建 EpollConn 后设置 `onClose`，在回调中将 Session 状态设置为 Disconnected，并从 Hub 注销

#### SetMaxFrameSize

```go
func (c *EpollConn) SetMaxFrameSize(n int) {
    c.maxFrame = n
    c.frameParser = frame.NewIncrementalParser(n)
}
```

- 修改最大允许的帧大小
- 调用后会**重新创建** `IncrementalParser`，因为解析器内部状态与帧大小限制相关
- 如果客户端发送超过此大小的帧，解析器会返回错误，连接将被关闭

#### SetEventLoop

```go
func (c *EpollConn) SetEventLoop(el interface{}) {
    if v, ok := el.(eventloop.EventLoop); ok {
        c.el = v
    }
}
```

- 存储 EventLoop 引用，用于后续的 `Register` / `Mod` / `Deregister` 操作
- 参数类型为 `interface{}` 而非 `eventloop.EventLoop`，这是为了**避免循环依赖**——conn 包不能直接导入 eventloop 包的具体类型
- 内部做类型断言，如果传入的类型不匹配则静默忽略

#### Read

```go
func (c *EpollConn) Read(b buf.ByteBuf) error {
    if !c.Active() {
        return ErrConnClosed
    }
    c.readMu.Lock()
    defer c.readMu.Unlock()
    if c.readBuf.Len() > 0 {
        b.Write(c.readBuf.Bytes())
        c.readBuf.Reset()
    }
    return nil
}
```

- 从内部 `readBuf` 复制数据到 ByteBuf
- 这是**回退模式**——仅在 `onFrame` 未设置时使用。当 `onFrame` 已设置时，数据通过帧回调直接处理，不需要通过 `Read` 方法
- 读取后清空 `readBuf`
- 注意：此方法不会阻塞在 fd 上，因为数据已经被 `handleReadEvent` 预读到 `readBuf` 中

#### Write

```go
func (c *EpollConn) Write(b buf.ByteBuf) error {
    if !c.Active() {
        return ErrConnClosed
    }
    c.writeMu.Lock()
    c.writeBuf = append(c.writeBuf, b)   // 将 ByteBuf 加入写队列
    c.writeMu.Unlock()
    c.flushWrite()                         // 尝试立即发送
    return nil
}
```

- 将 ByteBuf 加入写队列，然后调用 `flushWrite` 尝试立即发送
- 非阻塞设计：如果 fd 当前可写，数据可能立即全部发出；如果发送缓冲区已满，剩余数据留在队列中，等 EPOLLOUT 事件触发后再发送

#### flushWrite

```go
func (c *EpollConn) flushWrite() {
    c.writeMu.Lock()
    defer c.writeMu.Unlock()

    for len(c.writeBuf) > 0 {
        bb := c.writeBuf[0]
        data := bb.Bytes()
        if c.writeOff < len(data) {
            n, err := unix.Write(c.fd, data[c.writeOff:])
            if n > 0 {
                c.writeOff += n
            }
            if err != nil {
                if err == unix.EAGAIN {
                    // 发送缓冲区已满，注册 EPOLLOUT
                    c.registerWriteEvent()
                    return
                }
                // 其他错误，关闭连接
                c.closeLocked()
                return
            }
        }
        if c.writeOff >= len(data) {
            // 当前 ByteBuf 全部写完
            bb.Release()            // 释放引用计数
            c.writeBuf = c.writeBuf[1:]
            c.writeOff = 0
        }
    }

    // 所有数据都发完了，取消 EPOLLOUT
    if c.el != nil {
        _ = c.el.Mod(c.fd, eventloop.EventRead)
    }
}
```

**非阻塞写循环的完整流程**：

1. 取出写队列中的第一个 ByteBuf
2. 从 `writeOff` 位置开始，调用 `unix.Write` 写入 fd
3. 如果写入成功，推进 `writeOff`
4. 如果返回 `EAGAIN`：说明 fd 的发送缓冲区已满，调用 `registerWriteEvent()` 注册 EPOLLOUT，然后返回。等 fd 可写时 EventLoop 会调用 `handleWriteEvent` → `flushWrite` 继续发送
5. 如果是其他错误：关闭连接
6. 如果当前 ByteBuf 全部写完：调用 `bb.Release()` 释放引用计数，从队列中移除，重置 `writeOff`
7. 队列清空后：调用 `el.Mod(fd, EventRead)` 取消 EPOLLOUT 注册，避免不必要的写事件触发

#### Close / closeLocked

```go
func (c *EpollConn) Close() error {
    c.writeMu.Lock()
    defer c.writeMu.Unlock()
    c.closeLocked()
    return nil
}

func (c *EpollConn) closeLocked() {
    c.closeOnce.Do(func() {
        atomic.StoreInt32(&c.active, 0)    // 标记为不活跃
        if c.el != nil {
            _ = c.el.Deregister(c.fd)      // 从 EventLoop 注销 fd
        }
        for _, bb := range c.writeBuf {
            bb.Release()                    // 释放所有待写入的 ByteBuf
        }
        c.writeBuf = nil
        unix.Close(c.fd)                   // 关闭 fd
        if c.onClose != nil {
            c.onClose()                     // 调用关闭回调
        }
    })
}
```

**关闭流程**：

1. `Close` 方法加写锁（`writeMu`），然后调用 `closeLocked`
2. `closeOnce.Do` 保证关闭逻辑只执行一次
3. 原子地将 `active` 设为 0
4. 从 EventLoop 注销 fd（`Deregister`），此后 EventLoop 不再报告此 fd 的事件
5. 释放所有待写入队列中的 ByteBuf（调用 `Release` 减引用计数）
6. 调用 `unix.Close` 关闭底层 fd
7. 如果设置了 `onClose` 回调，调用它

#### OnEvent

```go
func (c *EpollConn) OnEvent(events uint32) {
    if events&eventloop.EventRead != 0 {
        c.handleReadEvent()       // EPOLLIN → 处理读事件
    }
    if events&eventloop.EventWrite != 0 {
        c.handleWriteEvent()      // EPOLLOUT → 处理写事件
    }
    if events&(eventloop.EventError|eventloop.EventHup) != 0 {
        c.Close()                 // EPOLLERR|EPOLLHUP → 关闭连接
    }
}
```

- 由 EventLoop 在 fd 就绪时调用
- 根据事件类型分发到不同的处理函数
- 注意：EPOLLIN 和 EPOLLOUT 可能同时触发，所以用 `if` 而不是 `else if`
- EPOLLERR 和 EPOLLHUP 表示连接出错或对端关闭，直接关闭连接

#### handleReadEvent

```go
func (c *EpollConn) handleReadEvent() {
    tmp := make([]byte, 4096)
    for {
        n, err := unix.Read(c.fd, tmp)
        if n > 0 {
            if c.onFrame != nil && c.frameParser != nil {
                // 使用 IncrementalParser 解析帧
                frames := c.frameParser.Feed(tmp[:n])
                for _, f := range frames {
                    c.onFrame(f)    // 每解析出一个完整帧，调用回调
                }
                if c.frameParser.Err() != nil {
                    c.Close()       // 解析错误，关闭连接
                    return
                }
            } else {
                // 回退模式：写入 readBuf
                c.readMu.Lock()
                c.readBuf.Write(tmp[:n])
                c.readMu.Unlock()
            }
        }
        if err != nil {
            if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
                return              // 数据读完，正常返回
            }
            c.Close()               // 其他错误，关闭连接
            return
        }
        if n == 0 {
            c.Close()               // EOF，对端关闭
            return
        }
    }
}
```

**非阻塞读循环的完整流程**：

1. 分配 4KB 临时缓冲区
2. 循环调用 `unix.Read` 从 fd 读取数据
3. **如果 `onFrame` 已设置**（推荐模式）：
   - 将读取的字节喂给 `IncrementalParser.Feed()`
   - `Feed` 返回所有已解析出的完整帧
   - 对每个帧调用 `onFrame` 回调
   - 如果解析器报错（如帧格式非法、帧超长），关闭连接
4. **如果 `onFrame` 未设置**（回退模式）：
   - 将数据写入 `readBuf`，后续通过 `Read()` 方法读取
5. `unix.Read` 返回 `EAGAIN` / `EWOULDBLOCK`：表示 fd 暂时没有更多数据，正常返回
6. 其他错误或 `n == 0`（EOF）：关闭连接

#### handleWriteEvent

```go
func (c *EpollConn) handleWriteEvent() {
    c.flushWrite()
}
```

- 当 fd 变为可写时由 EventLoop 调用
- 直接调用 `flushWrite` 继续发送写队列中的数据
- 如果队列清空，`flushWrite` 会自动取消 EPOLLOUT 注册

#### EventHandlerAdapter

```go
type EventHandlerAdapter struct {
    Conn *EpollConn
}

func (a *EventHandlerAdapter) OnEvent(fd int, events uint32) {
    a.Conn.OnEvent(events)
}
```

- 适配器，将 `EpollConn` 适配为 `eventloop.EventHandler` 接口
- EventLoop 要求 Handler 实现 `OnEvent(fd int, events uint32)` 方法
- `EpollConn` 自己的 `OnEvent` 方法签名是 `OnEvent(events uint32)`（不需要 fd，因为已知自己的 fd）
- 适配器在调用时忽略传入的 fd，直接委托给 `EpollConn.OnEvent`

---

## ConnWriter — Pipeline 出站头 Handler

**文件**：`conn_writer.go`

ConnWriter 是一个 OutboundHandler，充当 Pipeline 出站链的"头"——将最终的 ByteBuf 数据写入底层连接。

### 为什么需要 ConnWriter？

Pipeline 的出站链从 Tail 往 Head 方向流动。FrameCodec 将 `*conn.Message` 编码为 ByteBuf 后，还需要通过 `ctx.Write(bb)` 继续传递。如果没有 ConnWriter，ByteBuf 就无处可去——Pipeline 不知道该把数据写到哪里。ConnWriter 就是这个"出口"，它将 ByteBuf 转换为实际的 `Conn.Write()` 调用。

### 代码

```go
type ConnWriter struct {
    Conn Conn
}

func (cw *ConnWriter) Name() string { return "headWriter" }

func (cw *ConnWriter) Write(ctx pipeline.Context, msg interface{}) {
    if bb, ok := msg.(buf.ByteBuf); ok {
        _ = cw.Conn.Write(bb)   // 将 ByteBuf 写入底层连接
        return
    }
    ctx.FireChannelWrite(msg)   // 非 ByteBuf 类型继续传递
}

func (cw *ConnWriter) Flush(ctx pipeline.Context) {}
```

### 关键设计

- **自动添加**：server/client 在创建连接后，自动将 ConnWriter 作为第一个 Handler 添加到 Pipeline：`Pipeline.AddFirst("headWriter", &ConnWriter{Conn: c})`
- **类型过滤**：`Write` 方法只处理 `buf.ByteBuf` 类型的消息。如果是 ByteBuf，调用 `Conn.Write` 写入网络；如果不是，继续传递给前一个 OutboundHandler
- **不需要 Flush**：当前实现中 `Flush` 为空，因为 `Conn.Write` 是立即发送的
- **无入站能力**：ConnWriter 只实现了 `OutboundHandler`，没有实现 `InboundHandler`，所以入站事件会跳过它

---

## FrameCodec — 帧编解码器

**文件**：`codec.go`

FrameCodec 是一个 OutboundHandler，负责将业务层的 `*conn.Message` 编码为 WebSocket 帧格式，并转换为 ByteBuf 继续在出站链上流动。

### 重要变更

**旧版本**中 FrameCodec 有一个 `Writer io.Writer` 字段，直接将帧写入 io.Writer。**新版本移除了这个字段**。现在 FrameCodec 通过 Pipeline 机制工作：编码后的 ByteBuf 通过 `ctx.Write(bb)` 传递给下一个 OutboundHandler（通常是 ConnWriter），由 ConnWriter 负责实际的网络写入。

### 代码

```go
type FrameCodec struct {
    IsClient bool
    Pool     buf.Pool // 可选；如果为 nil 则分配临时 ByteBuf
}

func (fc *FrameCodec) Name() string { return "frameCodec" }

func (fc *FrameCodec) Write(ctx pipeline.Context, msg interface{}) {
    m, ok := msg.(*Message)
    if !ok {
        ctx.FireChannelWrite(msg)   // 非 *Message 类型，继续传递
        return
    }
    f := frame.Frame{
        FIN:     true,
        Opcode:  frame.Opcode(m.Type),
        Payload: m.Data,
        Masked:  fc.IsClient,
    }
    if fc.IsClient {
        f.MaskKey = frame.GenerateMaskKey()   // 客户端必须生成掩码
    }
    size := 14 + len(m.Data)
    var bb buf.ByteBuf
    if fc.Pool != nil {
        bb = fc.Pool.Get(size)    // 从池中获取 ByteBuf
    } else {
        bb = buf.NewByteBuf(size) // 新建 ByteBuf
    }
    _ = frame.WriteFrameTo(bb, f)  // 将帧写入 ByteBuf
    ctx.Write(bb)                   // 传递给前一个 OutboundHandler
}

func (fc *FrameCodec) Flush(ctx pipeline.Context) {}
```

### 编码流程

1. 检查消息是否为 `*conn.Message` 类型，不是则跳过
2. 将 `Message` 转换为 `frame.Frame`：
   - `FIN = true`：当前实现总是发送完整帧（不分片）
   - `Opcode`：由 `m.Type` 映射（0x1→Text, 0x2→Binary, 0x8→Close, 0x9→Ping, 0xA→Pong）
   - `Payload`：消息数据
   - `Masked`：客户端模式必须设为 true
3. 如果是客户端，生成 4 字节随机掩码 `MaskKey`
4. 计算帧的预估大小（14 字节帧头 + 负载长度），分配 ByteBuf
5. 调用 `frame.WriteFrameTo(bb, f)` 将帧序列化到 ByteBuf
6. 调用 `ctx.Write(bb)` 将 ByteBuf 传递给前一个 OutboundHandler（即 ConnWriter）

### 客户端 vs 服务端

| 行为 | 服务端 (`IsClient=false`) | 客户端 (`IsClient=true`) |
|---|---|---|
| `Masked` | `false` | `true`（RFC 6455 要求客户端到服务端的帧必须掩码） |
| `MaskKey` | 不生成 | 生成 4 字节随机掩码 |

### Pool 字段

- 可选的 ByteBuf 对象池。如果设置了 Pool，从池中获取 ByteBuf；否则新建
- 使用池可以减少内存分配和 GC 压力

---

## 握手 — RFC 6455

**文件**：`handshake.go`

### ServerHandshake

服务端验证 HTTP Upgrade 请求并返回 101 Switching Protocols 响应。

```go
func ServerHandshake(w http.ResponseWriter, r *http.Request) (net.Conn, error)
```

**参数**：
- `w`：`http.ResponseWriter`，用于发送 101 响应
- `r`：`*http.Request`，客户端的 HTTP 请求

**验证项**（任一失败返回 `errInvalidHandshake`）：

1. `Method == GET`
2. `Upgrade` 头为 `websocket`（不区分大小写）
3. `Connection` 头包含 `upgrade`（不区分大小写）
4. `Sec-WebSocket-Key` 非空
5. `Sec-WebSocket-Version == 13`

**通过验证后**：

1. 使用 `http.Hijacker` 接口接管底层 TCP 连接
2. 获取到 `net.Conn` 和 `bufio.ReadWriter`
3. 如果 `bufio.Reader` 中有缓冲数据（HTTP 中间件可能预读了部分数据），包装为 `drainConn`，确保缓冲数据不丢失
4. 构造 101 响应并发送
5. 返回原始 `net.Conn`，调用方负责 `ApplyTCPOptions`

### ClientHandshake

客户端发起 WebSocket 连接。

```go
func ClientHandshake(rawURL string, headers http.Header) (net.Conn, error)
```

**参数**：
- `rawURL`：WebSocket 服务器地址，如 `ws://localhost:8080/` 或 `wss://example.com/ws`
- `headers`：额外的 HTTP 请求头

**流程**：

1. 解析 URL：`ws://` 对应 TCP 端口 80，`wss://` 对应 TLS 端口 443
2. 建立 TCP 连接（`ws`）或 TLS 连接（`wss`），超时时间 `defaultHandshakeTimeout = 10s`
3. 设置连接截止时间（整个握手过程不超过 10 秒）
4. 生成 16 字节随机 `Sec-WebSocket-Key`
5. 计算 `Sec-WebSocket-Accept` 期望值
6. 发送 HTTP Upgrade 请求
7. 读取并验证服务端响应：
   - 状态码必须为 101
   - `Upgrade` 头为 `websocket`
   - `Sec-WebSocket-Accept` 与期望值匹配
8. 清除连接截止时间
9. 返回 `net.Conn`

### Accept Key 计算

```go
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func computeAcceptKey(secKey string) string {
    h := sha1.New()
    h.Write([]byte(secKey + websocketGUID))
    return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
```

- 将客户端发送的 `Sec-WebSocket-Key` 与固定的 GUID 拼接
- 计算 SHA-1 哈希
- Base64 编码后即为 `Sec-WebSocket-Accept` 的值
- 这是 RFC 6455 规定的计算方式，不提供安全性，仅防止缓存代理误判

### drainConn

```go
type drainConn struct {
    net.Conn
    buf *bufio.Reader
}

func (d *drainConn) Read(p []byte) (int, error) {
    if d.buf.Buffered() > 0 {
        return d.buf.Read(p)      // 先读缓冲区中的数据
    }
    return d.Conn.Read(p)          // 缓冲区读完后再从连接读
}
```

- 某些 HTTP 中间件在 Hijack 前会预读数据到 `bufio.Reader`
- `drainConn` 确保这些缓冲数据优先被读取，不丢失
- 同时也实现了 `syscall.Conn` 接口，支持 `ApplyTCPOptions`

---

## 原始 fd 握手（仅 Linux）

**文件**：`handshake_fd.go`（声明）、`handshake_fd_linux.go`（Linux 实现）、`handshake_fd_nonlinux.go`（非 Linux 桩）

### 为什么需要 fd 级别的握手？

在事件驱动模型中，EpollConn 基于原始 fd 工作。如果先用标准 `net.Conn` 握手再转为 fd，会涉及多次 fd 拷贝，效率不高。`ServerHandshakeFD` 和 `ClientHandshakeFD` 直接在原始 fd 上完成 HTTP 握手，避免了不必要的转换。

### ServerHandshakeFD

```go
func ServerHandshakeFD(fd int) (int, error)
```

**参数**：`fd` — 已接受的、非阻塞的客户端连接文件描述符

**返回**：成功时返回原始 fd（不是 dup 的 fd），失败时返回 fd 和 error

**流程**：

1. **临时设为阻塞模式**：`unix.SetNonblock(fd, false)`。因为 HTTP 请求/响应需要完整的读写，非阻塞模式下需要自己处理 EAGAIN，为了简化代码，暂时设为阻塞
2. **Dup fd**：`unix.Dup(fd)` 创建一个副本。这是**关键步骤**——后续 `os.NewFile` + `net.FileConn` 会接管这个 dup 的 fd，当 GC 回收 `*os.File` 时会关闭它。如果不 Dup，GC 会关闭原始 fd，导致连接被意外关闭
3. `os.NewFile(uintptr(dupFd), "ws-conn")` 将 dup 的 fd 包装为 `*os.File`
4. `net.FileConn(f)` 将 `*os.File` 转为 `net.Conn`。注意：`net.FileConn` 内部会再 Dup 一次 fd
5. **立即关闭 `*os.File`**：`f.Close()` 释放 dup 的 fd，避免 GC finalizer 关闭它
6. 设置 10 秒截止时间
7. `http.ReadRequest` 读取 HTTP 请求
8. 验证 WebSocket 升级请求（同 `ServerHandshake` 的验证逻辑）
9. 构造并写入 101 响应
10. **恢复非阻塞模式**：`defer unix.SetNonblock(fd, true)`，确保后续 EpollConn 可以正常进行非阻塞 I/O
11. 返回原始 fd

### ClientHandshakeFD

```go
func ClientHandshakeFD(fd int, rawURL string, headers http.Header) error
```

**参数**：
- `fd`：非阻塞的、处于连接中（EINPROGRESS）的文件描述符
- `rawURL`：WebSocket 服务器地址
- `headers`：额外的 HTTP 请求头

**流程**：

1. 与 `ServerHandshakeFD` 相同的 Dup 技巧
2. 临时设为阻塞模式
3. 构造并发送 HTTP Upgrade 请求
4. 读取并验证 101 响应
5. 恢复非阻塞模式

### 非 Linux 桩

```go
//go:build !linux

func ServerHandshakeFD(fd int) (int, error) {
    return fd, errors.New("ServerHandshakeFD is only supported on Linux")
}

func ClientHandshakeFD(fd int, rawURL string, headers http.Header) error {
    return errors.New("ClientHandshakeFD is only supported on Linux")
}
```

- 非 Linux 平台直接返回错误
- 使用 Go build tag `//go:build !linux` 确保只在非 Linux 平台编译

---

## 非阻塞拨号（仅 Linux）

**文件**：`dial.go`（声明）、`dial_linux.go`（Linux 实现）、`dial_nonlinux.go`（非 Linux 桩）

### DialNonBlock

```go
func DialNonBlock(addr string) (int, error)
```

创建一个非阻塞的 TCP 套接字并开始连接，返回处于连接中状态的 fd。

**Linux 实现流程**：

1. `net.ResolveTCPAddr` 解析地址
2. `unix.Socket(AF_INET, SOCK_STREAM|SOCK_NONBLOCK|SOCK_CLOEXEC, 0)` 创建非阻塞套接字
   - `SOCK_NONBLOCK`：非阻塞模式
   - `SOCK_CLOEXEC`：exec 时自动关闭 fd
3. `unix.Connect(fd, sa)` 发起连接
4. 对于非阻塞套接字，`Connect` 立即返回 `EINPROGRESS`，表示连接正在进行中
5. 返回 fd，调用方可以将此 fd 注册到 EventLoop，等 EPOLLOUT 事件表示连接完成

**非 Linux 桩**：

```go
func DialNonBlock(addr string) (int, error) {
    return -1, errors.New("DialNonBlock is only supported on Linux")
}
```

---

## TCP 参数设置

**文件**：`tcp.go`

### ApplyTCPOptions

```go
func ApplyTCPOptions(c net.Conn, noDelay, quickAck bool) error
```

在标准 `net.Conn` 上设置 TCP 参数。

**TCP_NODELAY**（`noDelay=true`）：

- 关闭 Nagle 算法
- Nagle 算法会将小包合并成大包发送，减少网络上的小包数量
- 对于 WebSocket 这种低延迟场景，Nagle 算法会增加延迟
- 关闭后，小帧（如 Ping/Pong、短消息）会立即发送

**TCP_QUICKACK**（`quickAck=true`，仅 Linux）：

- 关闭延迟确认（Delayed ACK）
- 延迟确认是 TCP 的优化策略，收到数据后不立即发送 ACK，而是等一小段时间看是否有数据要一起发送
- 在某些场景下，延迟确认与 Nagle 算法交互会导致额外延迟（Nagle 等确认，确认又延迟发送）
- 关闭后，收到数据立即发送 ACK，降低 RTT
- 非 Linux 平台设置此选项会被静默忽略（不会报错）

**实现方式**：

- 通过 `syscall.Conn` 接口获取底层 fd
- 使用 `rawConn.Control` 在 fd 上设置 socket 选项
- `TCP_NODELAY` 在所有平台生效
- `TCP_QUICKACK` 仅在 Linux 生效

### ListenTCPWithReusePort

```go
func ListenTCPWithReusePort(address string) (net.Listener, error)
```

使用 `SO_REUSEPORT` 创建 TCP 监听器。

- 允许多个进程/线程监听同一端口
- 内核在多个监听者之间做负载均衡，避免惊群效应
- 配合 `TCP_NODELAY`，避免 Nagle + Delayed ACK 交互延迟
- 使用 `net.ListenConfig` 的 `Control` 回调设置 `SO_REUSEPORT`

---

## 文件列表

| 文件 | 说明 |
|---|---|
| `conn.go` | `Conn` / `EventDrivenConn` 接口定义，`Message` 结构体，`NextConnID` 函数，`ErrConnClosed` 错误 |
| `netconn.go` | `netConn` 实现：基于标准 `net.Conn`，使用 `sync.Pool` 复用 4KB 读缓冲区，全平台可用 |
| `epollconn.go` | `EpollConn` 实现：基于原始 fd 的事件驱动连接，非阻塞读写 + EPOLLOUT 注册，`EventHandlerAdapter` 适配器 |
| `conn_writer.go` | `ConnWriter`：OutboundHandler，将 ByteBuf 写入 Conn，自动添加为 Pipeline 的第一个 Handler |
| `codec.go` | `FrameCodec`：OutboundHandler，将 `*Message` 编码为 WebSocket 帧，输出 ByteBuf |
| `handshake.go` | `ServerHandshake` / `ClientHandshake`：标准 `net.Conn` 上的 RFC 6455 握手，`drainConn`，`computeAcceptKey` |
| `handshake_fd.go` | `ServerHandshakeFD` / `ClientHandshakeFD` 的函数声明 |
| `handshake_fd_linux.go` | Linux 上基于原始 fd 的 WebSocket 握手实现（使用 `unix.Dup` 避免 GC 关闭 fd） |
| `handshake_fd_nonlinux.go` | 非 Linux 平台的桩实现，直接返回错误 |
| `dial.go` | `DialNonBlock` 函数声明 |
| `dial_linux.go` | Linux 上非阻塞拨号实现（`SOCK_NONBLOCK` + `SOCK_CLOEXEC`，`EINPROGRESS` 处理） |
| `dial_nonlinux.go` | 非 Linux 平台的桩实现，直接返回错误 |
| `tcp.go` | `ApplyTCPOptions`（TCP_NODELAY / TCP_QUICKACK），`ListenTCPWithReusePort`（SO_REUSEPORT），`drainConn.SyscallConn` |

---

## 注意事项

1. **netConn 是当前默认实现** — EpollConn 已经完整实现非阻塞读写，但需要配合 EventLoop 使用；当前生产环境默认使用 netConn
2. **FrameCodec 不再有 Writer 字段** — 旧版本直接写 io.Writer，新版本通过 Pipeline 传递 ByteBuf，最终由 ConnWriter 写入连接
3. **ConnWriter 是 Pipeline 的出口** — 没有它，出站数据流到 ByteBuf 就无处可去。server/client 会自动添加
4. **handshake 后返回原始 net.Conn** — Server/Client 负责将其包装为 `netConn` 或 `EpollConn`
5. **ServerHandshakeFD 使用 Dup 技巧** — 避免 GC finalizer 关闭原始 fd，这是 fd 级别操作的关键细节
6. **DialNonBlock 返回连接中的 fd** — 调用方需要将 fd 注册到 EventLoop，等 EPOLLOUT 表示连接完成后再调用 `ClientHandshakeFD`
7. **TCP 参数默认生效** — `TCPNoDelay` 默认 true，`TCPQuickAck` 默认 false，`SOReusePort` 默认 false
8. **drainConn 处理 Hijack 后的缓冲数据** — 某些 HTTP 中间件会在 Hijack 前预读数据，`drainConn` 确保这些数据不丢失
9. **EpollConn 的 RemoteAddr / LocalAddr 暂未实现** — 当前返回 nil，待后续从 fd 解析地址信息

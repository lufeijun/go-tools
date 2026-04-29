# ws/conn — 连接层

`ws/conn` 是 WebSocket 连接的抽象层，定义了 `Conn` 接口和两种实现：`netConn`（基于标准 `net.Conn`）与 `epollConn`（基于事件驱动）。同时提供 RFC 6455 握手协议和帧编解码器。

---

## 设计定位

连接层承上启下：

- **向上**：为 `session` 层提供 `Conn` 接口，隐藏底层 I/O 差异
- **向下**：对接 `eventloop`（事件驱动）或标准 `net.Conn`（fallback）
- **横向**：每个 `Conn` 自带 `ChannelPipeline`，数据在 Pipeline 上流动

---

## 核心接口

### Conn

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

| 方法 | 说明 |
|---|---|
| `ID()` | 连接唯一标识（uint64，全局递增） |
| `Pipeline()` | 每个连接独立的 ChannelPipeline |
| `Read(b)` | 从连接读取数据到 ByteBuf（eventloop 调用） |
| `Write(b)` | 将 ByteBuf 数据写入连接（eventloop 调用） |
| `RemoteAddr()` | 对端地址 |
| `LocalAddr()` | 本地地址 |
| `IsClient()` | 是否为客户端发起的连接 |
| `Close()` | 关闭连接 |
| `Active()` | 连接是否仍活跃 |

### EventDrivenConn

`epollConn` 实现的扩展接口：

```go
type EventDrivenConn interface {
    Conn
    FD() int                    // 底层文件描述符
    OnEvent(events uint32)      // eventloop 回调
    SetEventLoop(el interface{}) // 设置事件循环（interface{} 避免循环依赖）
}
```

### Message

业务层使用的高阶消息结构：

```go
type Message struct {
    Type   byte   // Opcode：0x1=Text, 0x2=Binary, 0x8=Close, 0x9=Ping, 0xA=Pong
    Data   []byte
    Status uint16 // Close 帧的状态码
}
```

---

## 两种实现

### 1. netConn（netconn.go）

基于标准库 `net.Conn`，兼容所有平台，适合开发调试和 fallback 场景。

```go
type netConn struct {
    id        uint64
    conn      net.Conn
    isClient  bool
    pipeline  pipeline.ChannelPipeline
    active    int32
    closeOnce sync.Once
}
```

**构造：**

```go
func NewNetConn(nc net.Conn, isClient bool, id uint64) *netConn
```

**Read 实现：**

```go
func (c *netConn) Read(b buf.ByteBuf) error {
    tmp := make([]byte, 4096)
    n, err := c.conn.Read(tmp)
    if n > 0 {
        b.Write(tmp[:n])
    }
    return err
}
```

- 使用临时 `[]byte` 中转，再写入 `ByteBuf`
- V2.1 优化为直接从 `ByteBuf` 底层数组读取，减少一次拷贝

**Write 实现：**

```go
func (c *netConn) Write(b buf.ByteBuf) error {
    data := b.ReadAll()
    for len(data) > 0 {
        n, err := c.conn.Write(data)
        if err != nil {
            return err
        }
        data = data[n:]
    }
    return nil
}
```

- 支持短写重试（partial write）
- 调用 `b.ReadAll()` 会移动 `readerIndex`，不影响底层数组生命周期

### 2. epollConn（epollconn.go）

基于原始 fd 的事件驱动实现，V2.0 为框架 stub，V2.1 完善非阻塞读写。

```go
type epollConn struct {
    id        uint64
    fd        int
    isClient  bool
    pipeline  pipeline.ChannelPipeline
    active    int32
    closeOnce sync.Once
    el        interface{} // 实际类型：eventloop.EventLoop
}
```

当前状态：
- `Read()` / `Write()` 为 TODO stub
- `RemoteAddr()` / `LocalAddr()` 为 nil（TODO 从 fd 解析）
- `OnEvent()` 为 TODO（V2.1 处理 read/write 事件）

---

## 连接 ID 生成

全局原子递增：

```go
var connIDSeq uint64

func NextConnID() uint64 {
    return atomic.AddUint64(&connIDSeq, 1)
}
```

---

## RFC 6455 握手（handshake.go）

### ServerHandshake

服务端验证 HTTP Upgrade 请求并返回 101 Switching Protocols。

```go
func ServerHandshake(w http.ResponseWriter, r *http.Request) (net.Conn, error)
```

验证项：

1. `Method == GET`
2. `Upgrade == websocket`
3. `Connection` 头包含 `upgrade`
4. `Sec-WebSocket-Key` 非空
5. `Sec-WebSocket-Version == 13`

通过验证后：
- 使用 `http.Hijacker` 接管底层 TCP 连接
- 若 `bufio.Reader` 有缓冲数据，包装为 `drainConn` 先消费缓冲
- 返回原始 `net.Conn`

### ClientHandshake

客户端发起 WebSocket 连接。

```go
func ClientHandshake(rawURL string, headers http.Header) (net.Conn, error)
```

流程：

1. 解析 `ws://` / `wss://`，分别对应 TCP 80 和 TLS 443
2. 建立 TCP/TLS 连接
3. 生成随机 16 字节 `Sec-WebSocket-Key`
4. 发送 HTTP Upgrade 请求
5. 验证服务端返回的 101 响应和 `Sec-WebSocket-Accept`

### Accept Key 计算

```go
const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func computeAcceptKey(secKey string) string {
    h := sha1.New()
    h.Write([]byte(secKey + websocketGUID))
    return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
```

---

## 帧编解码器（codec.go）

`FrameCodec` 是 `OutboundHandler`，将 `*conn.Message` 编码为 WebSocket 帧并写入底层连接。

```go
type FrameCodec struct {
    Writer   io.Writer
    IsClient bool
}
```

### Write 实现

```go
func (fc *FrameCodec) Write(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*Message); ok {
        f := frame.Frame{
            FIN:     true,
            Opcode:  frame.Opcode(m.Type),
            Payload: m.Data,
            Masked:  fc.IsClient,  // 客户端必须掩码
        }
        if fc.IsClient {
            f.MaskKey = frame.GenerateMaskKey()
        }
        _ = frame.WriteFrame(fc.Writer, f)
    }
    ctx.FireChannelWrite(msg)
}
```

- 服务端 `Masked = false`，客户端 `Masked = true`
- 写入完成后继续往前传（`FireChannelWrite`），供其他 OutboundHandler 处理

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `conn.go` | `Conn`、`EventDrivenConn` 接口；`Message`；`NextConnID`；`ErrConnClosed` |
| `netconn.go` | 标准 `net.Conn` 实现：`netConn` |
| `epollconn.go` | 事件驱动实现 stub：`epollConn` |
| `handshake.go` | `ServerHandshake`、`ClientHandshake`、Accept Key 计算、`drainConn` |
| `codec.go` | `FrameCodec`：OutboundHandler，将 `*Message` 编码为 WebSocket 帧 |
| `conn_test.go` | netConn 读写/ID/Active/Close、握手验证、Accept Key 正确性、epollConn 接口合规 |

---

## 注意事项

1. **netConn 是 V2.0 默认实现** — epollConn 的非阻塞读写在 V2.1 完善，当前生产环境使用 netConn
2. **handshake 后返回原始 net.Conn** — Server/Client 负责将其包装为 `netConn` 或 `epollConn`
3. **FrameCodec 必须加到 Pipeline 尾部** — `server.go`/`client.go` 在连接建立后自动添加
4. **drainConn 处理 Hijack 后的缓冲数据** — 某些 HTTP 中间件会在 Hijack 前预读数据，`drainConn` 确保这些数据不丢失
5. **ClientHandshake 支持 wss://** — 使用 `tls.Dial` 建立 TLS 连接

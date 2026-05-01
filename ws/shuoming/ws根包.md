# ws — 根包

`ws` 是框架的根包，提供**统一错误体系**、**全局配置**和**I/O 模式选择**，被 `server`、`client` 及所有子包共享。

---

## WSError — 统一错误类型

v2 引入 `WSError` 替代 v1 的 `CloseError`，支持错误码分类、链式解包和连接上下文。

```go
type WSError struct {
    Code    int      // 错误码：协议层(1xxx) / 网络层(2xxx) / 应用层(3xxx)
    Message string   // 可读错误描述
    Cause   error    // 底层错误（支持 errors.Is / errors.As）
    ConnID  uint64   // 关联连接 ID，0 表示全局错误
}
```

### 方法

```go
func (e *WSError) Error() string
func (e *WSError) Unwrap() error          // 返回 Cause，支持 errors.Is
func (e *WSError) WithConnID(id uint64) *WSError  // 绑定连接 ID，返回新实例（不可变）
```

### 预定义错误码

| 错误码 | 常量 | 含义 |
|---|---|---|
| 1002 | `ErrCodeProtocolError` | 协议格式错误 |
| 1003 | `ErrCodeUnsupportedData` | 不支持的数据类型 |
| 1007 | `ErrCodeInvalidFrame` | 无效的帧格式 |
| 1008 | `ErrCodePolicyViolation` | 策略违规 |
| 1009 | `ErrCodeMessageTooBig` | 消息过大 |
| 1011 | `ErrCodeInternalError` | 内部错误 |
| 2001 | `ErrCodeReadTimeout` | 网络读超时 |
| 2002 | `ErrCodeWriteTimeout` | 网络写超时 |
| 2003 | `ErrCodeConnReset` | 连接重置 |
| 3001 | `ErrCodeHubFull` | Hub 连接数超限 |

### 使用示例

```go
e := &ws.WSError{
    Code:    ws.ErrCodeProtocolError,
    Message: "invalid opcode",
    Cause:   io.EOF,
    ConnID:  42,
}

fmt.Println(e.Error())
// 输出: ws error code=1002: invalid opcode (conn=42)

if errors.Is(e, io.EOF) {
    fmt.Println("底层原因是 io.EOF")
}
```

### Pipeline 错误传播

任何 Handler 中产生的错误通过 `ctx.FireExceptionCaught(err)` 传播到 Pipeline 链。默认处理逻辑记录日志后关闭连接，用户可自定义 `ExceptionHandler` 替换。

---

## IOMode — I/O 模式选择

v2 引入 `IOMode` 类型，用于选择连接的 I/O 处理模型。

### 类型定义

```go
// IOMode determines the connection I/O model.
type IOMode int

const (
    // ModeNet uses net.Conn with goroutine-per-conn (default).
    // 使用 Go 标准库 net.Conn，每个连接一个 goroutine 处理读循环。
    // 所有平台可用。零值即为 ModeNet，无需显式设置。
    ModeNet IOMode = iota

    // ModeEpoll uses epollConn with event-driven Reactor model (Linux only).
    // 使用原始 fd + epoll 的事件驱动 Reactor 模型，数据由 EventLoop 驱动读取。
    // 仅 Linux 可用。目标：10 万–100 万并发连接，近零空闲 goroutine 开销。
    ModeEpoll
)
```

### 两种模式对比

| 方面 | ModeNet（默认） | ModeEpoll |
|---|---|---|
| I/O 模型 | goroutine-per-conn | 事件驱动 Reactor |
| 连接类型 | `netConn`（包装 `net.Conn`） | `epollConn`（包装原始 fd） |
| 读循环 | 每个 goroutine 独立 bufio 读循环 | EventLoop 统一驱动，通过 onFrame 回调分发 |
| 空闲开销 | 每连接一个 goroutine（约 8KB 栈） | 近零（无 goroutine 空闲等待） |
| 适用场景 | 中等并发（万级连接） | 高并发（10 万–100 万连接） |
| 平台支持 | 所有平台 | 仅 Linux |
| 握手方式 | `ServerHandshake`（Hijack）/ `ClientHandshake`（net.Dial） | `ServerHandshakeFD` / `ClientHandshakeFD`（原始 fd 操作） |

### 使用方式

```go
// 默认模式（ModeNet），无需设置 Mode 字段
cfg := ws.Config{
    Addr:         ":8080",
    PingInterval: 30 * time.Second,
}

// Epoll 模式（仅 Linux），显式设置 Mode
cfg := ws.Config{
    Mode:         ws.ModeEpoll,
    Addr:         ":8080",
    PingInterval: 30 * time.Second,
}
```

**零值即 ModeNet：** `IOMode` 的零值是 `ModeNet`（值为 0），所以如果不设置 `Mode` 字段，默认就是 Net 模式。大多数用户不需要关心 `Mode` 字段。

---

## ValidateMode — 模式校验

```go
func (c Config) ValidateMode() error
```

校验 `Config.Mode` 是否在当前平台上可用。

### 规则

- `ModeNet`：所有平台通过，返回 nil
- `ModeEpoll`：仅 Linux 通过；在非 Linux 平台返回错误

```go
func (c Config) ValidateMode() error {
    if c.Mode == ModeEpoll && runtime.GOOS != "linux" {
        return fmt.Errorf("epoll mode is only supported on Linux, current OS: %s", runtime.GOOS)
    }
    return nil
}
```

### 自动调用

`ValidateMode()` 会被 `NewServer` 和 `NewClient` 自动调用，无需手动检查：

```go
// NewServer 内部
func NewServer(cfg ws.Config) (Server, error) {
    if err := cfg.ValidateMode(); err != nil {
        return nil, err   // 非 Linux 上使用 ModeEpoll 会在这里返回错误
    }
    // ... 创建服务端
}

// NewClient 内部
func NewClient(cfg ws.Config) (Client, error) {
    if err := cfg.ValidateMode(); err != nil {
        return nil, err   // 非 Linux 上使用 ModeEpoll 会在这里返回错误
    }
    // ... 创建客户端
}
```

**因此，你只需要检查 `NewServer` / `NewClient` 的返回错误即可：**

```go
srv, err := server.NewServer(ws.Config{Mode: ws.ModeEpoll, Addr: ":8080"})
if err != nil {
    // 在非 Linux 平台上，err 会是 "epoll mode is only supported on Linux, current OS: darwin"
    log.Fatal(err)
}
```

---

## Config — 全局配置

`Config` 是 Server 和 Client 的通用配置结构体，零值表示"使用默认值"。

```go
type Config struct {
    Addr              string        // Server 监听地址 / Client 目标地址
    ReadBufferSize    int           // 读缓冲区大小，默认 4096
    WriteBufferSize   int           // 写缓冲区大小，默认 4096
    MaxConnections    int           // 最大连接数，0 表示不限制
    TCPNoDelay        bool          // 关闭 Nagle 算法，默认 true
    TCPQuickAck       bool          // Linux 下启用 TCP_QUICKACK，默认 false
    SOReusePort       bool          // 启用 SO_REUSEPORT，默认 false
    EventLoopWorkers  int           // SubEventLoop 数量，默认 runtime.NumCPU()
    EventLoopStrategy string        // 负载均衡策略: "roundrobin" | "leastconn"
    BufferPoolSmall   int           // 小 buffer 池对象数 (≤512B)，默认 4096
    BufferPoolDefault int           // 默认 buffer 池对象数 (≤4096B)，默认 1024
    BufferPoolLarge   int           // 大 buffer 池对象数 (≤65536B)，默认 256
    PingInterval      time.Duration // 心跳间隔，默认 30s
    PongTimeout       time.Duration // Pong 超时，默认 60s
    MaxFrameSize      int           // 单帧最大载荷，默认 64MB
    EnableCompression bool          // 预留：permessage-deflate 压缩
    Headers           http.Header   // Client 握手时附加的 HTTP 头
    ReconnectInterval time.Duration // Client 断线后重连初始间隔，默认 5s
    MaxReconnect      int           // Client 最大重连次数，默认 5
    Mode              IOMode        // I/O 模式：ModeNet（默认）或 ModeEpoll（Linux 专属）
}
```

### DefaultConfig

```go
func DefaultConfig() Config
```

返回一份完整的默认配置：

```go
Config{
    ReadBufferSize:    4096,
    WriteBufferSize:   4096,
    TCPNoDelay:        true,
    TCPQuickAck:       false,
    SOReusePort:       false,
    EventLoopWorkers:  0,              // 0 表示 runtime.NumCPU()
    EventLoopStrategy: "roundrobin",
    BufferPoolSmall:   4096,
    BufferPoolDefault: 1024,
    BufferPoolLarge:   256,
    PingInterval:      30 * time.Second,
    PongTimeout:       60 * time.Second,
    MaxFrameSize:      64 * 1024 * 1024,
    EnableCompression: false,
    ReconnectInterval: 5 * time.Second,
    MaxReconnect:      5,
    // Mode 未设置，零值为 ModeNet（默认模式）
}
```

**注意：** `DefaultConfig()` 返回的配置中 `Mode` 字段为零值（`ModeNet`），这是有意为之的 — 大多数用户使用 Net 模式即可，不需要显式设置。

### EventLoopWorkerCount

```go
func (c Config) EventLoopWorkerCount() int
```

返回有效的 SubEventLoop 数量：
- 若 `EventLoopWorkers > 0`，返回该值
- 否则返回 `runtime.NumCPU()`

---

## 配置使用模式

```go
// 模式 1：完全自定义（Mode 为零值，即 ModeNet）
cfg := ws.Config{
    Addr:         ":8080",
    PingInterval: 60 * time.Second,
}

// 模式 2：基于默认值修改
cfg := ws.DefaultConfig()
cfg.Addr = ":8080"
cfg.MaxConnections = 100000
cfg.PingInterval = 60 * time.Second

// 模式 3：Server/Client 自动填充零值
srv, err := server.NewServer(ws.Config{Addr: ":8080"})  // 其他字段自动使用默认值
if err != nil {
    log.Fatal(err)
}

// 模式 4：Epoll 模式（仅 Linux）
cfg := ws.Config{
    Mode:         ws.ModeEpoll,
    Addr:         ":8080",
}
srv, err := server.NewServer(cfg)
if err != nil {
    log.Fatal(err)  // 非 Linux 平台会在这里报错
}

// 模式 5：手动校验模式（可选，NewServer/NewClient 会自动调用）
if err := cfg.ValidateMode(); err != nil {
    log.Fatal("I/O 模式不支持:", err)
}
```

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `ws.go` | `WSError`、`IOMode`、`ModeNet`/`ModeEpoll` 常量、`Config`（含 `Mode` 字段）、`DefaultConfig`、`ValidateMode`、`EventLoopWorkerCount`、预定义错误码 |
| `ws_test.go` | WSError 格式化/Unwrap/WithConnID、DefaultConfig 各字段默认值测试、ValidateMode 测试 |

---

## 注意事项

1. **WSError 不可变** — `WithConnID` 返回新实例，不修改原实例
2. **Config 零值有语义** — `EventLoopWorkers = 0` 表示自动，不是"不启用"；`Mode` 零值为 `ModeNet`
3. **ModeEpoll 仅 Linux 可用** — `ValidateMode()` 和 `NewServer`/`NewClient` 都会校验，非 Linux 使用 `ModeEpoll` 会返回错误
4. **BufferPool 数字表示对象数** — 不是字节数，而是各自 tier 的 `sync.Pool` 预分配对象数量
5. **MaxFrameSize 防止内存攻击** — 收到超过此值的帧直接报错断开，建议根据业务调整（如 16MB）
6. **根包不依赖任何子包** — 确保 `server`/`client` 等子包可以安全 import `ws` 根包
7. **TCPNoDelay 默认 true** — 生产环境建议保持开启，避免 Nagle 算法导致的小帧延迟
8. **NewServer/NewClient 返回 error** — 必须检查返回的错误，特别是在使用 `ModeEpoll` 时

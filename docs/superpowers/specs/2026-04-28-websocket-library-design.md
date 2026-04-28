# WebSocket 开源库设计文档

## 概述

从零开发一个 Go 语言通用 WebSocket 库，第一版聚焦功能完整性，后期再优化性能。

### 核心决策

| 项 | 选择 |
|---|---|
| 端 | 服务端 + 客户端 |
| 底层协议 | 自己实现 RFC 6455 |
| API 风格 | Channel 式 |
| 帧类型 | 全部（Text / Binary / Ping / Pong / Close） |
| 扩展特性 | 心跳保活 + 自动重连 + 状态管理 |

## 架构

分层架构，职责清晰，后期可逐层优化：

```
协议层 (frame)  →  连接层 (conn)  →  会话层 (session)  →  用户 API (client/server)
```

## 高性能演进目标：单机百万连接

第一版聚焦功能，但架构上必须为百万级连接预留演进路径，避免后期无法重构。

### 核心瓶颈与对策

| 瓶颈 | V1 策略 | 演进路径 |
|---|---|---|
| goroutine 开销（3/连接） | goroutine-per-conn 模型 | V2 引入 epoll + goroutine 池，活跃连接才绑定 goroutine |
| channel 开销（2/连接） | channel 模型 | V2 提供 zero-channel 模式，回调直接处理 |
| 定时器开销（1/连接） | time.Ticker per conn | V2 替换为时间轮（time wheel），单 goroutine 管理所有超时 |
| buffer 内存碎片 | 每连接独立 buffer | V1 即使用 `sync.Pool` 复用 buffer，减少 GC 压力 |

### V1 必须遵守的架构约束

以下约束在 V1 就必须落地，否则后续无法平滑演进：

**1. 连接层接口化**
`conn.Conn` 必须是 interface，不是 struct。V1 提供基于 goroutine-per-conn 的实现，V2 可新增 epoll 实现，用户无需改代码。

```go
// conn 接口，V1 实现 goroutineConn，V2 实现 epollConn
type Conn interface {
    ReadChan()  <-chan Message
    WriteChan() chan<- Message
    Close() error
    RemoteAddr() net.Addr
    LocalAddr() net.Addr
    SetDeadline(t time.Time) error
}
```

**2. 连接 ID**
每个连接分配全局唯一 uint64 ID，用于 O(1) 查找，不使用 string 类型 key。

```go
var connIDSeq uint64

func nextConnID() uint64 {
    return atomic.AddUint64(&connIDSeq, 1)
}
```

**3. Buffer 复用**
V1 即使用 `sync.Pool` 管理帧读写 buffer，减少百万连接场景下的内存分配和 GC。

```go
var bufPool = sync.Pool{
    New: func() interface{} { return make([]byte, 0, 4096) },
}
```

**4. Server 内置 Hub**
Server 内置连接管理中心（Hub），支持注册/注销/广播，这是百万连接场景的必备基础设施。

```go
type Hub struct {
    conns map[uint64]*Session  // ID → Session
    register   chan *Session
    unregister chan *Session
    broadcast  chan Message
}

func (h *Hub) Run()                    // 单 goroutine 运行，管理所有连接
func (h *Hub) Broadcast(msg Message)    // 广播消息
func (h *Hub) Count() int              // 当前连接数
func (h *Hub) Get(id uint64) *Session  // 按 ID 查找
```

**5. 心跳可替换**
心跳机制通过接口抽象，V1 实现为 per-conn timer，V2 替换为 time wheel。

```go
type Heartbeater interface {
    Start(conn Conn)
    Stop()
}
```

## 包结构

```
ws/
├── frame/            # 协议层（公有）：RFC 6455 帧解析与构建，可独立使用
│   ├── frame.go           # 帧结构定义、解析、序列化
│   ├── mask.go            # 掩码处理
│   ├── pool.go            # 两级 buffer 复用池
│   └── frame_test.go
├── internal/         # 内部实现，外部不可 import
│   ├── conn/              # 连接层：Conn 接口 + goroutineConn 实现
│   │   ├── conn.go
│   │   ├── handshake.go
│   │   └── conn_test.go
│   └── session/           # 会话层：心跳、重连、状态管理
│       ├── session.go
│       ├── heartbeat.go   # Heartbeater 接口 + perConnHeartbeater 实现
│       ├── reconnect.go
│       └── session_test.go
├── hub.go            # Hub 连接管理中心
├── client.go         # 用户 API：Client
├── server.go         # 用户 API：Server
├── errors.go         # 公共错误类型（CloseError）
├── types.go          # 公共类型 re-export
└── ws_test.go        # 集成测试
```

`frame` 保持公有：用户可能只需要帧解析能力，不需要连接管理。`conn` 和 `session` 是用户 API 的内部实现细节，放入 `internal` 防止外部依赖。

## 协议层（frame 包）

### Frame 结构

```go
type Opcode byte

const (
    OpcodeText   Opcode = 0x1
    OpcodeBinary Opcode = 0x2
    OpcodeClose  Opcode = 0x8
    OpcodePing   Opcode = 0x9
    OpcodePong   Opcode = 0xA
)

type Frame struct {
    FIN     bool
    RSV1    bool
    RSV2    bool
    RSV3    bool
    Opcode  Opcode
    Masked  bool
    MaskKey [4]byte
    Payload []byte
}
```

### 核心函数

- `ReadFrame(r io.Reader) (Frame, error)` — 读取并解析一个完整帧
- `WriteFrame(w io.Writer, f Frame) error` — 序列化并写入帧
- 便捷构造函数：`NewTextFrame` / `NewBinaryFrame` / `NewPingFrame` / `NewPongFrame` / `NewCloseFrame`

### Buffer 复用

```go
// pool.go — 两级 buffer 复用池，避免小消息占大 buffer
const (
    smallBufSize = 512   // 小消息：心跳、控制帧
    defaultBufSize = 4096 // 常规消息
)

var smallBufPool = sync.Pool{
    New: func() interface{} { return make([]byte, smallBufSize) },
}

var defaultBufPool = sync.Pool{
    New: func() interface{} { return make([]byte, defaultBufSize) },
}

// GetBuf 按 payload 预估大小选择池子
func GetBuf(size int) []byte

// PutBuf 按 cap 归位到对应池子
func PutBuf(buf []byte)
```

### 关键行为

- `ReadFrame` 处理分片（fragmentation）：非 FIN 帧内部缓冲，直到收到 FIN 帧返回完整消息
- 掩码处理在协议层完成，上层无需关心
- Close 帧携带 status code + reason，解析为结构化数据
- payload 长度编码：7bit / 16bit / 64bit 三种模式按 RFC 6455 规范处理

## 错误类型

复用 WebSocket Close Code 作为错误码，不另建错误码体系：

```go
// errors.go
type CloseError struct {
    Code   uint16  // WebSocket Close Code（RFC 6455 定义）
    Reason string  // 关闭原因
    Cause  error   // 底层错误（可选）
}

func (e *CloseError) Error() string

// 预定义错误
var (
    ErrProtocolError    = &CloseError{Code: 1002, Reason: "protocol error"}
    ErrUnsupportedData  = &CloseError{Code: 1003, Reason: "unsupported data"}
    ErrInvalidFrame     = &CloseError{Code: 1007, Reason: "invalid frame payload data"}
    ErrPolicyViolation  = &CloseError{Code: 1008, Reason: "policy violation"}
    ErrMessageTooBig    = &CloseError{Code: 1009, Reason: "message too big"}
    ErrInternalError    = &CloseError{Code: 1011, Reason: "internal error"}
)

// WithCause 包装底层错误
func (e *CloseError) WithCause(cause error) *CloseError
```

## 连接层（internal/conn 包）

### Message 结构

```go
type Message struct {
    Type   Opcode  // Text, Binary, Close, Ping, Pong
    Data   []byte  // 消息内容
    Status uint16  // 仅 Close 帧使用
}
```

### Conn 接口

```go
// Conn 是 WebSocket 连接的抽象接口
// V1 提供 goroutineConn 实现，V2 可新增 epollConn 实现
type Conn interface {
    ReadChan()  <-chan Message
    WriteChan() chan<- Message
    Close() error
    RemoteAddr() net.Addr
    LocalAddr() net.Addr
    SetDeadline(t time.Time) error
    ID() uint64  // 全局唯一连接 ID
}
```

### goroutineConn（V1 默认实现）

```go
type goroutineConn struct {
    id   uint64
    conn net.Conn

    readChan  chan Message
    writeChan chan Message
    closeChan chan struct{}
    closeOnce sync.Once
}

func newGoroutineConn(nc net.Conn) *goroutineConn
```

### 握手

- `ServerHandshake(w http.ResponseWriter, r *http.Request) (*Conn, error)` — 解析 Upgrade 请求，返回 101
- `ClientHandshake(url string, headers http.Header) (*Conn, error)` — 发起客户端握手

### goroutineConn 关键行为

- 启动两个 goroutine：读循环（net.Conn → readChan）、写循环（writeChan → net.Conn）
- 读写 buffer 使用 `sync.Pool` 复用，减少 GC 压力
- 收到 Ping 自动回复 Pong（默认不暴露到 readChan）
- 收到 Close 帧自动回复 Close 帧并关闭连接
- channel 带缓冲（默认 256），避免 goroutine 阻塞
- `Close()` 方法优雅关闭：发送 Close 帧，等待对端确认，超时后强制关闭
- `sync.Once` 保证 Close 只执行一次

## 会话层（internal/session 包）

### 状态定义

```go
type State int

const (
    StateDisconnected State = iota
    StateConnecting
    StateConnected
    StateReconnecting
    StateClosed
)
```

### Session 结构

```go
type Session struct {
    conn conn.Conn  // 接口，非具体类型

    // 心跳（可替换实现）
    heartbeater Heartbeater

    // 心跳配置
    PingInterval time.Duration
    PongTimeout  time.Duration

    // 重连配置（客户端）
    ReconnectInterval time.Duration
    MaxReconnect      int

    // 状态
    StateChan <-chan State
    stateChan chan State

    // 透传 conn 的消息 channel
    ReadChan  <-chan Message
    WriteChan chan<- Message
}

// Heartbeater 心跳接口，V1 默认 perConnHeartbeater，V2 可替换 timeWheelHeartbeater
type Heartbeater interface {
    Start(c conn.Conn)
    Stop()
}
```

### 心跳保活

- 定时发 Ping（默认 30s 间隔）
- 等待 Pong 回复，超时未收到（默认 60s）则认为连接断开
- 心跳 goroutine 随连接关闭自动退出

### 自动重连（客户端）

- 连接断开后按 `ReconnectInterval`（默认 5s）重试
- 最多重试 `MaxReconnect` 次（默认 5 次）
- 重连期间状态为 `StateReconnecting`
- 重连成功后恢复心跳，状态变为 `StateConnected`
- 重连失败达到上限，状态变为 `StateClosed`

### 状态管理

- 每次状态变更通过 `StateChan` 通知上层
- 状态转换：Disconnected → Connecting → Connected → (断开) → Reconnecting → Connected / Closed

## 类型导出

`ws` 根包 re-export 子包的关键类型，用户只需 import `ws` 即可使用：

```go
// types.go
type Opcode = frame.Opcode
type Message = internal/conn.Message
type State = internal/session.State
type Conn = internal/conn.Conn
type CloseError = errors.CloseError

const (
    OpcodeText   = frame.OpcodeText
    OpcodeBinary = frame.OpcodeBinary
    OpcodeClose  = frame.OpcodeClose
    OpcodePing   = frame.OpcodePing
    OpcodePong   = frame.OpcodePong
)

const (
    StateDisconnected = session.StateDisconnected
    StateConnecting   = session.StateConnecting
    StateConnected    = session.StateConnected
    StateReconnecting = session.StateReconnecting
    StateClosed       = session.StateClosed
)
```

## 用户 API

### Client

```go
type ClientConfig struct {
    URL               string
    Headers           http.Header
    PingInterval      time.Duration  // 默认 30s
    PongTimeout       time.Duration  // 默认 60s
    ReconnectInterval time.Duration  // 默认 5s
    MaxReconnect      int            // 默认 5
}

func NewClient(cfg ClientConfig) *Client

// Connect 发起 WebSocket 连接，阻塞直到连接成功或首次握手失败
// 连接断开后，如果启用了重连，自动在后台重试
func (c *Client) Connect() error

// Close 关闭客户端，停止重连，释放资源
func (c *Client) Close() error
```

Client 嵌入 Session，直接暴露 ReadChan / WriteChan / StateChan。

`NewClient` 只创建客户端实例，不发起连接。需要显式调用 `Connect()` 开始连接。这样设计的原因：
- 用户可以在连接前注册状态监听
- 连接失败时可以决定是否重试
- 与 `Close()` 对称，生命周期清晰

使用示例：

```go
c := ws.NewClient(ws.ClientConfig{URL: "ws://localhost:8080/ws"})
if err := c.Connect(); err != nil {
    log.Fatal(err)
}
go func() {
    for msg := range c.ReadChan {
        fmt.Println(string(msg.Data))
    }
}()
c.WriteChan <- ws.Message{Type: ws.OpcodeText, Data: []byte("hello")}
```

### Server

```go
type ServerConfig struct {
    Addr              string           // 监听地址
    PingInterval      time.Duration    // 默认 30s
    PongTimeout       time.Duration    // 默认 60s
    MaxConnections    int              // 最大连接数，0 表示不限制
    HandshakeTimeout  time.Duration    // 握手超时，默认 10s
    ReadBufferSize    int              // 读 buffer 大小，默认 4096
    WriteBufferSize   int              // 写 buffer 大小，默认 4096
}

func NewServer(cfg ServerConfig) *Server

type Server struct {
    ConnChan <-chan *Session  // 新连接通知
    Hub      *Hub             // 内置连接管理中心
}

func (s *Server) ListenAndServe() error
func (s *Server) Shutdown(ctx context.Context) error  // 优雅关闭
```

### Hub（连接管理中心）

```go
type Hub struct {
    conns      map[uint64]*Session  // 仅在 Run() goroutine 中访问
    register   chan *Session
    unregister chan *Session
    broadcast  chan Message
    count      int

    // 所有查询也通过 channel 驱动，确保并发安全
    getReq     chan uint64
    getResp    chan *Session
    countReq   chan struct{}
    countResp  chan int
}

func (h *Hub) Run()                        // 单 goroutine 事件循环，唯一访问 conns 的地方
func (h *Hub) Register(s *Session)         // 注册连接
func (h *Hub) Unregister(id uint64)        // 注销连接
func (h *Hub) Broadcast(msg Message)        // 广播消息到所有连接
func (h *Hub) Send(id uint64, msg Message)  // 定向发送
func (h *Hub) Count() int                  // 当前连接数（通过 channel 查询）
func (h *Hub) Get(id uint64) *Session      // 按 ID 查找（通过 channel 查询）
```

Hub 单 goroutine 运行，**所有 map 访问（包括 Get/Count）都通过 channel 驱动**，确保零并发冲突。百万连接下仍只需 1 个 goroutine 管理所有连接的注册/注销/广播。

使用示例：

```go
srv := ws.NewServer(ws.ServerConfig{Addr: ":8080"})

// 启动 Hub 事件循环
go srv.Hub.Run()

go func() {
    for session := range srv.ConnChan {
        srv.Hub.Register(session)
        go handleSession(session)
    }
}()

srv.ListenAndServe()

// 广播消息
srv.Hub.Broadcast(ws.Message{Type: ws.OpcodeText, Data: []byte("hello all")})

// 定向发送
session := srv.Hub.Get(connID)
session.WriteChan <- ws.Message{Type: ws.OpcodeText, Data: []byte("hello you")}
```

## 错误处理

- 协议层错误（帧格式错误）→ 关闭连接，发送 Close 帧（status 1002）
- 握手错误 → 返回 HTTP 400，不建立连接
- 读写错误 → 关闭连接，触发状态变更为 Disconnected
- 重连耗尽 → 状态变更为 Closed，ReadChan 关闭

## 测试策略

- 协议层：单元测试，直接构造字节序列验证帧解析/序列化
- 连接层：使用 `net.Pipe()` 创建同步管道，无需真实网络
- 会话层：mock Conn，验证心跳/重连/状态逻辑
- 集成测试：启动真实 Server + Client，端到端验证
- V1 压力测试：至少 10K 连接，验证 goroutine/channel/buffer 内存增长趋势，确认 V2 演进路径可行

# ws — Go WebSocket 库技术说明

## 1. 项目概览

`ws` 是一个从零实现 RFC 6455 WebSocket 协议的 Go 语言库，不依赖任何第三方 WebSocket 库。提供服务端和客户端双端能力，以 Channel 为核心 API 风格，内置心跳保活、自动重连、状态管理、连接管理中心（Hub）等生产级特性。

设计目标：第一版聚焦功能完整性，同时为单机百万级连接预留架构演进路径。

## 2. 分层架构

```
┌─────────────────────────────────────────────────────┐
│                   用户 API 层                        │
│            Client                    Server          │
├─────────────────────────────────────────────────────┤
│                   会话层 (session)                   │
│        State · Heartbeater · Reconnector            │
├─────────────────────────────────────────────────────┤
│                   连接层 (conn)                      │
│     Conn 接口 · goroutineConn · Handshake           │
├─────────────────────────────────────────────────────┤
│                   协议层 (frame)                     │
│      Frame · ReadFrame · WriteFrame · Mask · Pool   │
└─────────────────────────────────────────────────────┘
```

每一层只依赖下一层，不跨层调用。这种分层的核心价值在于：**每一层可以独立替换实现而不影响上下层**。例如 V2 将 goroutineConn 替换为基于 epoll 的实现时，协议层和会话层代码无需任何改动。

## 3. 协议层 — frame 包

协议层是唯一对外公开的子包（`ws/frame`），因为帧解析能力有独立使用价值——用户可能只需要解析 WebSocket 帧而不需要连接管理。

### 3.1 帧格式

严格遵循 RFC 6455 Section 5.2 定义的数据帧格式：

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-------+-+-------------+-------------------------------+
|F|R|R|R| opcode|M| Payload len |    Extended payload length    |
|I|S|S|S|  (4)  |A|     (7)     |            (16/64)            |
|N|V|V|V|       |S|             |                               |
+-+-+-+-+-------+-+-------------+-------------------------------+
|     Extended payload length continued, if payload len == 127  |
+-------------------------------+-------------------------------+
|                               |Masking-key, if MASK set to 1  |
+-------------------------------+-------------------------------+
| Masking-key (continued)       |          Payload Data         |
+-------------------------------- - - - - - - - - - - - - - - - +
```

`Frame` 结构体直接映射上述格式：

```go
type Frame struct {
    FIN     bool        // 是否为最后一个分片
    RSV1    bool        // 扩展预留
    RSV2    bool
    RSV3    bool
    Opcode  Opcode      // 操作码：Text(0x1) Binary(0x2) Close(0x8) Ping(0x9) Pong(0xA)
    Masked  bool        // 是否掩码
    MaskKey [4]byte     // 掩码密钥
    Payload []byte      // 载荷数据
}
```

### 3.2 WriteFrame — 帧序列化

`WriteFrame` 将 Frame 结构体序列化为二进制格式写入 `io.Writer`。处理三种载荷长度模式：

| 载荷长度 | 编码方式 | 字段值 |
|---|---|---|
| 0–125 | 直接编码到 7 位 | `payloadLen` |
| 126–65535 | 7 位字段填 126，后跟 2 字节大端 uint16 | `0x7E` + 16bit |
| > 65535 | 7 位字段填 127，后跟 8 字节大端 uint64 | `0x7F` + 64bit |

掩码处理：如果 `Masked=true`，在帧头后追加 4 字节 MaskKey，载荷做 XOR 掩码后写入。

### 3.3 ReadFrame — 帧解析与分片重组

`ReadFrame` 从 `io.Reader` 读取并解析一个完整的 WebSocket 消息。核心逻辑：

1. 读取 2 字节帧头，解析 FIN/RSV/Opcode/Masked/PayloadLen
2. 根据 PayloadLen 读取扩展长度（16bit 或 64bit）
3. 如果 Masked，读取 4 字节 MaskKey
4. 读取载荷，如果 Masked 则去掩码
5. **分片重组**：如果 FIN=false，递归读取后续 continuation 帧，拼接 Payload，直到收到 FIN=true 的帧

分片重组的设计决策：对上层透明——调用者始终收到完整的、已重组的消息，不需要关心底层是否分片。这是 V1 的简化处理，代价是大量分片帧会占用读循环直到全部收到。V2 可考虑提供流式读取模式。

### 3.4 掩码处理

RFC 6455 要求客户端→服务端的帧必须掩码，服务端→客户端的帧不得掩码。掩码算法是简单的 XOR：

```
masked[i] = payload[i] ^ maskKey[i % 4]
```

`applyMask` 是无状态纯函数，输入输出都是 `[]byte`，不修改原始数据。`GenerateMaskKey` 使用 `crypto/rand` 生成随机 4 字节密钥。

### 3.5 两级 Buffer Pool

帧读写频繁分配/释放内存，百万连接下 GC 压力巨大。`pool.go` 使用 `sync.Pool` 实现两级缓冲池：

```
小消息池 (≤512B)  ← 心跳帧、控制帧、短文本
常规池 (≤4096B)   ← 典型业务消息
超过 4096B        ← 直接分配，交给 GC
```

`GetBuf(size)` 按预估大小选择池子，`PutBuf(buf)` 按 `cap` 归位。V2 可考虑增加大消息池或分级更细。

## 4. 连接层 — internal/conn 包

连接层管理一条 WebSocket 连接的完整生命周期：握手 → 读写循环 → 关闭。

### 4.1 Conn 接口

```go
type Conn interface {
    ReadChan()  <-chan Message
    WriteChan() chan<- Message
    Close() error
    RemoteAddr() net.Addr
    LocalAddr()  net.Addr
    ID() uint64
}
```

**为什么是接口而不是结构体？** 这是最关键的架构决策之一。V1 的 `goroutineConn` 每个连接占用 2 个 goroutine（读循环 + 写循环），百万连接 = 200 万 goroutine ≈ 4GB 栈内存。V2 将实现 `epollConn`，只在连接活跃时才绑定 goroutine，空闲连接零 goroutine 开销。接口化确保切换实现时用户代码零改动。

### 4.2 goroutineConn — V1 默认实现

```
                    ┌──────────┐
 net.Conn ──────►  │ readLoop  │ ──► readChan ──► 上层
                    └──────────┘
                    ┌──────────┐
 writeChan ──►     │ writeLoop │ ──► net.Conn
                    └──────────┘
```

**读循环** (`readLoop`)：
- 调用 `frame.ReadFrame` 从 `net.Conn` 读取帧
- 收到 **Ping**：自动构造 Pong 帧写回，同时将 Ping 消息放入 `readChan`（让上层感知心跳）
- 收到 **Close**：自动回复 Close 帧，将 Close 消息放入 `readChan`，然后退出循环
- 收到 **Text/Binary**：放入 `readChan`
- 读到错误或 `closeChan` 关闭时退出，退出时 `close(readChan)` 通知上层

**写循环** (`writeLoop`)：
- 从 `writeChan` 读取 `Message`，转换为 `Frame`，调用 `frame.WriteFrame` 写入 `net.Conn`
- 客户端连接（`isClient=true`）自动为每帧生成随机 MaskKey 并掩码
- 写入失败或 `closeChan` 关闭时退出

**Close**：
- `sync.Once` 保证只执行一次
- 先 `close(closeChan)` 通知读写循环退出
- 尝试发送 Close 帧（优雅关闭），然后关闭底层 `net.Conn`

### 4.3 连接 ID

每个连接分配全局唯一 `uint64` ID，使用 `atomic.AddUint64` 递增。用于 Hub 中的 O(1) 查找，避免使用 string 类型 key 带来的内存和比较开销。

### 4.4 握手协议

**服务端握手** (`ServerHandshake`)：
1. 验证 HTTP 方法为 GET
2. 验证 `Upgrade: websocket` 头
3. 验证 `Connection` 头包含 `upgrade`
4. 验证 `Sec-WebSocket-Key` 非空
5. 验证 `Sec-WebSocket-Version: 13`
6. 计算 `Sec-WebSocket-Accept`：`base64(sha1(secKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))`
7. 通过 `http.Hijacker` 接管 TCP 连接
8. 写入 101 Switching Protocols 响应
9. 创建 `goroutineConn(isClient=false)`

**客户端握手** (`ClientHandshake`)：
1. 解析 URL，区分 `ws://`（TCP 80）和 `wss://`（TLS 443）
2. 建立 TCP 连接
3. 生成 16 字节随机 `Sec-WebSocket-Key`
4. 发送 HTTP Upgrade 请求
5. 验证服务端返回 101 状态码和 `Sec-WebSocket-Accept`
6. 创建 `goroutineConn(isClient=true)`

## 5. 会话层 — internal/session 包

会话层在连接层之上增加三个横切关注点：状态管理、心跳保活、自动重连。

### 5.1 状态机

```
Disconnected ──► Connecting ──► Connected
                                   │
                                   ▼ (连接断开)
                              Reconnecting ──► Connecting ──► Connected
                                   │
                                   ▼ (重连耗尽)
                                  Closed
```

状态变更通过 `StateChan` 异步通知上层，channel 缓冲区为 16，防止状态变更频繁时阻塞。`SetState` 使用 `select + default` 非阻塞写入——如果上层还没消费旧状态，新状态直接覆盖。这是刻意的设计：状态是最新值语义，不是事件队列语义。

### 5.2 心跳保活

`Heartbeater` 接口允许替换心跳实现：

```go
type Heartbeater interface {
    Start(c conn.Conn)
    Stop()
    SetOnTimeout(fn func())
}
```

V1 实现 `perConnHeartbeater`：每个连接一个独立 goroutine，定时通过 `WriteChan` 发送 Ping 帧。

**关键设计决策：心跳不消费 ReadChan。** `ReadChan` 是用户收消息的唯一通道，如果心跳 goroutine 也从中读取 Pong，会偷走用户的消息。V1 的策略是：心跳只发 Ping，连接死亡时 Ping 写入失败，写循环退出，读循环随后退出，上层通过 ReadChan 关闭感知断连。Pong 超时检测留给 V2 的时间轮实现。

### 5.3 自动重连

`Reconnector` 在连接断开后按配置间隔重试拨号：

- 每次重试先进入 `StateConnecting`，失败后退回 `StateReconnecting`
- 重连成功后更新 Session 的 `connection`/`readChan`/`writeChan` 字段（同包访问），恢复心跳
- 达到最大重试次数后进入 `StateClosed`

重连器通过 `dial` 函数注入拨号逻辑，与具体握手实现解耦。

## 6. Hub — 连接管理中心

Hub 是百万连接场景的必备基础设施，提供连接注册/注销/广播/定向发送/查找/计数。

### 6.1 架构：单 goroutine 事件循环

```
          Register ──┐
         Unregister ─┤
          Broadcast ─┤
            Get ─────┼──► Run() ──► map[uint64]*Session
           Count ────┤         (唯一访问点)
            Stop ────┘
```

**所有 map 操作（包括 Get 和 Count）都通过 channel 驱动，由 `Run()` 中的单个 goroutine 串行执行。** 这是并发安全的核心保证——没有锁，没有 sync.Map，只有一个 goroutine 拥有 map 的唯一访问权。

`Get` 和 `Count` 使用请求-响应 channel 对（`getReq`/`getResp`、`countReq`/`countResp`）实现同步查询：

```go
func (h *Hub) Get(id uint64) *session.Session {
    h.getReq <- id        // 发送查询请求
    return <-h.getResp    // 等待查询结果
}
```

这种模式比 `sync.RWMutex` 更适合高并发读场景——读请求在 channel 中排队，不会被写操作阻塞，也不会出现读写锁的写饥饿问题。

### 6.2 广播

广播时遍历 `conns` map，向每个 Session 的 `WriteChan` 非阻塞写入。如果某个连接的 WriteChan 已满（`default` 分支），跳过该连接——宁可丢消息也不让广播 goroutine 阻塞在一个慢连接上。

## 7. 用户 API 层

### 7.1 类型导出

`types.go` 使用 Go 类型别名将子包类型 re-export 到根包：

```go
type Opcode = frame.Opcode
type Message = conn.Message
type State = session.State
type Conn = conn.Conn
```

用户只需 `import "github.com/lufeijun/goTools/ws"`，无需关心内部包路径。类型别名（`=`）而非新类型，确保与子包类型完全等价，可以互相赋值。

### 7.2 错误类型

`CloseError` 复用 WebSocket Close Code 作为错误码，不另建体系：

```go
type CloseError struct {
    Code   uint16   // RFC 6455 Close Code
    Reason string
    Cause  error    // 底层错误
}
```

预定义 6 个标准错误（1002/1003/1007/1008/1009/1011），`WithCause` 方法用于包装底层错误，`Unwrap` 方法支持 `errors.Is`/`errors.As` 错误链。

### 7.3 Server

Server 内嵌 Hub，通过 `http.HandleFunc` 处理 WebSocket 升级请求。请求处理流程：

1. 检查 `MaxConnections` 限制（查询 Hub.Count）
2. 调用 `ServerHandshake` 完成 WebSocket 握手
3. 创建 Session，安装 Heartbeater
4. 设置状态为 Connected，启动心跳
5. 将 Session 通过 `ConnChan` 推送给上层

`Shutdown` 方法先停止 Hub 事件循环，再调用 `http.Server.Shutdown` 优雅关闭 HTTP 服务。

### 7.4 Client

Client 的 `Connect()` 方法：
1. 调用 `ClientHandshake` 建立 WebSocket 连接
2. 创建 Session，安装 Heartbeater
3. 设置状态为 Connected，启动心跳

`ReadChan`/`WriteChan`/`StateChan` 透传 Session 的对应 channel。`Send` 是 `WriteChan <- msg` 的语法糖。`Close` 委托给 Session 的 Close（停止心跳 + 发送 Close 帧 + 关闭底层连接）。

## 8. 数据流

### 8.1 服务端收消息

```
客户端 ──TCP──► net.Conn ──► readLoop(ReadFrame) ──► readChan ──► Session.ReadChan ──► 用户
```

### 8.2 服务端发消息

```
用户 ──► Session.WriteChan ──► writeChan ──► writeLoop(WriteFrame) ──► net.Conn ──TCP──► 客户端
```

### 8.3 广播

```
Hub.Broadcast(msg) ──► broadcast chan ──► Run()遍历conns ──► 各Session.WriteChan ──► 各writeLoop
```

## 9. 高性能演进路径

V1 的架构约束为百万级连接预留了清晰的演进路线：

| 瓶颈 | V1 实现 | V2 演进 |
|---|---|---|
| goroutine 开销 | goroutine-per-conn（2/连接） | epoll + goroutine 池，活跃连接才绑定 goroutine |
| channel 开销 | channel 模型（2/连接） | 新增 zero-channel 回调模式 |
| 定时器开销 | time.Ticker per conn | 时间轮（time wheel），单 goroutine 管理所有超时 |
| buffer 内存 | sync.Pool 两级复用 | 分级更细的池 + 大消息池 |

**接口化是演进的关键**：Conn 接口 → V2 新增 epollConn 实现；Heartbeater 接口 → V2 新增 timeWheelHeartbeater 实现。用户代码通过接口交互，切换实现时零改动。

## 10. 包可见性设计

```
ws/frame/          ← 公有包，帧解析可独立使用
ws/internal/conn/  ← 私有包，连接管理是内部实现
ws/internal/session/ ← 私有包，会话管理是内部实现
ws/                ← 根包，用户 API + 类型导出
```

`frame` 保持公有，因为用户可能只需要帧解析能力（比如自定义连接管理）。`conn` 和 `session` 放入 `internal`，防止用户依赖内部实现细节——这样 V2 替换实现时不会破坏任何外部代码。

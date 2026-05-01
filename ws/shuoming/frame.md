# ws/frame — RFC 6455 协议层

`ws/frame` 是 WebSocket 协议的帧解析/序列化层，完整实现 RFC 6455 帧格式。该包从 v1 保留并继续公开，可独立使用（不依赖其他 ws 子包）。

---

## 设计定位

`frame` 是 ws 库的最底层之一，职责单一：**把二进制字节流与 Frame 结构体互相转换**。

- **无外部依赖**：不依赖 `buf`、`pipeline`、`conn` 等上层包
- **独立可用**：任何需要 RFC 6455 帧解析的 Go 项目都可以直接 import
- **兼容 v1**：v1 的测试用例全部保留，确保协议实现稳定性
- **高安全**：MaxFrameSize 校验、分片重组上限，防止 DoS
- **双模式支持**：阻塞读取（`ReadFrame`）和非阻塞增量解析（`IncrementalParser`）

---

## 帧结构

RFC 6455 帧头格式：

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-------+-+-------------+-------------------------------+
|F|R|R|R| opcode|M| Payload len |    Extended payload length    |
|I|S|S|S|  (4)  |A|     (7)     |             (16/64)           |
|N|V|V|V|       |S|             |   (if payload len==126/127)   |
| |1|2|3|       |K|             |                               |
+-+-+-+-+-------+-+-------------+ - - - - - - - - - - - - - - - +
|     Extended payload length continued, if payload len == 127  |
+ - - - - - - - - - - - - - - - +-------------------------------+
|                               |Masking-key, if MASK set to 1  |
+-------------------------------+-------------------------------+
| Masking-key (continued)       |          Payload Data         |
+-------------------------------- - - - - - - - - - - - - - - - +
:                     Payload Data continued ...                :
+ - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - - +
|                     Payload Data continued ...                |
+---------------------------------------------------------------+
```

### Frame 结构体

```go
type Frame struct {
    FIN     bool       // 是否为最后一帧
    RSV1    bool       // 扩展保留位 1
    RSV2    bool       // 扩展保留位 2
    RSV3    bool       // 扩展保留位 3
    Opcode  Opcode     // 操作码（4 位）
    Masked  bool       // 是否掩码
    MaskKey [4]byte    // 掩码密钥（仅客户端发送时设置）
    Payload []byte     // 载荷数据
}
```

### Opcode 定义

```go
type Opcode byte

const (
    OpcodeText   Opcode = 0x1  // 文本帧
    OpcodeBinary Opcode = 0x2  // 二进制帧
    OpcodeClose  Opcode = 0x8  // 关闭帧
    OpcodePing   Opcode = 0x9  // Ping 帧
    OpcodePong   Opcode = 0xA  // Pong 帧
)
```

---

## 核心 API

### 构造帧（便捷函数）

```go
func NewTextFrame(data []byte) Frame
func NewBinaryFrame(data []byte) Frame
func NewPingFrame(data []byte) Frame
func NewPongFrame(data []byte) Frame
func NewCloseFrame(code uint16, reason string) Frame
```

### 序列化

#### WriteFrame（标准路径）

```go
func WriteFrame(w io.Writer, f Frame) error
```

将 `Frame` 序列化为二进制并写入 `io.Writer`。逻辑：

1. 构造第 1 字节：`FIN/RSV/Opcode`
2. 构造第 2 字节：`MASK` 位 + Payload 长度
3. 若长度 >125，追加 2 字节（126）或 8 字节（127）扩展长度
4. 若 `Masked`，追加 4 字节掩码密钥，并对 Payload 做 XOR 掩码
5. 写入 Payload
6. **单次 `w.Write()` 发送全部数据**，避免 Nagle + Delayed ACK 交互延迟

#### WriteFrameTo（零拷贝路径）

```go
func WriteFrameTo(dst buf.ByteBuf, f Frame) error
```

直接序列化到传入的 `ByteBuf`，无堆分配。配合 Pool 使用可实现零 alloc 写帧：

```go
pool := buf.NewPool(512, 4096, 65536)
bb := pool.Get(256)
_ = WriteFrameTo(bb, f)
// 写入连接...
bb.Release()
```

### 解析

#### ReadFrame（标准路径）

```go
func ReadFrame(r io.Reader) (Frame, error)
```

从 `io.Reader` 读取并解析完整帧，自动处理分片帧合并。

#### ReadFrameLimit（安全路径）

```go
func ReadFrameLimit(r io.Reader, maxPayload int) (Frame, error)
```

带 MaxFrameSize 限制的帧解析：

- 读取 payload 长度后立即校验，超过 `maxPayload` 返回 `MaxFrameSizeError`
- 分片重组时累加已读长度，超过限制立即报错断开
- **防止恶意客户端发送极大长度帧头导致 OOM**

#### ReadFrameFromBuf（零拷贝 Peek+Skip 路径）

```go
func ReadFrameFromBuf(bb buf.ByteBuf, maxPayload int) (Frame, error)
```

从 `ByteBuf` 使用 **Peek+Skip** 零拷贝解析帧：

- 帧头通过 `Peek` 查看底层数组，不移动 reader index
- 完整帧解析完成后通过 `Skip` 消费已读字节
- Payload 直接引用 `ByteBuf` 底层数组，无需 `make` 临时 slice
- 若 `ByteBuf` 数据不完整，返回 `io.ErrShortBuffer` 并 **rewind reader index**
- 适合 `bufio.Reader` 批量预读后的内存中解析

```go
bb := buf.NewByteBuf(512)
bb.Write(rawFrameData)
f, err := ReadFrameFromBuf(bb, 64*1024*1024)
// f.Payload 是 bb 底层数组的视图，零拷贝
```

#### ReadFrameBuf（Pool 零拷贝路径）

```go
func ReadFrameBuf(r io.Reader, pool buf.Pool, maxPayload int) (Frame, buf.ByteBuf, error)
```

从 `io.Reader` 读取并解析帧，使用 Pool 获取 `ByteBuf`：

- 帧头和 Payload 全部写入同一个 `ByteBuf`
- Payload 直接引用 `ByteBuf` 底层数组
- 返回的 `ByteBuf` 由调用者负责 `Release()`
- 适合需要管理 ByteBuf 生命周期的场景（如 Pipeline Handler）

```go
f, bb, err := ReadFrameBuf(reader, pool, maxPayload)
if err != nil {
    return err
}
defer bb.Release()
// f.Payload Backed by bb
```

---

## IncrementalParser：非阻塞增量帧解析

### 为什么需要 IncrementalParser

`ReadFrame` 是**阻塞式**的，它使用 `io.ReadFull` 从 `io.Reader` 读取数据——在数据到达前会阻塞当前 goroutine。这种模式适合 **goroutine-per-conn**（net 模式），每个连接有一个独立 goroutine，阻塞等待数据没有问题。

但在 **epoll 事件驱动模式**（epollConn）下，数据到达是通过 EventLoop 的 `OnEvent` 回调通知的：

```go
// EpollConn.OnEvent 由 EventLoop 的 worker pool 调用
func (c *EpollConn) OnEvent(events uint32) {
    if events&eventloop.EventRead != 0 {
        c.handleReadEvent()   // 一次性读取所有可用数据
    }
}
```

在 `handleReadEvent` 中，我们用 `unix.Read` 非阻塞地读取数据，**一次调用可能读到 0 个、1 个或多个不完整的帧**。此时需要一种**非阻塞的、增量式的**解析方式：

- 数据不完整时不阻塞，等待下次 `OnEvent` 再读
- 数据包含多个帧时，一次解析出所有完整帧
- 跨多次 `Feed` 调用处理分片帧

这就是 `IncrementalParser` 的设计目标。

### 结构体

```go
type IncrementalParser struct {
    maxPayload int            // 最大允许的 payload 大小
    buf        []byte         // 内部缓冲区，累积未解析的数据
    err        error          // 上一次解析错误
    fragState  *fragmentState // 分片帧重组状态
}

// fragmentState 跟踪一个正在重组的分片消息
type fragmentState struct {
    opcode  Opcode // 首帧的操作码（OpcodeText 或 OpcodeBinary）
    payload []byte // 已累积的 payload 数据
}
```

### 构造函数

```go
func NewIncrementalParser(maxPayload int) *IncrementalParser {
    return &IncrementalParser{
        maxPayload: maxPayload,
        buf:        make([]byte, 0, 4096),   // 预分配 4KB，减少扩容
    }
}
```

- `maxPayload` 设置最大帧/消息大小，超过则返回 `MaxFrameSizeError`
- 内部缓冲区预分配 4096 字节，后续通过 `append` 自动扩容

### 核心方法：Feed

```go
func (p *IncrementalParser) Feed(data []byte) []Frame
```

**行为：**

1. 将 `data` 追加到内部缓冲区 `buf`
2. 循环尝试从 `buf` 解析完整帧（调用 `tryParseFrame`）
3. 解析成功的帧追加到返回切片
4. 如果数据不足以解析一个完整帧（`io.ErrShortBuffer`），停止循环，等待下次 `Feed`
5. 如果发生解析错误，记录到 `p.err`，后续 `Feed` 调用返回 nil

**详细流程：**

```
Feed([data1])
  |
  | append data1 到 buf
  |
  +--> tryParseFrame()
  |      |
  |      | 数据不够完整帧 --> return io.ErrShortBuffer --> 退出循环
  |      |
  |      | 解析出完整帧 --> 返回 Frame --> 加入 frames 切片
  |      |                     |
  |      |                     +--> 继续循环 tryParseFrame()
  |      |                              |
  |      |                              | 数据不够 --> 退出循环
  |      |                              | 又一个完整帧 --> 加入 frames
  |      |
  |      | 解析错误 --> 记录 p.err --> 退出循环
  |
  +--> return frames
```

**代码实现：**

```go
func (p *IncrementalParser) Feed(data []byte) []Frame {
    if p.err != nil {
        return nil   // 错误状态，不再解析
    }

    p.buf = append(p.buf, data...)   // 追加新数据

    var frames []Frame

    for {
        frame, err := p.tryParseFrame()
        if err == io.ErrShortBuffer {
            break   // 数据不完整，等待更多数据
        }
        if err != nil {
            p.err = err   // 记录错误，进入错误状态
            break
        }
        frames = append(frames, frame)
    }

    return frames
}
```

### 分片帧处理

WebSocket 允许将一条消息拆分为多个帧发送。IncrementalParser 通过 `fragState` 跨多次 `Feed` 调用跟踪分片状态：

**场景：一个消息被拆成 3 个帧**

```
第 1 次 Feed: 收到 [FIN=0, Opcode=0x1, "Hel"]
  -> fragState = {opcode: 0x1, payload: "Hel"}
  -> 返回 []（没有完整消息）

第 2 次 Feed: 收到 [FIN=0, Opcode=0x0, "lo "]
  -> fragState = {opcode: 0x1, payload: "Hello "}
  -> 返回 []（没有完整消息）

第 3 次 Feed: 收到 [FIN=1, Opcode=0x0, "World"]
  -> 合并: payload = "Hello " + "World" = "Hello World"
  -> fragState = nil
  -> 返回 [Frame{FIN=true, Opcode=0x1, Payload="Hello World"}]
```

**分片处理规则：**

1. **首帧（FIN=0，Opcode 为 0x1/0x2）** — 创建 `fragState`，记录初始 opcode 和 payload
2. **续帧（FIN=0，Opcode=0x0）** — 将 payload 追加到 `fragState.payload`
3. **末帧（FIN=1，Opcode=0x0）** — 将 payload 追加到 `fragState.payload`，合并为完整消息，清空 `fragState`
4. **非分片帧（FIN=1，Opcode 非 0x0）** — 直接返回，不经过 `fragState`

**错误检测：**

- 续帧的 Opcode 不为 0x0 → 返回 `ProtocolError{Code: 1002, Message: "invalid opcode for continuation frame"}`
- 未分片状态下收到 Opcode=0x0 → 返回 `ProtocolError{Code: 1002, Message: "invalid opcode: 0x0 for non-fragmented frame"}`

### MaxFrameSize 校验

`IncrementalParser` 在多个位置检查 payload 大小：

1. **解析单个帧时** — 检查当前帧的 payload 长度是否超过 `maxPayload`
2. **追加续帧 payload 时** — 检查累积的 payload 总长度是否超过 `maxPayload`
3. **合并末帧时** — 检查最终合并后的 payload 总长度是否超过 `maxPayload`

```go
// 单帧检查
if p.maxPayload > 0 && payloadLen > p.maxPayload {
    return Frame{}, &MaxFrameSizeError{Limit: p.maxPayload, Payload: payloadLen}
}

// 续帧累积检查
if p.maxPayload > 0 && len(p.fragState.payload) > p.maxPayload {
    return Frame{}, &MaxFrameSizeError{Limit: p.maxPayload, Payload: len(p.fragState.payload)}
}

// 末帧合并后检查
if p.maxPayload > 0 && len(result.Payload) > p.maxPayload {
    return Frame{}, &MaxFrameSizeError{Limit: p.maxPayload, Payload: len(result.Payload)}
}
```

**一旦超过限制，Parser 进入错误状态**，后续 `Feed` 调用返回 nil，直到调用 `Reset()` 重置。

### 错误处理

```go
// Err 返回上一次解析错误
func (p *IncrementalParser) Err() error {
    return p.err
}

// Reset 清除所有内部状态和错误，允许 Parser 被复用
func (p *IncrementalParser) Reset() {
    p.buf = p.buf[:0]   // 复用缓冲区，避免重新分配
    p.err = nil
    p.fragState = nil
}
```

**错误类型：**

```go
// ProtocolError 表示 WebSocket 协议违规
type ProtocolError struct {
    Code    int    // RFC 6455 关闭码
    Message string // 人类可读的错误消息
}

// MaxFrameSizeError 表示帧/消息超过了最大允许大小
type MaxFrameSizeError struct {
    Limit   int  // 最大允许大小
    Payload int  // 实际大小
}
```

**错误状态机：**

```
正常状态 --解析错误--> 错误状态
    ^                      |
    |                      | Reset()
    +----------------------+
```

- 正常状态：`Feed` 正常解析，`Err()` 返回 nil
- 错误状态：`Feed` 返回 nil，`Err()` 返回错误
- `Reset()` 将任何状态恢复为正常状态

---

## IncrementalParser vs ReadFrame 对比

| 对比项 | ReadFrame / ReadFrameLimit | IncrementalParser |
|---|---|---|
| **I/O 模式** | 阻塞，从 `io.Reader` 读取 | 非阻塞，由调用方 `Feed` 数据 |
| **适用场景** | goroutine-per-conn（net 模式） | 事件驱动（epoll 模式） |
| **数据来源** | 内部调用 `io.ReadFull` | 外部调用 `Feed(data)` |
| **数据不完整时** | 阻塞等待更多数据 | 返回空切片，等待下次 Feed |
| **分片帧处理** | 内部循环读取续帧，一次性返回 | 通过 `fragState` 跨多次 Feed 累积 |
| **依赖** | `io.Reader` | 无外部依赖，纯内存操作 |
| **错误恢复** | 返回 error，调用方决定是否继续 | 进入错误状态，需 `Reset()` 恢复 |
| **使用位置** | `serveConnNet`（net 模式的主循环） | `EpollConn.handleReadEvent`（epoll 模式的事件回调） |

### ReadFrame 的分片帧处理

```go
// ReadFrame 内部通过循环一次性读取所有续帧
if !result.FIN {
    firstOpcode := result.Opcode
    accumulated := result.Payload
    for {
        next, err := readFrameWithAccumulated(r, maxPayload, totalLen)
        accumulated = append(accumulated, next.Payload...)
        if next.FIN {
            result.Payload = accumulated
            break
        }
    }
}
```

**关键区别：** ReadFrame 在一个 goroutine 中循环阻塞读取，直到收到 FIN=1 的末帧。IncrementalParser 则将分片状态保存在 `fragState` 中，每次 `Feed` 处理一部分数据，多次调用完成重组。

---

## 完整使用示例：EpollConn 中的 IncrementalParser

以下是 `EpollConn.handleReadEvent` 的实际代码，展示了 IncrementalParser 在事件驱动模式下的典型用法：

```go
func (c *EpollConn) handleReadEvent() {
    tmp := make([]byte, 4096)
    for {
        // 非阻塞读取
        n, err := unix.Read(c.fd, tmp)
        if n > 0 {
            if c.onFrame != nil && c.frameParser != nil {
                // 将读到的数据喂给增量解析器
                frames := c.frameParser.Feed(tmp[:n])

                // 处理所有已解析的完整帧
                for _, f := range frames {
                    c.onFrame(f)   // 回调上层处理
                }

                // 检查解析错误
                if c.frameParser.Err() != nil {
                    c.Close()   // 协议违规或超限，关闭连接
                    return
                }
            } else {
                // 无 onFrame 回调时，写入 readBuf
                c.readMu.Lock()
                c.readBuf.Write(tmp[:n])
                c.readMu.Unlock()
            }
        }
        if err != nil {
            if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
                return   // 数据读完，等待下次事件
            }
            c.Close()   // 其他错误，关闭连接
            return
        }
        if n == 0 {
            c.Close()   // 对端关闭
            return
        }
    }
}
```

**关键点：**

1. **一次 Read 可能读到多个帧的部分数据** — `tmp[:n]` 可能包含 0.5 个帧、1.5 个帧或 3 个帧
2. **Feed 自动处理** — 完整帧会被解析出来，不完整帧留在内部缓冲区等待下次
3. **onFrame 回调** — 每个完整帧都通过回调通知上层，上层可以将消息分发到 Pipeline
4. **错误时关闭连接** — 解析错误表示协议违规或恶意数据，应立即关闭

### 独立使用示例

```go
package main

import (
    "fmt"
    "github.com/lufeijun/goTools/ws/frame"
)

func main() {
    // 创建解析器，最大 payload 64MB
    parser := frame.NewIncrementalParser(64 * 1024 * 1024)

    // 模拟事件驱动场景：数据分多次到达
    // 第一次：收到一个完整的文本帧
    data1 := []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f} // "Hello"
    frames1 := parser.Feed(data1)
    for _, f := range frames1 {
        fmt.Printf("收到帧: Opcode=%d, Payload=%s\n", f.Opcode, string(f.Payload))
    }

    // 第二次：收到半帧
    data2 := []byte{0x82, 0x04, 0x01, 0x02}   // 二进制帧头+2字节，缺后2字节
    frames2 := parser.Feed(data2)
    fmt.Printf("第二次: 解析出 %d 个帧\n", len(frames2))  // 0

    // 第三次：补齐剩余数据 + 新帧
    data3 := []byte{0x03, 0x04, 0x81, 0x02, 0x4f, 0x4b}  // 补齐 + "OK"
    frames3 := parser.Feed(data3)
    for _, f := range frames3 {
        fmt.Printf("收到帧: Opcode=%d\n", f.Opcode)
    }

    // 检查错误
    if parser.Err() != nil {
        fmt.Println("解析错误:", parser.Err())
    }

    // 重置解析器，复用内部缓冲区
    parser.Reset()
}
```

---

## 掩码处理

RFC 6455 要求客户端发送的帧必须掩码，服务端发送的帧不掩码。

```go
// applyMask 返回新切片，不修改输入（适合客户端 Outbound）
func applyMask(payload []byte, maskKey [4]byte) []byte

// applyMaskInPlace 原地修改，无分配（适合服务端 Inbound）
func applyMaskInPlace(payload []byte, maskKey [4]byte)

// GenerateMaskKey 使用 crypto/rand 生成，密码学安全
func GenerateMaskKey() [4]byte
```

**优化点：** 服务端 Inbound 路径使用 `applyMaskInPlace`，直接修改 `ByteBuf` 底层数组或 `ReadFrameBuf` 的 payload 区域，**每条入站消息减少一次 payload 级别的 alloc**。

**IncrementalParser 中的掩码处理：** `tryParseFrame` 在提取 payload 后，如果帧是掩码的，调用 `applyMaskInPlace` 原地解掩码，并将 `Masked` 标记清除。这意味着返回给调用方的 `Frame` 已经是解掩码后的数据。

---

## 底层 buffer 池

`frame` 包内部使用轻量级 `sync.Pool` 复用小数组，减少 GC 压力。

```go
const (
    smallBufSize   = 512
    defaultBufSize = 4096
)

func GetBuf(size int) []byte
func PutBuf(buf []byte)
```

这是 frame 包内部优化，用户通常不需要直接调用。

---

## Benchmark 基线

```go
BenchmarkWriteFrame      // 标准 WriteFrame
BenchmarkWriteFrameTo    // 零拷贝 WriteFrameTo（Pool 命中时零 alloc）
BenchmarkReadFrame       // 标准 ReadFrame
BenchmarkIncrementalParser  // IncrementalParser 增量解析
```

运行：`go test ./ws/frame -bench=. -benchmem`

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `frame.go` | `Frame` 结构体、`Opcode`、ReadFrame/ReadFrameLimit/ReadFrameFromBuf/ReadFrameBuf、WriteFrame/WriteFrameTo、便捷构造函数 |
| `parser.go` | `IncrementalParser` 非阻塞增量帧解析器、`fragmentState`、`ProtocolError`、`MaxFrameSizeError` |
| `mask.go` | `applyMask`、`applyMaskInPlace`、`GenerateMaskKey` |
| `pool.go` | 内部 `sync.Pool`：`GetBuf`、`PutBuf` |
| `frame_test.go` | 帧解析/序列化/零拷贝路径测试 |
| `parser_test.go` | IncrementalParser 增量解析、分片帧重组、错误状态测试 |
| `mask_test.go` | 掩码正确性/原地 XOR 测试 |
| `pool_test.go` | buffer 池测试 |
| `frame_bench_test.go` | 性能基准测试 |

---

## 注意事项

1. **ReadFrame 阻塞读取** — 使用 `io.ReadFull`，在网络层确保数据到达前会阻塞
2. **IncrementalParser 非阻塞** — 数据不完整时返回空切片，适合事件驱动 I/O
3. **ReadFrameFromBuf 不阻塞** — 从内存 ByteBuf 解析，数据不完整时返回 `io.ErrShortBuffer`
4. **自动合并不保留中间帧** — 分片帧的 `RSV`、`MaskKey` 等中间信息丢失，只保留合并后的 `Payload`
5. **Close 帧的 code/reason 由调用方处理** — `NewCloseFrame` 帮你打包，但发送/响应逻辑在 `server`/`client` 层
6. **v2 中 frame 层被 codec.go 封装** — 用户业务代码通常操作 `conn.Message` 而非直接使用 `frame.Frame`
7. **MaxFrameSize 必须设置** — 生产环境应根据业务调整，防止 DoS
8. **IncrementalParser 错误后必须 Reset** — 进入错误状态后 Feed 返回 nil，需调用 Reset 恢复
9. **IncrementalParser 内部缓冲区预分配 4KB** — Reset 时复用缓冲区，避免反复分配
10. **IncrementalParser 解掩码是原地操作** — 返回的 Frame 已经解掩码，`Masked` 标记被清除

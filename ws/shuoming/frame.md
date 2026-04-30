# ws/frame — RFC 6455 协议层

`ws/frame` 是 WebSocket 协议的帧解析/序列化层，完整实现 RFC 6455 帧格式。该包从 v1 保留并继续公开，可独立使用（不依赖其他 ws 子包）。

---

## 设计定位

`frame` 是 ws 库的最底层之一，职责单一：**把二进制字节流与 Frame 结构体互相转换**。

- **无外部依赖**：不依赖 `buf`、`pipeline`、`conn` 等上层包
- **独立可用**：任何需要 RFC 6455 帧解析的 Go 项目都可以直接 import
- **兼容 v1**：v1 的测试用例全部保留，确保协议实现稳定性
- **高安全**：MaxFrameSize 校验、分片重组上限，防止 DoS

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

---

## 分片帧处理

WebSocket 允许将一条消息拆分为多个帧发送（除首帧外，后续为 Continuation 帧，`Opcode = 0x0`）。

`ReadFrame` / `ReadFrameLimit` / `ReadFrameBuf` 内部自动合并：

```go
if !result.FIN {
    firstOpcode := result.Opcode
    accumulated := result.Payload
    totalLen := len(accumulated)
    for {
        next, err := readFrameWithAccumulated(r, maxPayload, totalLen)
        // ...
        accumulated = append(accumulated, next.Payload...)
        totalLen += len(next.Payload)
        if maxPayload > 0 && totalLen > maxPayload {
            return Frame{}, &MaxFrameSizeError{...}
        }
        if next.FIN {
            result.FIN = true
            result.Opcode = firstOpcode
            result.Payload = accumulated
            break
        }
    }
}
```

使用者无需关心分片，每次调用都收到完整消息。

**安全加固：** 分片重组时累加已读长度，超过 `MaxFrameSize` 立即返回错误，防止恶意客户端通过无限 continuation 帧耗尽内存。

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
```

运行：`go test ./ws/frame -bench=. -benchmem`

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `frame.go` | `Frame` 结构体、`Opcode`、ReadFrame/ReadFrameLimit/ReadFrameFromBuf/ReadFrameBuf、WriteFrame/WriteFrameTo、便捷构造函数 |
| `mask.go` | `applyMask`、`applyMaskInPlace`、`GenerateMaskKey` |
| `pool.go` | 内部 `sync.Pool`：`GetBuf`、`PutBuf` |
| `frame_test.go` | 帧解析/序列化/零拷贝路径测试 |
| `mask_test.go` | 掩码正确性/原地 XOR 测试 |
| `pool_test.go` | buffer 池测试 |
| `frame_bench_test.go` | 性能基准测试 |

---

## 注意事项

1. **ReadFrame 阻塞读取** — 使用 `io.ReadFull`，在网络层确保数据到达前会阻塞
2. **ReadFrameFromBuf 不阻塞** — 从内存 ByteBuf 解析，数据不完整时返回 `io.ErrShortBuffer`
3. **自动合并不保留中间帧** — 分片帧的 `RSV`、`MaskKey` 等中间信息丢失，只保留合并后的 `Payload`
4. **Close 帧的 code/reason 由调用方处理** — `NewCloseFrame` 帮你打包，但发送/响应逻辑在 `server`/`client` 层
5. **v2 中 frame 层被 codec.go 封装** — 用户业务代码通常操作 `conn.Message` 而非直接使用 `frame.Frame`
6. **MaxFrameSize 必须设置** — 生产环境应根据业务调整，防止 DoS

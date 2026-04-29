# ws/frame — RFC 6455 协议层

`ws/frame` 是 WebSocket 协议的帧解析/序列化层，完整实现 RFC 6455 帧格式。该包从 v1 保留并继续公开，可独立使用（不依赖其他 ws 子包）。

---

## 设计定位

`frame` 是 ws 库的最底层之一，职责单一：**把二进制字节流与 Frame 结构体互相转换**。

- **无外部依赖**：不依赖 `buf`、`pipeline`、`conn` 等上层包
- **独立可用**：任何需要 RFC 6455 帧解析的 Go 项目都可以直接 import
- **兼容 v1**：v1 的测试用例全部保留，确保协议实现稳定性

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

### 序列化：WriteFrame

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

### 解析：ReadFrame

```go
func ReadFrame(r io.Reader) (Frame, error)
```

从 `io.Reader` 读取并解析完整帧。逻辑：

1. 读取 2 字节基础头部
2. 解析 `FIN`、`RSV`、`Opcode`、`MASK`、基础长度
3. 若长度为 126/127，读取扩展长度字段
4. 若 `Masked`，读取 4 字节掩码密钥，并对 Payload 做去掩码
5. 读取 Payload 数据
6. **自动处理分片帧**：若非 `FIN`，循环读取后续 Continuation 帧，合并 Payload

---

## 掩码处理

RFC 6455 要求客户端发送的帧必须掩码，服务端发送的帧不掩码。

```go
// applyMask XORs the payload with the mask key per RFC 6455.
func applyMask(payload []byte, maskKey [4]byte) []byte

// GenerateMaskKey generates a random 4-byte mask key using crypto/rand.
func GenerateMaskKey() [4]byte
```

- `applyMask` 返回**新切片**，不修改输入
- `GenerateMaskKey` 使用 `crypto/rand` 生成，密码学安全

---

## 分片帧处理

WebSocket 允许将一条消息拆分为多个帧发送（除首帧外，后续为 Continuation 帧，`Opcode = 0x0`）。

`ReadFrame` 内部自动合并：

```go
if !result.FIN {
    firstOpcode := result.Opcode
    accumulated := result.Payload
    for {
        next, err := ReadFrame(r)
        accumulated = append(accumulated, next.Payload...)
        if next.FIN {
            result.FIN = true
            result.Opcode = firstOpcode
            result.Payload = accumulated
            break
        }
    }
}
```

使用者无需关心分片，每次 `ReadFrame` 都收到完整消息。

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

## 文件清单

| 文件 | 内容 |
|---|---|
| `frame.go` | `Frame` 结构体、`Opcode`、ReadFrame、WriteFrame、便捷构造函数 |
| `mask.go` | `applyMask`、`GenerateMaskKey` |
| `pool.go` | 内部 `sync.Pool`：`GetBuf`、`PutBuf` |
| `frame_test.go` | v1 保留的帧解析/序列化测试 |
| `mask_test.go` | 掩码正确性测试 |
| `pool_test.go` | buffer 池测试 |

---

## 注意事项

1. **ReadFrame 阻塞读取** — 使用 `io.ReadFull`，在网络层确保数据到达前会阻塞
2. **自动合并不保留中间帧** — 分片帧的 `RSV`、`MaskKey` 等中间信息丢失，只保留合并后的 `Payload`
3. **Close 帧的 code/reason 由调用方处理** — `NewCloseFrame` 帮你打包，但发送/响应逻辑在 `server`/`client` 层
4. **v2 中 frame 层被 codec.go 封装** — 用户业务代码通常操作 `conn.Message` 而非直接使用 `frame.Frame`

# WebSocket 开源库实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从零实现一个 Go 语言通用 WebSocket 库 V1，支持服务端+客户端双端、Channel 式 API、全部帧类型、心跳保活、自动重连、状态管理，并为百万级连接预留架构演进路径。

**Architecture:** 分层架构：协议层(frame) → 连接层(conn) → 会话层(session) → 用户API(client/server)。Conn 接口化，Hub 内置，Heartbeater 可替换，两级 Buffer Pool。

**Tech Stack:** Go 1.24, 标准库 net/http/crypto/sha1/encoding/binary, sync.Pool, 无第三方依赖

---

## File Structure

| File | Responsibility |
|---|---|
| `ws/frame/frame.go` | Opcode 常量、Frame 结构体、ReadFrame、WriteFrame、便捷构造函数、fragmentation 处理 |
| `ws/frame/mask.go` | 掩码/去掩码函数、generateMaskKey |
| `ws/frame/pool.go` | 两级 sync.Pool（small 512B / default 4KB）、GetBuf、PutBuf |
| `ws/frame/frame_test.go` | 帧解析/序列化/掩码/分片/边界测试 |
| `ws/internal/conn/conn.go` | Message 结构、Conn 接口、goroutineConn 实现、读写循环、Close 逻辑、连接 ID |
| `ws/internal/conn/handshake.go` | 服务端握手 + 客户端握手 |
| `ws/internal/conn/conn_test.go` | net.Pipe 测试读写循环、握手测试 |
| `ws/internal/session/session.go` | State 常量、Session 结构体、状态管理 |
| `ws/internal/session/heartbeat.go` | Heartbeater 接口、perConnHeartbeater 实现 |
| `ws/internal/session/reconnect.go` | 客户端自动重连逻辑 |
| `ws/internal/session/session_test.go` | 心跳/重连/状态 mock 测试 |
| `ws/errors.go` | CloseError 结构体、预定义错误、WithCause |
| `ws/types.go` | re-export 子包类型到 ws 根包 |
| `ws/hub.go` | Hub 结构体、Run 事件循环、Register/Unregister/Broadcast/Send/Get/Count |
| `ws/client.go` | ClientConfig、Client 结构体、NewClient、Connect、Close |
| `ws/server.go` | ServerConfig、Server 结构体、NewServer、ListenAndServe、Shutdown |

---

### Task 1: 项目初始化与错误类型

**Files:**
- Modify: `go.mod`
- Create: `ws/errors.go`, `ws/errors_test.go`

- [ ] **Step 1: 修复 go.mod module 名称并创建 ws 目录**

```bash
mkdir -p ws/frame ws/internal/conn ws/internal/session
```

将 `go.mod` 的 module 改为：

```
module github.com/lufeijun/goTools

go 1.24.4
```

- [ ] **Step 2: 写 CloseError 测试**

Create `ws/errors_test.go`:

```go
package ws

import (
	"errors"
	"testing"
)

func TestCloseError_Error(t *testing.T) {
	e := &CloseError{Code: 1002, Reason: "protocol error"}
	if got := e.Error(); got != "websocket close code 1002: protocol error" {
		t.Errorf("Error() = %q, want %q", got, "websocket close code 1002: protocol error")
	}
}

func TestCloseError_WithCause(t *testing.T) {
	cause := errors.New("underlying")
	e := ErrProtocolError.WithCause(cause)
	if e.Code != 1002 {
		t.Errorf("Code = %d, want 1002", e.Code)
	}
	if e.Cause != cause {
		t.Errorf("Cause = %v, want %v", e.Cause, cause)
	}
}

func TestPredefinedErrors(t *testing.T) {
	cases := []struct {
		err  *CloseError
		code uint16
	}{
		{ErrProtocolError, 1002},
		{ErrUnsupportedData, 1003},
		{ErrInvalidFrame, 1007},
		{ErrPolicyViolation, 1008},
		{ErrMessageTooBig, 1009},
		{ErrInternalError, 1011},
	}
	for _, tc := range cases {
		if tc.err.Code != tc.code {
			t.Errorf("Code = %d, want %d", tc.err.Code, tc.code)
		}
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/ -run TestClose -v`
Expected: FAIL — `CloseError` 未定义

- [ ] **Step 3: 实现 CloseError**

Create `ws/errors.go`:

```go
package ws

import "fmt"

type CloseError struct {
	Code   uint16
	Reason string
	Cause  error
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket close code %d: %s", e.Code, e.Reason)
}

func (e *CloseError) WithCause(cause error) *CloseError {
	return &CloseError{
		Code:   e.Code,
		Reason: e.Reason,
		Cause:  cause,
	}
}

func (e *CloseError) Unwrap() error {
	return e.Cause
}

var (
	ErrProtocolError   = &CloseError{Code: 1002, Reason: "protocol error"}
	ErrUnsupportedData = &CloseError{Code: 1003, Reason: "unsupported data"}
	ErrInvalidFrame    = &CloseError{Code: 1007, Reason: "invalid frame payload data"}
	ErrPolicyViolation = &CloseError{Code: 1008, Reason: "policy violation"}
	ErrMessageTooBig   = &CloseError{Code: 1009, Reason: "message too big"}
	ErrInternalError   = &CloseError{Code: 1011, Reason: "internal error"}
)
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/ -run TestClose -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add go.mod ws/errors.go ws/errors_test.go
git commit -m "feat(ws): add CloseError type with WebSocket close codes"
```

---

### Task 2: 协议层 — 掩码处理

**Files:**
- Create: `ws/frame/mask.go`, `ws/frame/mask_test.go`

- [ ] **Step 1: 写掩码测试**

Create `ws/frame/mask_test.go`:

```go
package frame

import (
	"bytes"
	"testing"
)

func TestApplyMask(t *testing.T) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	payload := []byte("Hello")
	masked := applyMask(payload, key)

	expected := []byte{0x7f, 0x9f, 0x4d, 0x51, 0x58}
	if !bytes.Equal(masked, expected) {
		t.Errorf("applyMask = %x, want %x", masked, expected)
	}
}

func TestApplyMask_Roundtrip(t *testing.T) {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	original := []byte("test payload data")
	masked := applyMask(original, key)
	unmasked := applyMask(masked, key)
	if !bytes.Equal(unmasked, original) {
		t.Errorf("roundtrip failed: got %x, want %x", unmasked, original)
	}
}

func TestApplyMask_Empty(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	result := applyMask([]byte{}, key)
	if len(result) != 0 {
		t.Errorf("empty payload should return empty")
	}
}

func TestGenerateMaskKey(t *testing.T) {
	key1 := generateMaskKey()
	key2 := generateMaskKey()
	if key1 == key2 {
		t.Error("two generated mask keys should differ")
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestApplyMask -v`
Expected: FAIL

- [ ] **Step 2: 实现掩码函数**

Create `ws/frame/mask.go`:

```go
package frame

import "crypto/rand"

func applyMask(payload []byte, maskKey [4]byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ maskKey[i%4]
	}
	return masked
}

func generateMaskKey() [4]byte {
	var key [4]byte
	rand.Read(key[:])
	return key
}
```

- [ ] **Step 3: 运行测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestApplyMask -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/frame/mask.go ws/frame/mask_test.go
git commit -m "feat(frame): add WebSocket frame masking per RFC 6455"
```

---

### Task 3: 协议层 — Buffer Pool

**Files:**
- Create: `ws/frame/pool.go`, `ws/frame/pool_test.go`

- [ ] **Step 1: 写 Pool 测试**

Create `ws/frame/pool_test.go`:

```go
package frame

import "testing"

func TestGetBuf_SmallSize(t *testing.T) {
	buf := GetBuf(100)
	if cap(buf) != smallBufSize {
		t.Errorf("cap(buf) = %d, want %d", cap(buf), smallBufSize)
	}
}

func TestGetBuf_DefaultSize(t *testing.T) {
	buf := GetBuf(2000)
	if cap(buf) != defaultBufSize {
		t.Errorf("cap(buf) = %d, want %d", cap(buf), defaultBufSize)
	}
}

func TestPutBuf_ReturnsToCorrectPool(t *testing.T) {
	small := GetBuf(100)
	PutBuf(small)
	default_ := GetBuf(2000)
	PutBuf(default_)
}

func TestGetBuf_LargerThanDefault(t *testing.T) {
	buf := GetBuf(10000)
	if cap(buf) < 10000 {
		t.Errorf("cap(buf) = %d, want >= 10000", cap(buf))
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestGetBuf -v`
Expected: FAIL

- [ ] **Step 2: 实现 Pool**

Create `ws/frame/pool.go`:

```go
package frame

import "sync"

const (
	smallBufSize   = 512
	defaultBufSize = 4096
)

var smallBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, smallBufSize) },
}

var defaultBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, defaultBufSize) },
}

func GetBuf(size int) []byte {
	if size <= smallBufSize {
		return smallBufPool.Get().([]byte)[:0]
	}
	if size <= defaultBufSize {
		return defaultBufPool.Get().([]byte)[:0]
	}
	return make([]byte, size)
}

func PutBuf(buf []byte) {
	c := cap(buf)
	if c <= smallBufSize {
		smallBufPool.Put(buf)
	} else if c <= defaultBufSize {
		defaultBufPool.Put(buf)
	}
}
```

- [ ] **Step 3: 运行测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestGetBuf -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/frame/pool.go ws/frame/pool_test.go
git commit -m "feat(frame): add two-level buffer pool for memory reuse"
```

---

### Task 4: 协议层 — Frame 结构与 WriteFrame

**Files:**
- Create: `ws/frame/frame.go`, `ws/frame/frame_test.go`

- [ ] **Step 1: 写 WriteFrame 测试**

Create `ws/frame/frame_test.go`:

```go
package frame

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteFrame_TextUnmasked(t *testing.T) {
	f := NewTextFrame([]byte("Hello"))
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("WriteFrame = %x, want %x", buf.Bytes(), expected)
	}
}

func TestWriteFrame_BinaryMasked(t *testing.T) {
	f := NewBinaryFrame([]byte{0x01, 0x02})
	f.Masked = true
	f.MaskKey = [4]byte{0x37, 0xfa, 0x21, 0x3d}

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[0] != 0x82 {
		t.Errorf("first byte = %x, want 0x82", buf.Bytes()[0])
	}
	if buf.Bytes()[1] != 0x82 {
		t.Errorf("second byte = %x, want 0x82", buf.Bytes()[1])
	}
}

func TestWriteFrame_Ping(t *testing.T) {
	f := NewPingFrame(nil)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte{0x89, 0x00}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("WriteFrame ping = %x, want %x", buf.Bytes(), expected)
	}
}

func TestWriteFrame_Pong(t *testing.T) {
	f := NewPongFrame(nil)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte{0x8A, 0x00}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("WriteFrame pong = %x, want %x", buf.Bytes(), expected)
	}
}

func TestWriteFrame_Close(t *testing.T) {
	f := NewCloseFrame(1000, "bye")
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[0] != 0x88 {
		t.Errorf("first byte = %x, want 0x88", buf.Bytes()[0])
	}
	if buf.Bytes()[1] != 0x05 {
		t.Errorf("second byte = %x, want 0x05", buf.Bytes()[1])
	}
}

func TestWriteFrame_MediumPayload(t *testing.T) {
	payload := make([]byte, 126)
	for i := range payload {
		payload[i] = byte(i)
	}
	f := NewTextFrame(payload)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[1] != 0x7E {
		t.Errorf("length indicator = %x, want 0x7E", buf.Bytes()[1])
	}
	length := uint16(buf.Bytes()[2])<<8 | uint16(buf.Bytes()[3])
	if length != 126 {
		t.Errorf("decoded length = %d, want 126", length)
	}
}

func TestWriteFrame_LargePayload(t *testing.T) {
	payload := make([]byte, 65536)
	f := NewBinaryFrame(payload)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[1] != 0x7F {
		t.Errorf("length indicator = %x, want 0x7F", buf.Bytes()[1])
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestWriteFrame -v`
Expected: FAIL

- [ ] **Step 2: 实现 Frame 结构体和 WriteFrame**

Create `ws/frame/frame.go`:

```go
package frame

import (
	"encoding/binary"
	"io"
)

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

func NewTextFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodeText, Payload: data}
}

func NewBinaryFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodeBinary, Payload: data}
}

func NewPingFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodePing, Payload: data}
}

func NewPongFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodePong, Payload: data}
}

func NewCloseFrame(code uint16, reason string) Frame {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], code)
	copy(payload[2:], reason)
	return Frame{FIN: true, Opcode: OpcodeClose, Payload: payload}
}

func WriteFrame(w io.Writer, f Frame) error {
	header := make([]byte, 0, 14)

	b1 := byte(f.Opcode)
	if f.FIN {
		b1 |= 0x80
	}
	if f.RSV1 {
		b1 |= 0x40
	}
	if f.RSV2 {
		b1 |= 0x20
	}
	if f.RSV3 {
		b1 |= 0x10
	}
	header = append(header, b1)

	b2 := byte(0)
	if f.Masked {
		b2 |= 0x80
	}

	payloadLen := len(f.Payload)
	switch {
	case payloadLen <= 125:
		b2 |= byte(payloadLen)
		header = append(header, b2)
	case payloadLen <= 65535:
		b2 |= 126
		header = append(header, b2)
		lenBytes := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBytes, uint16(payloadLen))
		header = append(header, lenBytes...)
	default:
		b2 |= 127
		header = append(header, b2)
		lenBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(lenBytes, uint64(payloadLen))
		header = append(header, lenBytes...)
	}

	if f.Masked {
		header = append(header, f.MaskKey[:]...)
	}

	if _, err := w.Write(header); err != nil {
		return err
	}

	if payloadLen == 0 {
		return nil
	}

	if f.Masked {
		masked := applyMask(f.Payload, f.MaskKey)
		_, err := w.Write(masked)
		return err
	}

	_, err := w.Write(f.Payload)
	return err
}
```

- [ ] **Step 3: 运行 WriteFrame 测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestWriteFrame -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/frame/frame.go ws/frame/frame_test.go
git commit -m "feat(frame): add Frame struct and WriteFrame with all length modes"
```

---

### Task 5: 协议层 — ReadFrame 与分片处理

**Files:**
- Modify: `ws/frame/frame.go` — 添加 ReadFrame
- Modify: `ws/frame/frame_test.go` — 添加 ReadFrame 测试

- [ ] **Step 1: 写 ReadFrame 测试**

Append to `ws/frame/frame_test.go`:

```go
func TestReadFrame_TextUnmasked(t *testing.T) {
	raw := []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}
	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !f.FIN {
		t.Error("FIN = false, want true")
	}
	if f.Opcode != OpcodeText {
		t.Errorf("Opcode = %d, want %d", f.Opcode, OpcodeText)
	}
	if string(f.Payload) != "Hello" {
		t.Errorf("Payload = %q, want %q", string(f.Payload), "Hello")
	}
}

func TestReadFrame_Masked(t *testing.T) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	original := []byte("Hello")
	masked := applyMask(original, key)

	raw := []byte{0x81, 0x85}
	raw = append(raw, key[:]...)
	raw = append(raw, masked...)

	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Payload) != "Hello" {
		t.Errorf("Payload = %q, want %q", string(f.Payload), "Hello")
	}
}

func TestReadFrame_Ping(t *testing.T) {
	raw := []byte{0x89, 0x00}
	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != OpcodePing {
		t.Errorf("Opcode = %d, want Ping", f.Opcode)
	}
}

func TestReadFrame_16bitLength(t *testing.T) {
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	var buf bytes.Buffer
	WriteFrame(&buf, Frame{FIN: true, Opcode: OpcodeBinary, Payload: payload})

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Payload) != 200 {
		t.Errorf("Payload len = %d, want 200", len(f.Payload))
	}
}

func TestReadFrame_CloseWithStatus(t *testing.T) {
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, 1000)
	payload = append(payload, "normal"...)

	var buf bytes.Buffer
	WriteFrame(&buf, Frame{FIN: true, Opcode: OpcodeClose, Payload: payload})

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != OpcodeClose {
		t.Errorf("Opcode = %d, want Close", f.Opcode)
	}
	code := binary.BigEndian.Uint16(f.Payload[:2])
	if code != 1000 {
		t.Errorf("Close code = %d, want 1000", code)
	}
}

func TestReadFrame_Fragmentation(t *testing.T) {
	frag1 := Frame{FIN: false, Opcode: OpcodeText, Payload: []byte("Hel")}
	frag2 := Frame{FIN: false, Opcode: 0, Payload: []byte("lo ")}
	frag3 := Frame{FIN: true, Opcode: 0, Payload: []byte("World")}

	var buf bytes.Buffer
	WriteFrame(&buf, frag1)
	WriteFrame(&buf, frag2)
	WriteFrame(&buf, frag3)

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !f.FIN {
		t.Error("FIN = false, want true (reassembled)")
	}
	if f.Opcode != OpcodeText {
		t.Errorf("Opcode = %d, want Text", f.Opcode)
	}
	if string(f.Payload) != "Hello World" {
		t.Errorf("Payload = %q, want %q", string(f.Payload), "Hello World")
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -run TestReadFrame -v`
Expected: FAIL

- [ ] **Step 2: 实现 ReadFrame**

Append to `ws/frame/frame.go`:

```go
func ReadFrame(r io.Reader) (Frame, error) {
	var result Frame
	var header [2]byte

	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}

	result.FIN = header[0]&0x80 != 0
	result.RSV1 = header[0]&0x40 != 0
	result.RSV2 = header[0]&0x20 != 0
	result.RSV3 = header[0]&0x10 != 0
	result.Opcode = Opcode(header[0] & 0x0F)

	result.Masked = header[1]&0x80 != 0
	payloadLen := int(header[1] & 0x7F)

	switch payloadLen {
	case 126:
		var lenBytes [2]byte
		if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
			return Frame{}, err
		}
		payloadLen = int(binary.BigEndian.Uint16(lenBytes[:]))
	case 127:
		var lenBytes [8]byte
		if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
			return Frame{}, err
		}
		payloadLen = int(binary.BigEndian.Uint64(lenBytes[:]))
	}

	if result.Masked {
		if _, err := io.ReadFull(r, result.MaskKey[:]); err != nil {
			return Frame{}, err
		}
	}

	if payloadLen > 0 {
		result.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, result.Payload); err != nil {
			return Frame{}, err
		}
		if result.Masked {
			result.Payload = applyMask(result.Payload, result.MaskKey)
			result.Masked = false
		}
	}

	if !result.FIN {
		firstOpcode := result.Opcode
		accumulated := result.Payload

		for {
			next, err := ReadFrame(r)
			if err != nil {
				return Frame{}, err
			}
			accumulated = append(accumulated, next.Payload...)
			if next.FIN {
				result.FIN = true
				result.Opcode = firstOpcode
				result.Payload = accumulated
				break
			}
		}
	}

	return result, nil
}
```

- [ ] **Step 3: 运行全部 frame 测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -v`
Expected: ALL PASS

- [ ] **Step 4: Commit**

```bash
git add ws/frame/frame.go ws/frame/frame_test.go
git commit -m "feat(frame): add ReadFrame with fragmentation reassembly"
```

---

### Task 6: 连接层 — Conn 接口与 goroutineConn

**Files:**
- Create: `ws/internal/conn/conn.go`, `ws/internal/conn/conn_test.go`

- [ ] **Step 1: 写 goroutineConn 测试**

Create `ws/internal/conn/conn_test.go`:

```go
package conn

import (
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/frame"
)

func TestGoroutineConn_SendReceive(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	defer srvConn.Close()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	msg := Message{Type: frame.OpcodeText, Data: []byte("hello from client")}

	go func() {
		cliConn.WriteChan() <- msg
	}()

	select {
	case received := <-srvConn.ReadChan():
		if received.Type != frame.OpcodeText {
			t.Errorf("Type = %d, want %d", received.Type, frame.OpcodeText)
		}
		if string(received.Data) != "hello from client" {
			t.Errorf("Data = %q, want %q", string(received.Data), "hello from client")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestGoroutineConn_Bidirectional(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	defer srvConn.Close()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	cliConn.WriteChan() <- Message{Type: frame.OpcodeText, Data: []byte("ping")}

	select {
	case msg := <-srvConn.ReadChan():
		if string(msg.Data) != "ping" {
			t.Errorf("server got %q, want %q", string(msg.Data), "ping")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	srvConn.WriteChan() <- Message{Type: frame.OpcodeText, Data: []byte("pong")}

	select {
	case msg := <-cliConn.ReadChan():
		if string(msg.Data) != "pong" {
			t.Errorf("client got %q, want %q", string(msg.Data), "pong")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestGoroutineConn_AutoPong(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	defer srvConn.Close()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	cliConn.WriteChan() <- Message{Type: frame.OpcodePing, Data: []byte("heartbeat")}

	select {
	case msg := <-srvConn.ReadChan():
		if msg.Type != frame.OpcodePing {
			t.Errorf("got type %d, want Ping", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	select {
	case msg := <-cliConn.ReadChan():
		if msg.Type != frame.OpcodePong {
			t.Errorf("auto pong type = %d, want Pong", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for auto pong")
	}
}

func TestGoroutineConn_Close(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	id := srvConn.ID()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	if id == 0 {
		t.Error("ID should not be zero")
	}

	err := srvConn.Close()
	if err != nil {
		t.Fatal(err)
	}

	srvConn.Close()
}

func TestGoroutineConn_IDIncrement(t *testing.T) {
	client1, server1 := net.Pipe()
	client2, server2 := net.Pipe()

	conn1 := newGoroutineConn(server1, false)
	conn2 := newGoroutineConn(server2, false)

	if conn1.ID() >= conn2.ID() {
		t.Errorf("conn2.ID (%d) should be greater than conn1.ID (%d)", conn2.ID(), conn1.ID())
	}

	conn1.Close()
	conn2.Close()
	client1.Close()
	client2.Close()
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/internal/conn/ -v`
Expected: FAIL

- [ ] **Step 2: 实现 Conn 接口和 goroutineConn**

Create `ws/internal/conn/conn.go`:

```go
package conn

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/frame"
)

type Message struct {
	Type   frame.Opcode
	Data   []byte
	Status uint16
}

type Conn interface {
	ReadChan() <-chan Message
	WriteChan() chan<- Message
	Close() error
	RemoteAddr() net.Addr
	LocalAddr() net.Addr
	ID() uint64
}

var connIDSeq uint64

func nextConnID() uint64 {
	return atomic.AddUint64(&connIDSeq, 1)
}

const defaultChanSize = 256

type goroutineConn struct {
	id        uint64
	conn      net.Conn
	isClient  bool
	readChan  chan Message
	writeChan chan Message
	closeChan chan struct{}
	closeOnce sync.Once
}

func newGoroutineConn(nc net.Conn, isClient bool) *goroutineConn {
	c := &goroutineConn{
		id:        nextConnID(),
		conn:      nc,
		isClient:  isClient,
		readChan:  make(chan Message, defaultChanSize),
		writeChan: make(chan Message, defaultChanSize),
		closeChan: make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

func (c *goroutineConn) ReadChan() <-chan Message  { return c.readChan }
func (c *goroutineConn) WriteChan() chan<- Message  { return c.writeChan }
func (c *goroutineConn) ID() uint64                 { return c.id }
func (c *goroutineConn) RemoteAddr() net.Addr       { return c.conn.RemoteAddr() }
func (c *goroutineConn) LocalAddr() net.Addr        { return c.conn.LocalAddr() }

func (c *goroutineConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeChan)

		closeFrame := frame.NewCloseFrame(1000, "")
		closeFrame.Masked = c.isClient
		if c.isClient {
			closeFrame.MaskKey = frame.GenerateMaskKey()
		}
		frame.WriteFrame(c.conn, closeFrame)

		err = c.conn.Close()
	})
	return err
}

func (c *goroutineConn) readLoop() {
	defer close(c.readChan)
	for {
		select {
		case <-c.closeChan:
			return
		default:
		}

		f, err := frame.ReadFrame(c.conn)
		if err != nil {
			return
		}

		msg := Message{Type: f.Opcode, Data: f.Payload}
		if f.Opcode == frame.OpcodeClose && len(f.Payload) >= 2 {
			msg.Status = uint16(f.Payload[0])<<8 | uint16(f.Payload[1])
		}

		switch f.Opcode {
		case frame.OpcodePing:
			pong := frame.NewPongFrame(f.Payload)
			pong.Masked = c.isClient
			if c.isClient {
				pong.MaskKey = frame.GenerateMaskKey()
			}
			frame.WriteFrame(c.conn, pong)
			select {
			case c.readChan <- msg:
			case <-c.closeChan:
				return
			}

		case frame.OpcodeClose:
			closeFrame := frame.NewCloseFrame(msg.Status, "")
			closeFrame.Masked = c.isClient
			if c.isClient {
				closeFrame.MaskKey = frame.GenerateMaskKey()
			}
			frame.WriteFrame(c.conn, closeFrame)
			select {
			case c.readChan <- msg:
			case <-c.closeChan:
			}
			return

		default:
			select {
			case c.readChan <- msg:
			case <-c.closeChan:
				return
			}
		}
	}
}

func (c *goroutineConn) writeLoop() {
	for {
		select {
		case msg, ok := <-c.writeChan:
			if !ok {
				return
			}

			var f frame.Frame
			switch msg.Type {
			case frame.OpcodeText:
				f = frame.NewTextFrame(msg.Data)
			case frame.OpcodeBinary:
				f = frame.NewBinaryFrame(msg.Data)
			case frame.OpcodePing:
				f = frame.NewPingFrame(msg.Data)
			case frame.OpcodePong:
				f = frame.NewPongFrame(msg.Data)
			case frame.OpcodeClose:
				f = frame.NewCloseFrame(msg.Status, string(msg.Data))
			default:
				continue
			}

			f.Masked = c.isClient
			if c.isClient {
				f.MaskKey = frame.GenerateMaskKey()
			}

			if err := frame.WriteFrame(c.conn, f); err != nil {
				return
			}

		case <-c.closeChan:
			return
		}
	}
}
```

注意：需要将 `mask.go` 中的 `generateMaskKey` 导出为 `GenerateMaskKey`（大写开头），因为 `conn` 包需要调用它。

- [ ] **Step 3: 导出 generateMaskKey**

Modify `ws/frame/mask.go` — 将 `generateMaskKey` 重命名为 `GenerateMaskKey`，并更新 `mask_test.go` 中的调用。

- [ ] **Step 4: 运行测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ ./ws/internal/conn/ -v -timeout 10s`
Expected: ALL PASS

- [ ] **Step 5: Commit**

```bash
git add ws/internal/conn/conn.go ws/internal/conn/conn_test.go ws/frame/mask.go ws/frame/mask_test.go
git commit -m "feat(conn): add Conn interface and goroutineConn with read/write loops"
```

---

### Task 7: 连接层 — 握手协议

**Files:**
- Create: `ws/internal/conn/handshake.go`
- Modify: `ws/internal/conn/conn_test.go` — 添加握手测试

- [ ] **Step 1: 写握手测试**

Append to `ws/internal/conn/conn_test.go`（添加必要的 import: `net/http/httptest`, `strings`）:

```go
func TestServerHandshake(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := ServerHandshake(w, r)
		if err != nil {
			t.Errorf("ServerHandshake error: %v", err)
			return
		}
		defer c.Close()

		msg := <-c.ReadChan()
		if string(msg.Data) != "hello" {
			t.Errorf("got %q, want %q", string(msg.Data), "hello")
		}
		c.WriteChan() <- Message{Type: frame.OpcodeText, Data: []byte("world")}
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	c, err := ClientHandshake(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	c.WriteChan() <- Message{Type: frame.OpcodeText, Data: []byte("hello")}

	select {
	case msg := <-c.ReadChan():
		if string(msg.Data) != "world" {
			t.Errorf("got %q, want %q", string(msg.Data), "world")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestServerHandshake_InvalidRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()

	_, err := ServerHandshake(w, req)
	if err == nil {
		t.Error("expected error for invalid handshake")
	}
}
```

- [ ] **Step 2: 实现握手**

Create `ws/internal/conn/handshake.go`:

```go
package conn

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

var errInvalidHandshake = errors.New("invalid websocket handshake")

func ServerHandshake(w http.ResponseWriter, r *http.Request) (Conn, error) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return nil, errInvalidHandshake
	}

	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "Upgrade required", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}

	if !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "Connection upgrade required", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}

	secKey := r.Header.Get("Sec-WebSocket-Key")
	if secKey == "" {
		http.Error(w, "Missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}

	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "Unsupported WebSocket version", http.StatusBadRequest)
		return nil, errInvalidHandshake
	}

	acceptKey := computeAcceptKey(secKey)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "WebSocket not supported", http.StatusInternalServerError)
		return nil, errInvalidHandshake
	}

	netConn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n\r\n"

	if _, err := buf.WriteString(response); err != nil {
		netConn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		netConn.Close()
		return nil, err
	}

	return newGoroutineConn(netConn, false), nil
}

func ClientHandshake(rawURL string, headers http.Header) (Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	switch u.Scheme {
	case "ws":
		u.Scheme = "tcp"
	case "wss":
		u.Scheme = "tls"
	default:
		return nil, errors.New("invalid scheme: use ws:// or wss://")
	}

	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "tcp" {
			host += ":80"
		} else {
			host += ":443"
		}
	}

	netConn, err := net.Dial(u.Scheme, host)
	if err != nil {
		return nil, err
	}

	secKey := generateClientSecKey()
	acceptKey := computeAcceptKey(secKey)

	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + secKey + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n"

	if headers != nil {
		for k, vs := range headers {
			for _, v := range vs {
				req += k + ": " + v + "\r\n"
			}
		}
	}

	req += "\r\n"

	if _, err := netConn.Write([]byte(req)); err != nil {
		netConn.Close()
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(netConn), nil)
	if err != nil {
		netConn.Close()
		return nil, err
	}

	if resp.StatusCode != 101 {
		netConn.Close()
		return nil, errors.New("server returned status " + resp.Status)
	}

	if resp.Header.Get("Upgrade") != "websocket" {
		netConn.Close()
		return nil, errInvalidHandshake
	}

	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey {
		netConn.Close()
		return nil, errors.New("invalid Sec-WebSocket-Accept")
	}

	return newGoroutineConn(netConn, true), nil
}

func computeAcceptKey(secKey string) string {
	h := sha1.New()
	h.Write([]byte(secKey + websocketGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func generateClientSecKey() string {
	key := make([]byte, 16)
	rand.Read(key)
	return base64.StdEncoding.EncodeToString(key)
}
```

- [ ] **Step 3: 运行测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/internal/conn/ -v -timeout 10s`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/internal/conn/handshake.go ws/internal/conn/conn_test.go
git commit -m "feat(conn): add server and client WebSocket handshake per RFC 6455"
```

---

### Task 8: 会话层 — 状态管理与心跳

**Files:**
- Create: `ws/internal/session/session.go`, `ws/internal/session/heartbeat.go`, `ws/internal/session/session_test.go`

- [ ] **Step 1: 写心跳和状态测试**

Create `ws/internal/session/session_test.go`:

```go
package session

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/internal/conn"
)

type mockConn struct {
	readChan  chan conn.Message
	writeChan chan conn.Message
}

func newMockConn() *mockConn {
	return &mockConn{
		readChan:  make(chan conn.Message, 64),
		writeChan: make(chan conn.Message, 64),
	}
}

func (m *mockConn) ReadChan() <-chan conn.Message { return m.readChan }
func (m *mockConn) WriteChan() chan<- conn.Message { return m.writeChan }
func (m *mockConn) Close() error                   { return nil }
func (m *mockConn) RemoteAddr() net.Addr           { return nil }
func (m *mockConn) LocalAddr() net.Addr            { return nil }
func (m *mockConn) ID() uint64                     { return 1 }

func TestSession_StateTransitions(t *testing.T) {
	mc := newMockConn()
	s := NewSession(mc, SessionConfig{
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	if s.State() != StateDisconnected {
		t.Errorf("initial state = %d, want Disconnected", s.State())
	}

	s.SetState(StateConnecting)
	if s.State() != StateConnecting {
		t.Errorf("state = %d, want Connecting", s.State())
	}

	s.SetState(StateConnected)
	if s.State() != StateConnected {
		t.Errorf("state = %d, want Connected", s.State())
	}

	select {
	case st := <-s.StateChan():
		if st != StateConnected {
			t.Errorf("StateChan = %d, want Connected", st)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for state change")
	}
}

func TestHeartbeat_PingSent(t *testing.T) {
	mc := newMockConn()
	hb := NewPerConnHeartbeater(100*time.Millisecond, 5*time.Second)
	s := NewSession(mc, SessionConfig{
		PingInterval: 100 * time.Millisecond,
		PongTimeout:  5 * time.Second,
	})
	s.SetHeartbeater(hb)
	s.SetState(StateConnected)
	hb.Start(mc)

	select {
	case msg := <-mc.writeChan:
		if msg.Type != frame.OpcodePing {
			t.Errorf("got type %d, want Ping", msg.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for ping")
	}

	hb.Stop()
}

func TestReconnect_MaxRetries(t *testing.T) {
	mc := newMockConn()
	cfg := SessionConfig{
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		ReconnectInterval: 100 * time.Millisecond,
		MaxReconnect:      3,
	}
	s := NewSession(mc, cfg)
	s.SetState(StateConnected)
	s.SetState(StateDisconnected)

	rc := NewReconnector(cfg.ReconnectInterval, cfg.MaxReconnect, func() (conn.Conn, error) {
		return nil, errors.New("connection refused")
	})

	go rc.Start(s)

	select {
	case st := <-s.StateChan():
		if st != StateClosed {
			t.Errorf("state = %d, want Closed after max retries", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for reconnect exhaustion")
	}
}
```

- [ ] **Step 2: 实现 Session**

Create `ws/internal/session/session.go`:

```go
package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
)

type State int

const (
	StateDisconnected State = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

func (s State) String() string {
	switch s {
	case StateDisconnected:
		return "disconnected"
	case StateConnecting:
		return "connecting"
	case StateConnected:
		return "connected"
	case StateReconnecting:
		return "reconnecting"
	case StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

type SessionConfig struct {
	PingInterval      time.Duration
	PongTimeout       time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      int
}

type Session struct {
	conn        conn.Conn
	heartbeater Heartbeater
	config      SessionConfig

	stateChan chan State
	state     State

	readChan  <-chan conn.Message
	writeChan chan<- conn.Message
}

func NewSession(c conn.Conn, cfg SessionConfig) *Session {
	return &Session{
		conn:      c,
		config:    cfg,
		stateChan: make(chan State, 16),
		state:     StateDisconnected,
		readChan:  c.ReadChan(),
		writeChan: c.WriteChan(),
	}
}

func (s *Session) State() State            { return s.state }
func (s *Session) StateChan() <-chan State { return s.stateChan }
func (s *Session) ReadChan() <-chan conn.Message  { return s.readChan }
func (s *Session) WriteChan() chan<- conn.Message { return s.writeChan }
func (s *Session) Conn() conn.Conn         { return s.conn }

func (s *Session) SetState(st State) {
	s.state = st
	select {
	case s.stateChan <- st:
	default:
	}
}

func (s *Session) SetHeartbeater(hb Heartbeater) {
	s.heartbeater = hb
}

func (s *Session) Heartbeater() Heartbeater {
	return s.heartbeater
}

func (s *Session) Close() error {
	if s.heartbeater != nil {
		s.heartbeater.Stop()
	}
	s.SetState(StateClosed)
	return s.conn.Close()
}
```

Create `ws/internal/session/heartbeat.go`:

```go
package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/internal/conn"
)

type Heartbeater interface {
	Start(c conn.Conn)
	Stop()
	SetOnTimeout(fn func())
}

type perConnHeartbeater struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	stopChan     chan struct{}
	onTimeout    func()
}

func NewPerConnHeartbeater(pingInterval, pongTimeout time.Duration) *perConnHeartbeater {
	return &perConnHeartbeater{
		pingInterval: pingInterval,
		pongTimeout:  pongTimeout,
		stopChan:     make(chan struct{}),
	}
}

func (h *perConnHeartbeater) Start(c conn.Conn) {
	h.stopChan = make(chan struct{})
	go h.run(c)
}

func (h *perConnHeartbeater) Stop() {
	select {
	case h.stopChan <- struct{}{}:
	default:
	}
}

func (h *perConnHeartbeater) SetOnTimeout(fn func()) {
	h.onTimeout = fn
}

func (h *perConnHeartbeater) run(c conn.Conn) {
	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()

	lastPong := time.Now()

	for {
		select {
		case <-ticker.C:
			if time.Since(lastPong) > h.pongTimeout && h.onTimeout != nil {
				h.onTimeout()
				return
			}
			select {
			case c.WriteChan() <- conn.Message{Type: frame.OpcodePing, Data: []byte{}}:
			case <-h.stopChan:
				return
			}

		case msg, ok := <-c.ReadChan():
			if !ok {
				return
			}
			if msg.Type == frame.OpcodePong {
				lastPong = time.Now()
			}

		case <-h.stopChan:
			return
		}
	}
}
```

Create `ws/internal/session/reconnect.go`:

```go
package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
)

type reconnector struct {
	interval   time.Duration
	maxRetries int
	dial       func() (conn.Conn, error)
	stopChan   chan struct{}
}

func NewReconnector(interval time.Duration, maxRetries int, dial func() (conn.Conn, error)) *reconnector {
	return &reconnector{
		interval:   interval,
		maxRetries: maxRetries,
		dial:       dial,
		stopChan:   make(chan struct{}),
	}
}

func (r *reconnector) Start(s *Session) {
	s.SetState(StateReconnecting)

	for i := 0; i < r.maxRetries; i++ {
		select {
		case <-r.stopChan:
			return
		case <-time.After(r.interval):
		}

		s.SetState(StateConnecting)
		c, err := r.dial()
		if err != nil {
			s.SetState(StateReconnecting)
			continue
		}

		s.conn = c
		s.readChan = c.ReadChan()
		s.writeChan = c.WriteChan()
		s.SetState(StateConnected)
		return
	}

	s.SetState(StateClosed)
}

func (r *reconnector) Stop() {
	select {
	case r.stopChan <- struct{}{}:
	default:
	}
}
```

- [ ] **Step 3: 运行测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/internal/session/ -v -timeout 10s`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/internal/session/session.go ws/internal/session/heartbeat.go ws/internal/session/reconnect.go ws/internal/session/session_test.go
git commit -m "feat(session): add Session, perConnHeartbeater, and auto-reconnect"
```

---

### Task 9: Hub 连接管理中心

**Files:**
- Create: `ws/hub.go`, `ws/hub_test.go`

- [ ] **Step 1: 写 Hub 测试**

Create `ws/hub_test.go`:

```go
package ws

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type mockHubConn struct {
	readChan  chan conn.Message
	writeChan chan conn.Message
	id        uint64
}

func newMockHubConn(id uint64) *mockHubConn {
	return &mockHubConn{
		readChan:  make(chan conn.Message, 64),
		writeChan: make(chan conn.Message, 64),
		id:        id,
	}
}

func (m *mockHubConn) ReadChan() <-chan conn.Message { return m.readChan }
func (m *mockHubConn) WriteChan() chan<- conn.Message { return m.writeChan }
func (m *mockHubConn) Close() error { return nil }
func (m *mockHubConn) ID() uint64   { return m.id }

func newTestSession(id uint64) *session.Session {
	mc := newMockHubConn(id)
	return session.NewSession(mc, session.SessionConfig{})
}

func TestHub_RegisterAndCount(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s := newTestSession(1)
	h.Register(s)
	time.Sleep(50 * time.Millisecond)

	if count := h.Count(); count != 1 {
		t.Errorf("Count = %d, want 1", count)
	}
}

func TestHub_Unregister(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s := newTestSession(1)
	h.Register(s)
	time.Sleep(50 * time.Millisecond)

	h.Unregister(1)
	time.Sleep(50 * time.Millisecond)

	if count := h.Count(); count != 0 {
		t.Errorf("Count = %d, want 0", count)
	}
}

func TestHub_Get(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s := newTestSession(1)
	h.Register(s)
	time.Sleep(50 * time.Millisecond)

	got := h.Get(1)
	if got == nil {
		t.Error("Get(1) = nil, want session")
	}

	got = h.Get(999)
	if got != nil {
		t.Error("Get(999) should be nil")
	}
}

func TestHub_Broadcast(t *testing.T) {
	h := NewHub()
	go h.Run()
	defer h.Stop()

	s1 := newTestSession(1)
	s2 := newTestSession(2)
	h.Register(s1)
	h.Register(s2)
	time.Sleep(50 * time.Millisecond)

	msg := conn.Message{Type: 0x1, Data: []byte("broadcast")}
	h.Broadcast(msg)
	time.Sleep(50 * time.Millisecond)
}
```

- [ ] **Step 2: 实现 Hub**

Create `ws/hub.go`:

```go
package ws

import (
	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type Hub struct {
	conns      map[uint64]*session.Session
	register   chan *session.Session
	unregister chan uint64
	broadcast  chan conn.Message
	getReq     chan uint64
	getResp    chan *session.Session
	countReq   chan struct{}
	countResp  chan int
	stopChan   chan struct{}
}

func NewHub() *Hub {
	return &Hub{
		conns:      make(map[uint64]*session.Session),
		register:   make(chan *session.Session, 64),
		unregister: make(chan uint64, 64),
		broadcast:  make(chan conn.Message, 64),
		getReq:     make(chan uint64),
		getResp:    make(chan *session.Session, 1),
		countReq:   make(chan struct{}),
		countResp:  make(chan int, 1),
		stopChan:   make(chan struct{}),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case s := <-h.register:
			h.conns[s.Conn().ID()] = s

		case id := <-h.unregister:
			delete(h.conns, id)

		case msg := <-h.broadcast:
			for _, s := range h.conns {
				select {
				case s.WriteChan() <- msg:
				default:
				}
			}

		case id := <-h.getReq:
			s, ok := h.conns[id]
			if ok {
				h.getResp <- s
			} else {
				h.getResp <- nil
			}

		case <-h.countReq:
			h.countResp <- len(h.conns)

		case <-h.stopChan:
			return
		}
	}
}

func (h *Hub) Register(s *session.Session)  { h.register <- s }
func (h *Hub) Unregister(id uint64)         { h.unregister <- id }
func (h *Hub) Broadcast(msg conn.Message)   { h.broadcast <- msg }

func (h *Hub) Send(id uint64, msg conn.Message) {
	h.getReq <- id
	s := <-h.getResp
	if s != nil {
		s.WriteChan() <- msg
	}
}

func (h *Hub) Get(id uint64) *session.Session {
	h.getReq <- id
	return <-h.getResp
}

func (h *Hub) Count() int {
	h.countReq <- struct{}{}
	return <-h.countResp
}

func (h *Hub) Stop() {
	close(h.stopChan)
}
```

- [ ] **Step 3: 运行测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/ -run TestHub -v -timeout 10s`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/hub.go ws/hub_test.go
git commit -m "feat(ws): add Hub connection manager with channel-safe map access"
```

---

### Task 10: 类型导出

**Files:**
- Create: `ws/types.go`

- [ ] **Step 1: 创建 types.go**

Create `ws/types.go`:

```go
package ws

import (
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type Opcode = frame.Opcode
type Message = conn.Message
type State = session.State
type Conn = conn.Conn

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

- [ ] **Step 2: 验证编译**

Run: `cd /var/www/claude/ws/go-tools && go build ./ws/`
Expected: 编译通过

- [ ] **Step 3: Commit**

```bash
git add ws/types.go
git commit -m "feat(ws): re-export sub-package types for single-import usage"
```

---

### Task 11: 用户 API — Server

**Files:**
- Create: `ws/server.go`

- [ ] **Step 1: 实现 Server**

Create `ws/server.go`:

```go
package ws

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type ServerConfig struct {
	Addr              string
	PingInterval      time.Duration
	PongTimeout       time.Duration
	MaxConnections    int
	HandshakeTimeout  time.Duration
	ReadBufferSize    int
	WriteBufferSize   int
}

func defaultServerConfig(cfg ServerConfig) ServerConfig {
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 60 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.ReadBufferSize == 0 {
		cfg.ReadBufferSize = 4096
	}
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = 4096
	}
	return cfg
}

type Server struct {
	config   ServerConfig
	hub      *Hub
	listener net.Listener
	connChan chan *session.Session
	server   *http.Server
}

func NewServer(cfg ServerConfig) *Server {
	cfg = defaultServerConfig(cfg)
	return &Server{
		config:   cfg,
		hub:      NewHub(),
		connChan: make(chan *session.Session, 64),
	}
}

func (s *Server) Hub() *Hub                          { return s.hub }
func (s *Server) ConnChan() <-chan *session.Session   { return s.connChan }
func (s *Server) Listener() net.Listener              { return s.listener }

func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
			http.Error(w, "Too many connections", http.StatusServiceUnavailable)
			return
		}

		c, err := conn.ServerHandshake(w, r)
		if err != nil {
			return
		}

		sess := session.NewSession(c, session.SessionConfig{
			PingInterval: s.config.PingInterval,
			PongTimeout:  s.config.PongTimeout,
		})
		hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
		hb.SetOnTimeout(func() {
			sess.SetState(session.StateDisconnected)
			c.Close()
		})
		sess.SetHeartbeater(hb)
		sess.SetState(session.StateConnected)
		hb.Start(c)

		s.connChan <- sess
	})

	s.server = &http.Server{Handler: mux}

	var err error
	s.listener, err = net.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}

	return s.server.Serve(s.listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.Stop()
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}
```

- [ ] **Step 2: 验证编译**

Run: `cd /var/www/claude/ws/go-tools && go build ./ws/`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add ws/server.go
git commit -m "feat(ws): add Server with HTTP handler, connection limiting, and graceful shutdown"
```

---

### Task 12: 用户 API — Client

**Files:**
- Create: `ws/client.go`

- [ ] **Step 1: 实现 Client**

Create `ws/client.go`:

```go
package ws

import (
	"net/http"
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type ClientConfig struct {
	URL               string
	Headers           http.Header
	PingInterval      time.Duration
	PongTimeout       time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      int
}

func defaultClientConfig(cfg ClientConfig) ClientConfig {
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 60 * time.Second
	}
	if cfg.ReconnectInterval == 0 {
		cfg.ReconnectInterval = 5 * time.Second
	}
	if cfg.MaxReconnect == 0 {
		cfg.MaxReconnect = 5
	}
	return cfg
}

type Client struct {
	config  ClientConfig
	session *session.Session
	headers http.Header
}

func NewClient(cfg ClientConfig) *Client {
	cfg = defaultClientConfig(cfg)
	return &Client{
		config:  cfg,
		headers: cfg.Headers,
	}
}

func (c *Client) Connect() error {
	wc, err := conn.ClientHandshake(c.config.URL, c.headers)
	if err != nil {
		return err
	}

	sess := session.NewSession(wc, session.SessionConfig{
		PingInterval:      c.config.PingInterval,
		PongTimeout:       c.config.PongTimeout,
		ReconnectInterval: c.config.ReconnectInterval,
		MaxReconnect:      c.config.MaxReconnect,
	})

	hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		wc.Close()
		rc := session.NewReconnector(
			c.config.ReconnectInterval,
			c.config.MaxReconnect,
			func() (conn.Conn, error) {
				return conn.ClientHandshake(c.config.URL, c.headers)
			},
		)
		go rc.Start(sess)
	})
	sess.SetHeartbeater(hb)
	sess.SetState(session.StateConnected)
	hb.Start(wc)

	c.session = sess
	return nil
}

func (c *Client) ReadChan() <-chan Message {
	if c.session == nil {
		return nil
	}
	return c.session.ReadChan()
}

func (c *Client) WriteChan() chan<- Message {
	if c.session == nil {
		return nil
	}
	return c.session.WriteChan()
}

func (c *Client) StateChan() <-chan State {
	if c.session == nil {
		return nil
	}
	return c.session.StateChan()
}

func (c *Client) Send(msg Message) {
	if c.session != nil {
		c.session.WriteChan() <- msg
	}
}

func (c *Client) Close() error {
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}
```

- [ ] **Step 2: 验证编译**

Run: `cd /var/www/claude/ws/go-tools && go build ./ws/`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add ws/client.go
git commit -m "feat(ws): add Client with Connect, Send, auto-reconnect support"
```

---

### Task 13: 集成测试

**Files:**
- Create: `ws/ws_test.go`

- [ ] **Step 1: 写端到端集成测试**

Create `ws/ws_test.go`:

```go
package ws

import (
	"context"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/internal/session"
)

func TestIntegration_EchoServer(t *testing.T) {
	srv := NewServer(ServerConfig{
		Addr:         "127.0.0.1:0",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	go srv.Hub.Run()

	go func() {
		for s := range srv.ConnChan() {
			go func(sess *session.Session) {
				for msg := range sess.ReadChan() {
					sess.WriteChan() <- msg
				}
			}(s)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()

	time.Sleep(100 * time.Millisecond)

	addr := srv.Listener().Addr().String()
	client := NewClient(ClientConfig{
		URL:          "ws://" + addr + "/",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	if err := client.Connect(); err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	client.Send(Message{Type: OpcodeText, Data: []byte("echo test")})

	select {
	case msg := <-client.ReadChan():
		if string(msg.Data) != "echo test" {
			t.Errorf("got %q, want %q", string(msg.Data), "echo test")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}

	srv.Shutdown(context.Background())
}

func TestIntegration_Broadcast(t *testing.T) {
	srv := NewServer(ServerConfig{
		Addr:         "127.0.0.1:0",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	go srv.Hub.Run()

	go func() {
		for s := range srv.ConnChan() {
			srv.Hub.Register(s)
		}
	}()

	go srv.ListenAndServe()
	time.Sleep(100 * time.Millisecond)

	addr := srv.Listener().Addr().String()

	c1 := NewClient(ClientConfig{URL: "ws://" + addr + "/", PingInterval: 30 * time.Second, PongTimeout: 60 * time.Second})
	c1.Connect()
	defer c1.Close()

	c2 := NewClient(ClientConfig{URL: "ws://" + addr + "/", PingInterval: 30 * time.Second, PongTimeout: 60 * time.Second})
	c2.Connect()
	defer c2.Close()

	time.Sleep(100 * time.Millisecond)

	srv.Hub.Broadcast(Message{Type: OpcodeText, Data: []byte("broadcast msg")})
	time.Sleep(100 * time.Millisecond)

	for i, c := range []*Client{c1, c2} {
		select {
		case msg := <-c.ReadChan():
			if string(msg.Data) != "broadcast msg" {
				t.Errorf("client %d got %q, want %q", i, string(msg.Data), "broadcast msg")
			}
		case <-time.After(2 * time.Second):
			t.Errorf("client %d timeout", i)
		}
	}

	srv.Shutdown(context.Background())
}

func TestIntegration_MaxConnections(t *testing.T) {
	srv := NewServer(ServerConfig{
		Addr:           "127.0.0.1:0",
		PingInterval:   30 * time.Second,
		PongTimeout:    60 * time.Second,
		MaxConnections: 1,
	})

	go srv.Hub.Run()
	go srv.ListenAndServe()
	time.Sleep(100 * time.Millisecond)

	addr := srv.Listener().Addr().String()

	c1 := NewClient(ClientConfig{URL: "ws://" + addr + "/", PingInterval: 30 * time.Second, PongTimeout: 60 * time.Second})
	err := c1.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	time.Sleep(100 * time.Millisecond)

	c2 := NewClient(ClientConfig{URL: "ws://" + addr + "/", PingInterval: 30 * time.Second, PongTimeout: 60 * time.Second})
	err = c2.Connect()
	if err == nil {
		c2.Close()
		t.Error("second connection should be rejected when MaxConnections=1")
	}

	srv.Shutdown(context.Background())
}
```

- [ ] **Step 2: 运行全部测试**

Run: `cd /var/www/claude/ws/go-tools && go test ./... -v -timeout 30s`
Expected: ALL PASS

- [ ] **Step 3: Commit**

```bash
git add ws/ws_test.go
git commit -m "test(ws): add integration tests for echo, broadcast, and max connections"
```

---

### Task 14: 最终检查与修复

- [ ] **Step 1: 运行全部测试确认通过**

Run: `cd /var/www/claude/ws/go-tools && go test ./... -timeout 30s`
Expected: ALL PASS

- [ ] **Step 2: 运行 go vet 检查**

Run: `cd /var/www/claude/ws/go-tools && go vet ./...`
Expected: 无警告

- [ ] **Step 3: 修复所有编译/vet 问题后提交**

如有问题修复后：

```bash
git add -A
git commit -m "fix(ws): address vet warnings and final cleanup"
```

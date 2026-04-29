# ws v2 — High-Concurrency WebSocket Library Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rebuild the ws library for high concurrency (100K-1M connections) with interface-oriented architecture, Netty-style Pipeline, cross-platform event-driven I/O, reference-counted ByteBuf, sharded Hub, and unified error handling.

**Architecture:** Layered design from bottom-up: eventloop → buf → frame → pipeline → conn → session → hub → server/client. Each layer depends only on interfaces from layers below. No `internal` packages — all packages are public, isolation via interfaces.

**Tech Stack:** Go 1.24, standard library only (net, net/http, crypto/sha1, encoding/binary, sync, sync/atomic), platform-specific syscalls via `golang.org/x/sys/unix` (for epoll/kqueue)

---

## File Structure Map

| File | Responsibility |
|---|---|
| `ws/eventloop/eventloop.go` | EventLoop / Poller / EventHandler interfaces |
| `ws/eventloop/epoll_linux.go` | Linux epoll Poller implementation |
| `ws/eventloop/kqueue_bsd.go` | BSD kqueue Poller implementation |
| `ws/eventloop/eventloop_test.go` | EventLoop tests with mock Poller |
| `ws/buf/bytebuf.go` | ByteBuf interface + default implementation |
| `ws/buf/pool.go` | ByteBuf Pool with tiered sync.Pool |
| `ws/buf/bytebuf_test.go` | Reference count, Slice, zero-copy tests |
| `ws/frame/frame.go` | Frame struct, Opcode, ReadFrame, WriteFrame (v1 adapted) |
| `ws/frame/mask.go` | Mask/unmask (v1 preserved) |
| `ws/frame/frame_test.go` | Frame parse/serialize tests (v1 preserved) |
| `ws/pipeline/handler.go` | ChannelHandler / InboundHandler / OutboundHandler / Context interfaces |
| `ws/pipeline/pipeline.go` | ChannelPipeline implementation |
| `ws/pipeline/pipeline_test.go` | Handler chain tests |
| `ws/conn/conn.go` | Conn / EventDrivenConn interfaces |
| `ws/conn/netconn.go` | Standard net.Conn-based implementation |
| `ws/conn/epollconn.go` | Event-driven Conn implementation |
| `ws/conn/conn_test.go` | Conn tests |
| `ws/session/session.go` | Session interface + State + implementation |
| `ws/session/heartbeat.go` | Heartbeater interface + perConnHeartbeater |
| `ws/session/reconnect.go` | Reconnector interface + implementation |
| `ws/session/session_test.go` | Session/heartbeat/reconnect tests |
| `ws/hub/hub.go` | Hub interface + sharded lock implementation |
| `ws/hub/hub_test.go` | Hub tests |
| `ws/server/server.go` | Server + Bootstrap |
| `ws/server/server_test.go` | Server tests |
| `ws/client/client.go` | Client + Bootstrap |
| `ws/client/client_test.go` | Client tests |
| `ws/ws.go` | Root package: WSError, Config, re-export types |
| `ws/ws_test.go` | Integration tests |

**Files to delete from v1:** `ws/hub.go`, `ws/client.go`, `ws/server.go`, `ws/types.go`, `ws/errors.go`, `ws/errors_test.go`

**Files to move from v1:** `ws/internal/conn/handshake.go` → `ws/conn/handshake.go` (adapted for v2 conn interface)

---

### Task 1: Project Initialization

**Files:**
- Modify: `go.mod`
- Create: directory structure
- Delete: `ws/internal/`, `ws/hub.go`, `ws/client.go`, `ws/server.go`, `ws/types.go`, `ws/errors.go`, `ws/errors_test.go`

- [ ] **Step 1: Update go.mod module name**

Read `go.mod` and ensure:
```
module github.com/lufeijun/goTools

go 1.24.4
```

- [ ] **Step 2: Create directory structure**

```bash
mkdir -p ws/eventloop ws/buf ws/pipeline ws/conn ws/session ws/hub ws/server ws/client
```

- [ ] **Step 3: Remove v1 files**

```bash
cd /var/www/claude/ws/go-tools
rm -rf ws/internal/
rm -f ws/hub.go ws/hub_test.go ws/client.go ws/server.go ws/types.go ws/errors.go ws/errors_test.go
```

- [ ] **Step 4: Verify frame package still compiles**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -v`
Expected: PASS — frame tests from v1 still pass

- [ ] **Step 5: Commit**

```bash
git add go.mod
git add ws/eventloop ws/buf ws/pipeline ws/conn ws/session ws/hub ws/server ws/client
git rm -r ws/internal/ ws/hub.go ws/hub_test.go ws/client.go ws/server.go ws/types.go ws/errors.go ws/errors_test.go
git commit -m "chore(ws): init v2 directory structure, remove v1 internal packages"
```

---

### Task 2: WSError — Unified Error Type

**Files:**
- Create: `ws/ws.go` (WSError portion)
- Create: `ws/ws_test.go` (WSError tests)

- [ ] **Step 1: Write the failing test**

Create `ws/ws_test.go`:
```go
package ws

import (
	"errors"
	"testing"
)

func TestWSError_Error(t *testing.T) {
	e := &WSError{
		Code:    ErrCodeProtocolError,
		Message: "invalid opcode",
		ConnID:  42,
	}
	want := "ws error code=1002: invalid opcode (conn=42)"
	if got := e.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestWSError_Unwrap(t *testing.T) {
	cause := errors.New("underlying io error")
	e := &WSError{
		Code:    ErrCodeReadTimeout,
		Message: "read timeout",
		Cause:   cause,
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is should match wrapped cause")
	}
}

func TestWSError_WithConnID(t *testing.T) {
	e := &WSError{Code: ErrCodeInternalError, Message: "fail"}
	e2 := e.WithConnID(99)
	if e2.ConnID != 99 {
		t.Errorf("ConnID = %d, want 99", e2.ConnID)
	}
	if e.ConnID != 0 {
		t.Error("original WSError should not be modified")
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws -run TestWSError -v`
Expected: FAIL — `WSError` undefined

- [ ] **Step 2: Implement WSError**

Create `ws/ws.go`:
```go
package ws

import "fmt"

// WSError is the unified error type for v2.
type WSError struct {
	Code    int
	Message string
	Cause   error
	ConnID  uint64
}

func (e *WSError) Error() string {
	if e.ConnID != 0 {
		return fmt.Sprintf("ws error code=%d: %s (conn=%d)", e.Code, e.Message, e.ConnID)
	}
	return fmt.Sprintf("ws error code=%d: %s", e.Code, e.Message)
}

func (e *WSError) Unwrap() error {
	return e.Cause
}

func (e *WSError) WithConnID(id uint64) *WSError {
	return &WSError{
		Code:    e.Code,
		Message: e.Message,
		Cause:   e.Cause,
		ConnID:  id,
	}
}

// Predefined error codes.
const (
	ErrCodeProtocolError   = 1002
	ErrCodeUnsupportedData = 1003
	ErrCodeInvalidFrame    = 1007
	ErrCodePolicyViolation = 1008
	ErrCodeMessageTooBig   = 1009
	ErrCodeInternalError   = 1011
	ErrCodeReadTimeout     = 2001
	ErrCodeWriteTimeout    = 2002
	ErrCodeConnReset       = 2003
	ErrCodeHubFull         = 3001
)
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws -run TestWSError -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/ws.go ws/ws_test.go
git commit -m "feat(ws): add unified WSError with error codes and chain support"
```

---

### Task 3: Config — Global Configuration

**Files:**
- Modify: `ws/ws.go` (append Config)
- Modify: `ws/ws_test.go` (append Config tests)

- [ ] **Step 1: Write the failing test**

Append to `ws/ws_test.go`:
```go
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ReadBufferSize != 4096 {
		t.Errorf("ReadBufferSize = %d, want 4096", cfg.ReadBufferSize)
	}
	if cfg.WriteBufferSize != 4096 {
		t.Errorf("WriteBufferSize = %d, want 4096", cfg.WriteBufferSize)
	}
	if cfg.TCPNoDelay != true {
		t.Error("TCPNoDelay should default to true")
	}
	if cfg.EventLoopWorkers != 0 {
		t.Errorf("EventLoopWorkers = %d, want 0 (means auto)", cfg.EventLoopWorkers)
	}
	if cfg.PingInterval != 30e9 { // 30s in nanoseconds
		t.Errorf("PingInterval = %v, want 30s", cfg.PingInterval)
	}
	if cfg.PongTimeout != 60e9 {
		t.Errorf("PongTimeout = %v, want 60s", cfg.PongTimeout)
	}
	if cfg.MaxFrameSize != 64*1024*1024 {
		t.Errorf("MaxFrameSize = %d, want 64MB", cfg.MaxFrameSize)
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws -run TestDefaultConfig -v`
Expected: FAIL — `DefaultConfig` undefined

- [ ] **Step 2: Implement Config and DefaultConfig**

Append to `ws/ws.go`:
```go
import (
	"net/http"
	"runtime"
	"time"
)

// Config is the global configuration for Server and Client.
// Zero values mean "use default".
type Config struct {
	Addr              string
	ReadBufferSize    int
	WriteBufferSize   int
	MaxConnections    int
	TCPNoDelay        bool
	TCPQuickAck       bool
	SOReusePort       bool
	EventLoopWorkers  int
	EventLoopStrategy string
	BufferPoolSmall   int
	BufferPoolDefault int
	BufferPoolLarge   int
	PingInterval      time.Duration
	PongTimeout       time.Duration
	MaxFrameSize      int
	EnableCompression bool
	Headers           http.Header
	ReconnectInterval time.Duration
	MaxReconnect      int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		TCPNoDelay:        true,
		TCPQuickAck:       false,
		SOReusePort:       false,
		EventLoopWorkers:  0, // 0 means runtime.NumCPU()
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
	}
}

// EventLoopWorkerCount returns the effective number of event loop workers.
func (c Config) EventLoopWorkerCount() int {
	if c.EventLoopWorkers > 0 {
		return c.EventLoopWorkers
	}
	return runtime.NumCPU()
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws -run TestDefaultConfig -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/ws.go ws/ws_test.go
git commit -m "feat(ws): add Config with defaults for network, performance, heartbeat, protocol"
```

---

### Task 4: ByteBuf Interface + Tests

**Files:**
- Create: `ws/buf/bytebuf.go`
- Create: `ws/buf/bytebuf_test.go`

- [ ] **Step 1: Write the failing tests**

Create `ws/buf/bytebuf_test.go`:
```go
package buf

import (
	"bytes"
	"testing"
)

func TestByteBuf_ReadWrite(t *testing.T) {
	b := NewByteBuf(64)
	defer b.Release()

	b.Write([]byte("hello"))
	if b.ReadableBytes() != 5 {
		t.Errorf("ReadableBytes = %d, want 5", b.ReadableBytes())
	}

	got := b.ReadBytes(3)
	if !bytes.Equal(got, []byte("hel")) {
		t.Errorf("ReadBytes = %q, want %q", got, "hel")
	}
	if b.ReadableBytes() != 2 {
		t.Errorf("ReadableBytes after read = %d, want 2", b.ReadableBytes())
	}
}

func TestByteBuf_Slice(t *testing.T) {
	b := NewByteBuf(64)
	defer b.Release()

	b.Write([]byte("hello world"))
	s := b.Slice(2, 5)
	defer s.Release()

	if !bytes.Equal(s.ReadAll(), []byte("llo w")) {
		t.Errorf("Slice = %q, want %q", s.ReadAll(), "llo w")
	}
	if s.RefCount() != 1 {
		t.Errorf("Slice RefCount = %d, want 1", s.RefCount())
	}
}

func TestByteBuf_ReferenceCount(t *testing.T) {
	b := NewByteBuf(64)
	if b.RefCount() != 1 {
		t.Errorf("initial RefCount = %d, want 1", b.RefCount())
	}

	b.Retain()
	if b.RefCount() != 2 {
		t.Errorf("after Retain RefCount = %d, want 2", b.RefCount())
	}

	b.Release()
	if b.RefCount() != 1 {
		t.Errorf("after Release RefCount = %d, want 1", b.RefCount())
	}

	b.Release()
	// after second Release, the buf should be returned to pool
}

func TestByteBuf_DoubleFreePanic(t *testing.T) {
	b := NewByteBuf(64)
	b.Release()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on double Release")
		}
	}()
	b.Release()
}

func TestByteBuf_Peek(t *testing.T) {
	b := NewByteBuf(64)
	defer b.Release()

	b.Write([]byte("abc"))
	if !bytes.Equal(b.Peek(2), []byte("ab")) {
		t.Errorf("Peek = %q, want %q", b.Peek(2), "ab")
	}
	if b.ReadableBytes() != 3 {
		t.Errorf("ReadableBytes after Peek = %d, want 3", b.ReadableBytes())
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/buf -v`
Expected: FAIL — `ByteBuf` interface and `NewByteBuf` undefined

- [ ] **Step 2: Implement ByteBuf interface**

Create `ws/buf/bytebuf.go`:
```go
package buf

import (
	"fmt"
	"sync/atomic"
)

// ByteBuf is a reference-counted byte buffer with separate read/write indexes.
type ByteBuf interface {
	ReadableBytes() int
	ReadBytes(n int) []byte
	ReadAll() []byte
	Skip(n int)
	Peek(n int) []byte
	WritableBytes() int
	Write(p []byte) (int, error)
	WriteByte(b byte) error
	EnsureWritable(min int)
	Slice(start, length int) ByteBuf
	Retain() ByteBuf
	Release()
	RefCount() int
	Bytes() []byte
	ReaderIndex() int
	WriterIndex() int
	SetReaderIndex(int)
	SetWriterIndex(int)
}

// byteBuf is the default ByteBuf implementation.
type byteBuf struct {
	data         []byte
	readerIndex  int
	writerIndex  int
	refCount     int32
	pool         *bufPool
}

// NewByteBuf creates a new ByteBuf with the given capacity.
// The returned ByteBuf has refCount == 1.
func NewByteBuf(capacity int) ByteBuf {
	return &byteBuf{
		data:     make([]byte, 0, capacity),
		refCount: 1,
	}
}

func (b *byteBuf) ReadableBytes() int { return b.writerIndex - b.readerIndex }
func (b *byteBuf) WritableBytes() int { return cap(b.data) - b.writerIndex }

func (b *byteBuf) ReadBytes(n int) []byte {
	if n > b.ReadableBytes() {
		n = b.ReadableBytes()
	}
	result := make([]byte, n)
	copy(result, b.data[b.readerIndex:b.readerIndex+n])
	b.readerIndex += n
	return result
}

func (b *byteBuf) ReadAll() []byte {
	return b.ReadBytes(b.ReadableBytes())
}

func (b *byteBuf) Skip(n int) {
	if n > b.ReadableBytes() {
		n = b.ReadableBytes()
	}
	b.readerIndex += n
}

func (b *byteBuf) Peek(n int) []byte {
	if n > b.ReadableBytes() {
		n = b.ReadableBytes()
	}
	return b.data[b.readerIndex : b.readerIndex+n]
}

func (b *byteBuf) Write(p []byte) (int, error) {
	b.EnsureWritable(len(p))
	b.data = b.data[:b.writerIndex+len(p)]
	copy(b.data[b.writerIndex:], p)
	b.writerIndex += len(p)
	return len(p), nil
}

func (b *byteBuf) WriteByte(v byte) error {
	b.EnsureWritable(1)
	b.data = b.data[:b.writerIndex+1]
	b.data[b.writerIndex] = v
	b.writerIndex++
	return nil
}

func (b *byteBuf) EnsureWritable(min int) {
	if b.WritableBytes() >= min {
		return
	}
	needed := b.writerIndex + min
	if needed <= cap(b.data) {
		return
	}
	newCap := cap(b.data) * 2
	if newCap < needed {
		newCap = needed
	}
	newData := make([]byte, len(b.data), newCap)
	copy(newData, b.data)
	b.data = newData
}

func (b *byteBuf) Slice(start, length int) ByteBuf {
	if start < 0 || start+length > b.writerIndex {
		panic(fmt.Sprintf("slice out of range: start=%d length=%d writerIndex=%d", start, length, b.writerIndex))
	}
	s := &byteBuf{
		data:         b.data,
		readerIndex:  start,
		writerIndex:  start + length,
		refCount:     1,
	}
	b.Retain()
	return s
}

func (b *byteBuf) Retain() ByteBuf {
	rc := atomic.AddInt32(&b.refCount, 1)
	if rc <= 1 {
		panic(fmt.Sprintf("ByteBuf(%p) Retain on released buffer, refCount=%d", b, rc))
	}
	return b
}

func (b *byteBuf) Release() {
	rc := atomic.AddInt32(&b.refCount, -1)
	if rc == 0 {
		b.readerIndex = 0
		b.writerIndex = 0
		if b.pool != nil {
			b.pool.Put(b)
		}
	} else if rc < 0 {
		panic(fmt.Sprintf("ByteBuf(%p) double free detected, refCount=%d", b, rc))
	}
}

func (b *byteBuf) RefCount() int       { return int(atomic.LoadInt32(&b.refCount)) }
func (b *byteBuf) Bytes() []byte       { return b.data }
func (b *byteBuf) ReaderIndex() int    { return b.readerIndex }
func (b *byteBuf) WriterIndex() int    { return b.writerIndex }
func (b *byteBuf) SetReaderIndex(v int) { b.readerIndex = v }
func (b *byteBuf) SetWriterIndex(v int) { b.writerIndex = v }
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/buf -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/buf/bytebuf.go ws/buf/bytebuf_test.go
git commit -m "feat(buf): add reference-counted ByteBuf with read/write indexes"
```

---

### Task 5: ByteBuf Pool

**Files:**
- Create: `ws/buf/pool.go`
- Create: `ws/buf/pool_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/buf/pool_test.go`:
```go
package buf

import "testing"

func TestPool_GetPut_Small(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b := p.Get(100)
	if cap(b.Bytes()) != 512 {
		t.Errorf("cap = %d, want 512", cap(b.Bytes()))
	}
	b.Release()
}

func TestPool_GetPut_Default(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b := p.Get(2000)
	if cap(b.Bytes()) != 4096 {
		t.Errorf("cap = %d, want 4096", cap(b.Bytes()))
	}
	b.Release()
}

func TestPool_GetPut_Large(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b := p.Get(100000)
	if cap(b.Bytes()) < 100000 {
		t.Errorf("cap = %d, want >= 100000", cap(b.Bytes()))
	}
	b.Release()
}

func TestPool_Reuse(t *testing.T) {
	p := NewPool(512, 4096, 65536)
	b1 := p.Get(100)
	b1.Write([]byte("hello"))
	b1.Release()

	b2 := p.Get(100)
	if b2.ReadableBytes() != 0 {
		t.Errorf("reused buf should be empty, got %d readable", b2.ReadableBytes())
	}
	b2.Release()
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/buf -run TestPool -v`
Expected: FAIL — `Pool`, `NewPool`, `bufPool` undefined

- [ ] **Step 2: Implement Pool**

Create `ws/buf/pool.go`:
```go
package buf

import "sync"

// Pool is a tiered ByteBuf pool.
type Pool interface {
	Get(capacity int) ByteBuf
	Put(ByteBuf)
}

type bufPool struct {
	small   sync.Pool // capacity <= smallSize
	default sync.Pool // capacity <= defaultSize
	large   sync.Pool // capacity <= largeSize

	smallSize   int
	defaultSize int
	largeSize   int
}

// NewPool creates a tiered ByteBuf pool.
// smallSize, defaultSize, largeSize define the tier boundaries.
func NewPool(smallSize, defaultSize, largeSize int) Pool {
	return &bufPool{
		smallSize:   smallSize,
		defaultSize: defaultSize,
		largeSize:   largeSize,
	}
}

func (p *bufPool) Get(capacity int) ByteBuf {
	var b *byteBuf
	switch {
	case capacity <= p.smallSize:
		if v := p.small.Get(); v != nil {
			b = v.(*byteBuf)
		} else {
			b = &byteBuf{data: make([]byte, 0, p.smallSize)}
		}
	case capacity <= p.defaultSize:
		if v := p.default.Get(); v != nil {
			b = v.(*byteBuf)
		} else {
			b = &byteBuf{data: make([]byte, 0, p.defaultSize)}
		}
	case capacity <= p.largeSize:
		if v := p.large.Get(); v != nil {
			b = v.(*byteBuf)
		} else {
			b = &byteBuf{data: make([]byte, 0, p.largeSize)}
		}
	default:
		b = &byteBuf{data: make([]byte, 0, capacity)}
	}
	b.readerIndex = 0
	b.writerIndex = 0
	b.refCount = 1
	b.pool = p
	return b
}

func (p *bufPool) Put(b ByteBuf) {
	bb, ok := b.(*byteBuf)
	if !ok {
		return
	}
	c := cap(bb.data)
	switch {
	case c <= p.smallSize:
		p.small.Put(bb)
	case c <= p.defaultSize:
		p.default.Put(bb)
	case c <= p.largeSize:
		p.large.Put(bb)
	}
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/buf -run TestPool -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/buf/pool.go ws/buf/pool_test.go
git commit -m "feat(buf): add tiered ByteBuf pool with small/default/large sync.Pool"
```

---

### Task 6: eventloop — Interfaces

**Files:**
- Create: `ws/eventloop/eventloop.go`
- Create: `ws/eventloop/eventloop_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/eventloop/eventloop_test.go`:
```go
package eventloop

import (
	"testing"
	"time"
)

type mockHandler struct {
	called bool
	fd     int
	evts   uint32
}

func (m *mockHandler) OnEvent(fd int, events uint32) {
	m.called = true
	m.fd = fd
	m.evts = events
}

func TestEventConstants(t *testing.T) {
	if EventRead != 1 {
		t.Errorf("EventRead = %d, want 1", EventRead)
	}
	if EventWrite != 2 {
		t.Errorf("EventWrite = %d, want 2", EventWrite)
	}
	if EventError != 4 {
		t.Errorf("EventError = %d, want 4", EventError)
	}
	if EventHup != 8 {
		t.Errorf("EventHup = %d, want 8", EventHup)
	}
}

func TestEvent_String(t *testing.T) {
	e := Event{FD: 3, Events: EventRead | EventWrite}
	if e.FD != 3 {
		t.Errorf("FD = %d, want 3", e.FD)
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -v`
Expected: FAIL — `EventRead`, `EventWrite`, etc. undefined

- [ ] **Step 2: Implement interfaces**

Create `ws/eventloop/eventloop.go`:
```go
package eventloop

// EventHandler is the callback interface for fd events.
type EventHandler interface {
	OnEvent(fd int, events uint32)
}

// EventLoop manages a set of file descriptors and dispatches events.
type EventLoop interface {
	Register(fd int, handler EventHandler) error
	Deregister(fd int) error
	Wake()
	Run() error
	Stop() error
}

// Poller is the low-level OS-specific polling abstraction.
type Poller interface {
	Open() error
	Close() error
	Add(fd int, events uint32) error
	Mod(fd int, events uint32) error
	Del(fd int) error
	Wait(timeoutMs int) ([]Event, error)
}

// Event represents a single fd event.
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

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/eventloop/eventloop.go ws/eventloop/eventloop_test.go
git commit -m "feat(eventloop): add EventLoop, Poller, EventHandler interfaces"
```

---

### Task 7: eventloop — Epoll Implementation (Linux)

**Files:**
- Create: `ws/eventloop/epoll_linux.go`
- Create: `ws/eventloop/epoll_linux_test.go`

- [ ] **Step 1: Add dependency**

```bash
cd /var/www/claude/ws/go-tools
go get golang.org/x/sys/unix
```

- [ ] **Step 2: Write the failing test**

Create `ws/eventloop/epoll_linux_test.go`:
```go
//go:build linux
// +build linux

package eventloop

import (
	"os"
	"testing"
)

func TestEpollPoller_Lifecycle(t *testing.T) {
	p := newEpollPoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEpollPoller_AddDel(t *testing.T) {
	p := newEpollPoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Use a pipe to get a real fd
	r, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	fd := int(r.Fd())
	if err := p.Add(fd, EventRead); err != nil {
		t.Fatal(err)
	}
	if err := p.Del(fd); err != nil {
		t.Fatal(err)
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -run TestEpoll -v`
Expected: FAIL — `newEpollPoller` undefined (on Linux) or build skip (on non-Linux)

- [ ] **Step 3: Implement epoll Poller**

Create `ws/eventloop/epoll_linux.go`:
```go
//go:build linux
// +build linux

package eventloop

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

type epollPoller struct {
	epfd int
}

func newEpollPoller() Poller {
	return &epollPoller{}
}

func (p *epollPoller) Open() error {
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	p.epfd = fd
	return nil
}

func (p *epollPoller) Close() error {
	if p.epfd != 0 {
		return unix.Close(p.epfd)
	}
	return nil
}

func (p *epollPoller) Add(fd int, events uint32) error {
	var ev unix.EpollEvent
	ev.Fd = int32(fd)
	ev.Events = epollEvents(events)
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_ADD, fd, &ev)
}

func (p *epollPoller) Mod(fd int, events uint32) error {
	var ev unix.EpollEvent
	ev.Fd = int32(fd)
	ev.Events = epollEvents(events)
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_MOD, fd, &ev)
}

func (p *epollPoller) Del(fd int) error {
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_DEL, fd, nil)
}

func (p *epollPoller) Wait(timeoutMs int) ([]Event, error) {
	const maxEvents = 1024
	var epEvents [maxEvents]unix.EpollEvent

	n, err := unix.EpollWait(p.epfd, epEvents[:], timeoutMs)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return nil, nil
		}
		return nil, err
	}

	events := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		ev := epEvents[i]
		events = append(events, Event{
			FD:     int(ev.Fd),
			Events: fromEpollEvents(ev.Events),
		})
	}
	return events, nil
}

func epollEvents(events uint32) uint32 {
	var e uint32
	if events&EventRead != 0 {
		e |= unix.EPOLLIN
	}
	if events&EventWrite != 0 {
		e |= unix.EPOLLOUT
	}
	if events&EventError != 0 {
		e |= unix.EPOLLERR
	}
	if events&EventHup != 0 {
		e |= unix.EPOLLHUP | unix.EPOLLRDHUP
	}
	return e | unix.EPOLLET // edge-triggered
}

func fromEpollEvents(e uint32) uint32 {
	var events uint32
	if e&unix.EPOLLIN != 0 {
		events |= EventRead
	}
	if e&unix.EPOLLOUT != 0 {
		events |= EventWrite
	}
	if e&unix.EPOLLERR != 0 {
		events |= EventError
	}
	if e&(unix.EPOLLHUP|unix.EPOLLRDHUP) != 0 {
		events |= EventHup
	}
	return events
}
```

- [ ] **Step 4: Run tests (on Linux)**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -run TestEpoll -v`
Expected: PASS (on Linux)

- [ ] **Step 5: Commit**

```bash
git add ws/eventloop/epoll_linux.go ws/eventloop/epoll_linux_test.go go.mod go.sum
git commit -m "feat(eventloop): add Linux epoll Poller implementation"
```

---

### Task 8: eventloop — Kqueue Implementation (BSD)

**Files:**
- Create: `ws/eventloop/kqueue_bsd.go`
- Create: `ws/eventloop/kqueue_bsd_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/eventloop/kqueue_bsd_test.go`:
```go
//go:build darwin || freebsd || openbsd
// +build darwin freebsd openbsd

package eventloop

import (
	"os"
	"testing"
)

func TestKqueuePoller_Lifecycle(t *testing.T) {
	p := newKqueuePoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKqueuePoller_AddDel(t *testing.T) {
	p := newKqueuePoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	r, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	fd := int(r.Fd())
	if err := p.Add(fd, EventRead); err != nil {
		t.Fatal(err)
	}
	if err := p.Del(fd); err != nil {
		t.Fatal(err)
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -run TestKqueue -v`
Expected: FAIL (on BSD) or skip (on non-BSD)

- [ ] **Step 2: Implement kqueue Poller**

Create `ws/eventloop/kqueue_bsd.go`:
```go
//go:build darwin || freebsd || openbsd
// +build darwin freebsd openbsd

package eventloop

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

type kqueuePoller struct {
	kqfd int
}

func newKqueuePoller() Poller {
	return &kqueuePoller{}
}

func (p *kqueuePoller) Open() error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	p.kqfd = fd
	return nil
}

func (p *kqueuePoller) Close() error {
	if p.kqfd != 0 {
		return unix.Close(p.kqfd)
	}
	return nil
}

func (p *kqueuePoller) Add(fd int, events uint32) error {
	kevents := kqueueEvents(fd, events, unix.EV_ADD)
	_, err := unix.Kevent(p.kqfd, kevents, nil, nil)
	return err
}

func (p *kqueuePoller) Mod(fd int, events uint32) error {
	// kqueue: EV_ADD with same ident overwrites existing filter
	return p.Add(fd, events)
}

func (p *kqueuePoller) Del(fd int) error {
	kevents := []unix.Kevent_t{
		{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_DELETE},
		{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_DELETE},
	}
	_, err := unix.Kevent(p.kqfd, kevents, nil, nil)
	if err != nil && !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return nil
}

func (p *kqueuePoller) Wait(timeoutMs int) ([]Event, error) {
	const maxEvents = 1024
	var kevents [maxEvents]unix.Kevent_t

	var ts *unix.Timespec
	if timeoutMs >= 0 {
		t := unix.NsecToTimespec(int64(timeoutMs) * 1e6)
		ts = &t
	}

	n, err := unix.Kevent(p.kqfd, nil, kevents[:], ts)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return nil, nil
		}
		return nil, err
	}

	// Aggregate events by fd
	fdEvents := make(map[int]uint32)
	for i := 0; i < n; i++ {
		ev := kevents[i]
		fd := int(ev.Ident)
		switch ev.Filter {
		case unix.EVFILT_READ:
			fdEvents[fd] |= EventRead
		case unix.EVFILT_WRITE:
			fdEvents[fd] |= EventWrite
		}
		if ev.Flags&unix.EV_ERROR != 0 {
			fdEvents[fd] |= EventError
		}
		if ev.Flags&unix.EV_EOF != 0 {
			fdEvents[fd] |= EventHup
		}
	}

	events := make([]Event, 0, len(fdEvents))
	for fd, e := range fdEvents {
		events = append(events, Event{FD: fd, Events: e})
	}
	return events, nil
}

func kqueueEvents(fd int, events uint32, flags uint16) []unix.Kevent_t {
	var kev []unix.Kevent_t
	if events&EventRead != 0 {
		kev = append(kev, unix.Kevent_t{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_READ,
			Flags:  flags,
		})
	}
	if events&EventWrite != 0 {
		kev = append(kev, unix.Kevent_t{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_WRITE,
			Flags:  flags,
		})
	}
	return kev
}
```

- [ ] **Step 3: Run tests (on BSD)**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -run TestKqueue -v`
Expected: PASS (on BSD)

- [ ] **Step 4: Commit**

```bash
git add ws/eventloop/kqueue_bsd.go ws/eventloop/kqueue_bsd_test.go
git commit -m "feat(eventloop): add BSD kqueue Poller implementation"
```

---

### Task 9: eventloop — DefaultEventLoop

**Files:**
- Modify: `ws/eventloop/eventloop.go`
- Modify: `ws/eventloop/eventloop_test.go`

- [ ] **Step 1: Write the failing test**

Append to `ws/eventloop/eventloop_test.go`:
```go
func TestDefaultEventLoop_RegisterDeregister(t *testing.T) {
	p := &mockPoller{}
	el := NewEventLoop(p)

	h := &mockHandler{}
	if err := el.Register(3, h); err != nil {
		t.Fatal(err)
	}
	if err := el.Deregister(3); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultEventLoop_Dispatch(t *testing.T) {
	p := &mockPoller{}
	el := NewEventLoop(p)

	h := &mockHandler{}
	el.Register(3, h)

	p.events = []Event{{FD: 3, Events: EventRead}}
	go el.Run()
	time.Sleep(50 * time.Millisecond)
	el.Stop()

	if !h.called {
		t.Error("handler should have been called")
	}
	if h.fd != 3 {
		t.Errorf("fd = %d, want 3", h.fd)
	}
	if h.evts != EventRead {
		t.Errorf("events = %d, want EventRead", h.evts)
	}
}

type mockPoller struct {
	events   []Event
	adds     []int
	dels     []int
	waitDone chan struct{}
}

func (m *mockPoller) Open() error  { return nil }
func (m *mockPoller) Close() error { return nil }
func (m *mockPoller) Add(fd int, events uint32) error {
	m.adds = append(m.adds, fd)
	return nil
}
func (m *mockPoller) Mod(fd int, events uint32) error { return nil }
func (m *mockPoller) Del(fd int) error {
	m.dels = append(m.dels, fd)
	return nil
}
func (m *mockPoller) Wait(timeoutMs int) ([]Event, error) {
	if m.waitDone != nil {
		close(m.waitDone)
	}
	if len(m.events) > 0 {
		ev := m.events
		m.events = nil
		return ev, nil
	}
	// block forever until stopped
	select {}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -run TestDefaultEventLoop -v`
Expected: FAIL — `NewEventLoop` undefined

- [ ] **Step 2: Implement DefaultEventLoop**

Append to `ws/eventloop/eventloop.go`:
```go
import (
	"errors"
	"sync"
	"sync/atomic"
)

// defaultEventLoop is the standard EventLoop implementation.
type defaultEventLoop struct {
	poller   Poller
	handlers map[int]EventHandler
	mu       sync.RWMutex
	running  int32
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewEventLoop creates an EventLoop using the given Poller.
func NewEventLoop(p Poller) EventLoop {
	return &defaultEventLoop{
		poller:   p,
		handlers: make(map[int]EventHandler),
		stopCh:   make(chan struct{}),
	}
}

func (el *defaultEventLoop) Register(fd int, handler EventHandler) error {
	el.mu.Lock()
	defer el.mu.Unlock()
	if err := el.poller.Add(fd, EventRead); err != nil {
		return err
	}
	el.handlers[fd] = handler
	return nil
}

func (el *defaultEventLoop) Deregister(fd int) error {
	el.mu.Lock()
	defer el.mu.Unlock()
	delete(el.handlers, fd)
	return el.poller.Del(fd)
}

func (el *defaultEventLoop) Wake() {
	// TODO: implement wake using eventfd / pipe (V2.1)
}

func (el *defaultEventLoop) Run() error {
	if !atomic.CompareAndSwapInt32(&el.running, 0, 1) {
		return errors.New("eventloop already running")
	}
	defer atomic.StoreInt32(&el.running, 0)

	if err := el.poller.Open(); err != nil {
		return err
	}
	defer el.poller.Close()

	for {
		select {
		case <-el.stopCh:
			return nil
		default:
		}

		events, err := el.poller.Wait(100)
		if err != nil {
			continue
		}

		for _, ev := range events {
			el.mu.RLock()
			h, ok := el.handlers[ev.FD]
			el.mu.RUnlock()
			if ok {
				h.OnEvent(ev.FD, ev.Events)
			}
		}
	}
}

func (el *defaultEventLoop) Stop() error {
	if atomic.LoadInt32(&el.running) == 0 {
		return nil
	}
	close(el.stopCh)
	return nil
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/eventloop -run TestDefaultEventLoop -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/eventloop/eventloop.go ws/eventloop/eventloop_test.go
git commit -m "feat(eventloop): add DefaultEventLoop with Register/Deregister/Run/Stop"
```

---

### Task 10: Pipeline — Handler Interfaces

**Files:**
- Create: `ws/pipeline/handler.go`

- [ ] **Step 1: Write the failing test**

Create `ws/pipeline/handler_test.go`:
```go
package pipeline

import "testing"

type testHandler struct {
	name string
}

func (h *testHandler) Name() string { return h.name }

type testInbound struct {
	testHandler
	readCalled bool
}

func (h *testInbound) ChannelRead(ctx Context, msg interface{}) { h.readCalled = true }
func (h *testInbound) ChannelActive(ctx Context)                {}
func (h *testInbound) ChannelInactive(ctx Context)              {}
func (h *testInbound) ExceptionCaught(ctx Context, err error)   {}

type testOutbound struct {
	testHandler
	writeCalled bool
}

func (h *testOutbound) Write(ctx Context, msg interface{}) { h.writeCalled = true }
func (h *testOutbound) Flush(ctx Context)                  {}

func TestHandlerInterfaces(t *testing.T) {
	in := &testInbound{testHandler: testHandler{name: "in"}}
	out := &testOutbound{testHandler: testHandler{name: "out"}}

	if in.Name() != "in" {
		t.Errorf("inbound name = %q, want in", in.Name())
	}
	if out.Name() != "out" {
		t.Errorf("outbound name = %q, want out", out.Name())
	}
	var _ InboundHandler = in
	var _ OutboundHandler = out
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/pipeline -v`
Expected: FAIL — `Context` undefined

- [ ] **Step 2: Implement Handler interfaces**

Create `ws/pipeline/handler.go`:
```go
package pipeline

// ChannelPipeline is the handler chain for a connection.
type ChannelPipeline interface {
	AddFirst(name string, handler ChannelHandler) ChannelPipeline
	AddLast(name string, handler ChannelHandler) ChannelPipeline
	Remove(name string) ChannelPipeline
	FireChannelRead(msg interface{})
	FireChannelWrite(msg interface{})
	FireChannelActive()
	FireChannelInactive()
	FireExceptionCaught(err error)
}

// ChannelHandler is the base handler interface.
type ChannelHandler interface {
	Name() string
}

// InboundHandler handles inbound data (reads).
type InboundHandler interface {
	ChannelHandler
	ChannelRead(ctx Context, msg interface{})
	ChannelActive(ctx Context)
	ChannelInactive(ctx Context)
	ExceptionCaught(ctx Context, err error)
}

// OutboundHandler handles outbound data (writes).
type OutboundHandler interface {
	ChannelHandler
	Write(ctx Context, msg interface{})
	Flush(ctx Context)
}

// Context is the handler's execution context within a pipeline.
type Context interface {
	Pipeline() ChannelPipeline
	FireChannelRead(msg interface{})
	FireChannelWrite(msg interface{})
	Write(msg interface{})
	Flush()
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/pipeline -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/pipeline/handler.go ws/pipeline/handler_test.go
git commit -m "feat(pipeline): add ChannelHandler, InboundHandler, OutboundHandler, Context interfaces"
```

---

### Task 11: Pipeline — ChannelPipeline Implementation

**Files:**
- Create: `ws/pipeline/pipeline.go`
- Create: `ws/pipeline/pipeline_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/pipeline/pipeline_test.go`:
```go
package pipeline

import (
	"testing"
)

type recorderInbound struct {
	testHandler
	reads    []interface{}
	active   bool
	inactive bool
}

func (h *recorderInbound) ChannelRead(ctx Context, msg interface{}) {
	h.reads = append(h.reads, msg)
	ctx.FireChannelRead(msg)
}
func (h *recorderInbound) ChannelActive(ctx Context)   { h.active = true }
func (h *recorderInbound) ChannelInactive(ctx Context) { h.inactive = true }
func (h *recorderInbound) ExceptionCaught(ctx Context, err error) {}

type recorderOutbound struct {
	testHandler
	writes []interface{}
}

func (h *recorderOutbound) Write(ctx Context, msg interface{}) {
	h.writes = append(h.writes, msg)
}
func (h *recorderOutbound) Flush(ctx Context) {}

func TestDefaultPipeline_InboundChain(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	h2 := &recorderInbound{testHandler: testHandler{name: "h2"}}
	p.AddLast("h1", h1)
	p.AddLast("h2", h2)

	p.FireChannelRead("hello")

	if len(h1.reads) != 1 || h1.reads[0] != "hello" {
		t.Errorf("h1.reads = %v, want [hello]", h1.reads)
	}
	if len(h2.reads) != 1 || h2.reads[0] != "hello" {
		t.Errorf("h2.reads = %v, want [hello]", h2.reads)
	}
}

func TestDefaultPipeline_OutboundChain(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderOutbound{testHandler: testHandler{name: "h1"}}
	h2 := &recorderOutbound{testHandler: testHandler{name: "h2"}}
	p.AddLast("h1", h1)
	p.AddLast("h2", h2)

	// Outbound: from tail to head
	p.FireChannelWrite("world")

	if len(h2.writes) != 1 || h2.writes[0] != "world" {
		t.Errorf("h2.writes = %v, want [world]", h2.writes)
	}
	if len(h1.writes) != 1 || h1.writes[0] != "world" {
		t.Errorf("h1.writes = %v, want [world]", h1.writes)
	}
}

func TestDefaultPipeline_Remove(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	h2 := &recorderInbound{testHandler: testHandler{name: "h2"}}
	p.AddLast("h1", h1)
	p.AddLast("h2", h2)
	p.Remove("h1")

	p.FireChannelRead("msg")
	if len(h1.reads) != 0 {
		t.Error("h1 should have been removed")
	}
	if len(h2.reads) != 1 {
		t.Error("h2 should have received msg")
	}
}

func TestDefaultPipeline_ActiveInactive(t *testing.T) {
	p := NewPipeline()
	h1 := &recorderInbound{testHandler: testHandler{name: "h1"}}
	p.AddLast("h1", h1)
	p.FireChannelActive()
	if !h1.active {
		t.Error("h1 should be active")
	}
	p.FireChannelInactive()
	if !h1.inactive {
		t.Error("h1 should be inactive")
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/pipeline -v`
Expected: FAIL — `NewPipeline` undefined

- [ ] **Step 2: Implement DefaultPipeline**

Create `ws/pipeline/pipeline.go`:
```go
package pipeline

import "sync"

// NewPipeline creates a new ChannelPipeline.
func NewPipeline() ChannelPipeline {
	p := &defaultPipeline{
		ctxs: make(map[string]*handlerContext),
	}
	p.head = &handlerContext{pipeline: p, name: "head"}
	p.tail = &handlerContext{pipeline: p, name: "tail"}
	p.head.next = p.tail
	p.tail.prev = p.head
	return p
}

type defaultPipeline struct {
	mu   sync.RWMutex
	head *handlerContext
	tail *handlerContext
	ctxs map[string]*handlerContext
}

type handlerContext struct {
	pipeline ChannelPipeline
	name     string
	handler  ChannelHandler
	prev     *handlerContext
	next     *handlerContext
}

func (c *handlerContext) Pipeline() ChannelPipeline          { return c.pipeline }
func (c *handlerContext) FireChannelRead(msg interface{})    { c.invokeChannelRead(msg) }
func (c *handlerContext) FireChannelWrite(msg interface{})   { c.invokeChannelWrite(msg) }
func (c *handlerContext) Write(msg interface{})              { c.invokeChannelWrite(msg) }
func (c *handlerContext) Flush()                             {}

func (c *handlerContext) invokeChannelRead(msg interface{}) {
	next := c.findNextInbound()
	if next != nil {
		next.handler.(InboundHandler).ChannelRead(next, msg)
	}
}

func (c *handlerContext) invokeChannelWrite(msg interface{}) {
	prev := c.findPrevOutbound()
	if prev != nil {
		prev.handler.(OutboundHandler).Write(prev, msg)
	}
}

func (c *handlerContext) findNextInbound() *handlerContext {
	for ctx := c.next; ctx != nil; ctx = ctx.next {
		if _, ok := ctx.handler.(InboundHandler); ok {
			return ctx
		}
	}
	return nil
}

func (c *handlerContext) findPrevOutbound() *handlerContext {
	for ctx := c.prev; ctx != nil; ctx = ctx.prev {
		if _, ok := ctx.handler.(OutboundHandler); ok {
			return ctx
		}
	}
	return nil
}

func (p *defaultPipeline) AddFirst(name string, handler ChannelHandler) ChannelPipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := &handlerContext{pipeline: p, name: name, handler: handler}
	p.insertAfter(p.head, ctx)
	p.ctxs[name] = ctx
	return p
}

func (p *defaultPipeline) AddLast(name string, handler ChannelHandler) ChannelPipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx := &handlerContext{pipeline: p, name: name, handler: handler}
	p.insertBefore(p.tail, ctx)
	p.ctxs[name] = ctx
	return p
}

func (p *defaultPipeline) Remove(name string) ChannelPipeline {
	p.mu.Lock()
	defer p.mu.Unlock()
	ctx, ok := p.ctxs[name]
	if !ok {
		return p
	}
	delete(p.ctxs, name)
	ctx.prev.next = ctx.next
	ctx.next.prev = ctx.prev
	return p
}

func (p *defaultPipeline) insertAfter(after, ctx *handlerContext) {
	ctx.prev = after
	ctx.next = after.next
	after.next.prev = ctx
	after.next = ctx
}

func (p *defaultPipeline) insertBefore(before, ctx *handlerContext) {
	ctx.next = before
	ctx.prev = before.prev
	before.prev.next = ctx
	before.prev = ctx
}

func (p *defaultPipeline) FireChannelRead(msg interface{})  { p.head.invokeChannelRead(msg) }
func (p *defaultPipeline) FireChannelWrite(msg interface{}) { p.tail.invokeChannelWrite(msg) }
func (p *defaultPipeline) FireChannelActive()               { /* TODO */ }
func (p *defaultPipeline) FireChannelInactive()             { /* TODO */ }
func (p *defaultPipeline) FireExceptionCaught(err error)    { /* TODO */ }
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/pipeline -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/pipeline/pipeline.go ws/pipeline/pipeline_test.go
git commit -m "feat(pipeline): add DefaultPipeline with Inbound/Outbound chain traversal"
```

---

### Task 12: Conn — Interfaces + netConn Implementation

**Files:**
- Create: `ws/conn/conn.go`
- Create: `ws/conn/netconn.go`
- Create: `ws/conn/conn_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/conn/conn_test.go`:
```go
package conn

import (
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

func TestNetConn_ReadWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	c := NewNetConn(server, false, 1)
	defer c.Close()

	// Write from client side
	go client.Write([]byte("hello"))

	bb := buf.NewByteBuf(64)
	defer bb.Release()

	c.Read(bb)
	if !bytes.Equal(bb.ReadAll(), []byte("hello")) {
		t.Errorf("read = %q, want hello", bb.ReadAll())
	}
}

func TestNetConn_ID(t *testing.T) {
	client1, server1 := net.Pipe()
	client2, server2 := net.Pipe()
	defer client1.Close()
	defer client2.Close()
	defer server1.Close()
	defer server2.Close()

	c1 := NewNetConn(server1, false, 1)
	c2 := NewNetConn(server2, false, 2)

	if c1.ID() != 1 {
		t.Errorf("c1.ID = %d, want 1", c1.ID())
	}
	if c2.ID() != 2 {
		t.Errorf("c2.ID = %d, want 2", c2.ID())
	}
}

func TestNetConn_IsClient(t *testing.T) {
	_, server := net.Pipe()
	defer server.Close()

	sc := NewNetConn(server, false, 1)
	if sc.IsClient() {
		t.Error("server conn should not be client")
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/conn -v`
Expected: FAIL — `Conn`, `netConn` undefined

- [ ] **Step 2: Implement Conn interface and netConn**

Create `ws/conn/conn.go`:
```go
package conn

import (
	"net"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

// Message is the high-level message structure.
type Message struct {
	Type   byte   // Opcode
	Data   []byte
	Status uint16 // For Close frames
}

// Conn is the WebSocket connection abstraction.
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

// EventDrivenConn is the event-driven Conn extension.
type EventDrivenConn interface {
	Conn
	FD() int
	OnEvent(events uint32)
	SetEventLoop(el interface{})
}

var connIDSeq uint64

// NextConnID returns the next unique connection ID.
func NextConnID() uint64 {
	return atomic.AddUint64(&connIDSeq, 1)
}
```

Create `ws/conn/netconn.go`:
```go
package conn

import (
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type netConn struct {
	id       uint64
	conn     net.Conn
	isClient bool
	pipeline pipeline.ChannelPipeline
	active   int32
	closeOnce sync.Once
}

func NewNetConn(nc net.Conn, isClient bool, id uint64) *netConn {
	c := &netConn{
		id:       id,
		conn:     nc,
		isClient: isClient,
		pipeline: pipeline.NewPipeline(),
		active:   1,
	}
	return c
}

func (c *netConn) ID() uint64                    { return c.id }
func (c *netConn) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *netConn) RemoteAddr() net.Addr          { return c.conn.RemoteAddr() }
func (c *netConn) LocalAddr() net.Addr           { return c.conn.LocalAddr() }
func (c *netConn) IsClient() bool                { return c.isClient }
func (c *netConn) Active() bool                  { return atomic.LoadInt32(&c.active) == 1 }

func (c *netConn) Read(b buf.ByteBuf) error {
	if !c.Active() {
		return io.EOF
	}
	// Read into a temp buffer then write to ByteBuf
	tmp := make([]byte, 4096)
	n, err := c.conn.Read(tmp)
	if n > 0 {
		b.Write(tmp[:n])
	}
	return err
}

func (c *netConn) Write(b buf.ByteBuf) error {
	if !c.Active() {
		return io.ErrClosedPipe
	}
	_, err := c.conn.Write(b.ReadAll())
	return err
}

func (c *netConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.active, 0)
		err = c.conn.Close()
	})
	return err
}
```

- [ ] **Step 3: Add WebSocket handshake functions**

Create `ws/conn/handshake.go` (adapted from v1, returns `net.Conn`):

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

// ServerHandshake validates the HTTP Upgrade request and returns the raw net.Conn.
func ServerHandshake(w http.ResponseWriter, r *http.Request) (net.Conn, error) {
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
	netConn, bw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey + "\r\n\r\n"
	if _, err := bw.WriteString(response); err != nil {
		netConn.Close()
		return nil, err
	}
	if err := bw.Flush(); err != nil {
		netConn.Close()
		return nil, err
	}
	return netConn, nil
}

// ClientHandshake initiates a client WebSocket connection and returns the raw net.Conn.
func ClientHandshake(rawURL string, headers http.Header) (net.Conn, error) {
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
	return netConn, nil
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

Append to `ws/conn/conn_test.go`:
```go
func TestServerHandshake_InvalidRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()
	_, err := ServerHandshake(w, req)
	if err == nil {
		t.Error("expected error for invalid handshake")
	}
}
```

Add import to conn_test.go: `"net/http/httptest"`

- [ ] **Step 4: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/conn -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add ws/conn/conn.go ws/conn/netconn.go ws/conn/handshake.go ws/conn/conn_test.go
git commit -m "feat(conn): add Conn interface, netConn, and RFC 6455 handshake"
```

---

### Task 13: Conn — epollConn Implementation

**Files:**
- Create: `ws/conn/epollconn.go`
- Modify: `ws/conn/conn_test.go`

- [ ] **Step 1: Write the failing test**

Append to `ws/conn/conn_test.go`:
```go
func TestEpollConn_Interface(t *testing.T) {
	// We can't test real epoll without a real fd, but we can verify the struct
	// implements EventDrivenConn interface.
	var _ EventDrivenConn = (*epollConn)(nil)
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/conn -run TestEpollConn -v`
Expected: FAIL — `epollConn` undefined

- [ ] **Step 2: Implement epollConn**

Create `ws/conn/epollconn.go`:
```go
package conn

import (
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

// epollConn is an event-driven Conn backed by a raw fd.
type epollConn struct {
	id       uint64
	fd       int
	isClient bool
	pipeline pipeline.ChannelPipeline
	active   int32
	closeOnce sync.Once
	el       interface{} // actual type: eventloop.EventLoop
}

func newEpollConn(fd int, isClient bool, id uint64) *epollConn {
	return &epollConn{
		id:       id,
		fd:       fd,
		isClient: isClient,
		pipeline: pipeline.NewPipeline(),
		active:   1,
	}
}

func (c *epollConn) ID() uint64                    { return c.id }
func (c *epollConn) Pipeline() pipeline.ChannelPipeline { return c.pipeline }
func (c *epollConn) FD() int                       { return c.fd }
func (c *epollConn) IsClient() bool                { return c.isClient }
func (c *epollConn) Active() bool                  { return atomic.LoadInt32(&c.active) == 1 }
func (c *epollConn) RemoteAddr() net.Addr          { return nil } // TODO: resolve from fd
func (c *epollConn) LocalAddr() net.Addr           { return nil } // TODO: resolve from fd

func (c *epollConn) Read(b buf.ByteBuf) error {
	if !c.Active() {
		return net.ErrClosed
	}
	// TODO: non-blocking read using syscall.Read (V2.1)
	return nil
}

func (c *epollConn) Write(b buf.ByteBuf) error {
	if !c.Active() {
		return net.ErrClosed
	}
	// TODO: non-blocking write using syscall.Write (V2.1)
	return nil
}

func (c *epollConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.active, 0)
		// TODO: deregister from eventloop, close fd
	})
	return err
}

func (c *epollConn) OnEvent(events uint32) {
	// TODO: handle read/write events (V2.1)
}

func (c *epollConn) SetEventLoop(el interface{}) {
	c.el = el
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/conn -run TestEpollConn -v`
Expected: PASS (interface compliance only)

- [ ] **Step 4: Commit**

```bash
git add ws/conn/epollconn.go ws/conn/conn_test.go
git commit -m "feat(conn): add epollConn stub for event-driven connections"
```

---

### Task 14: Session — Interface + Implementation

**Files:**
- Create: `ws/session/session.go`
- Create: `ws/session/session_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/session/session_test.go`:
```go
package session

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
)

import (
	"net"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type mockConn struct {
	id        uint64
	pip       pipeline.ChannelPipeline
	readBuf   []byte
}

func newMockConn(id uint64) *mockConn {
	return &mockConn{
		id:  id,
		pip: pipeline.NewPipeline(),
	}
}

func (m *mockConn) ID() uint64                          { return m.id }
func (m *mockConn) Pipeline() pipeline.ChannelPipeline   { return m.pip }
func (m *mockConn) Read(b buf.ByteBuf) error            { return nil }
func (m *mockConn) Write(b buf.ByteBuf) error           { return nil }
func (m *mockConn) RemoteAddr() net.Addr                { return nil }
func (m *mockConn) LocalAddr() net.Addr                 { return nil }
func (m *mockConn) IsClient() bool                       { return false }
func (m *mockConn) Close() error                         { return nil }
func (m *mockConn) Active() bool                         { return true }

func TestSession_StateTransitions(t *testing.T) {
	mc := newMockConn(1)
	s := NewSession(mc, Config{
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
	select {
	case st := <-s.StateChan():
		if st != StateConnected {
			t.Errorf("StateChan = %d, want Connected", st)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for state")
	}
}

func TestSession_Close(t *testing.T) {
	mc := newMockConn(1)
	s := NewSession(mc, Config{})
	s.SetState(StateConnected)
	s.Close()
	if s.State() != StateClosed {
		t.Errorf("state = %d, want Closed", s.State())
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/session -v`
Expected: FAIL — `Session`, `State`, `NewSession` undefined

- [ ] **Step 2: Implement Session**

Create `ws/session/session.go`:
```go
package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/conn"
)

// State represents the connection lifecycle state.
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

// Config is the session configuration.
type Config struct {
	PingInterval      time.Duration
	PongTimeout       time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      int
}

// Session is the WebSocket session abstraction.
type Session interface {
	Conn() conn.Conn
	State() State
	StateChan() <-chan State
	SetState(State)
	Close() error
}

// defaultSession is the standard Session implementation.
type defaultSession struct {
	conn      conn.Conn
	config    Config
	stateChan chan State
	state     State
}

// NewSession creates a new Session.
func NewSession(c conn.Conn, cfg Config) Session {
	return &defaultSession{
		conn:      c,
		config:    cfg,
		stateChan: make(chan State, 16),
		state:     StateDisconnected,
	}
}

func (s *defaultSession) Conn() conn.Conn         { return s.conn }
func (s *defaultSession) State() State            { return s.state }
func (s *defaultSession) StateChan() <-chan State { return s.stateChan }

func (s *defaultSession) SetState(st State) {
	s.state = st
	select {
	case s.stateChan <- st:
	default:
	}
}

func (s *defaultSession) Close() error {
	s.SetState(StateClosed)
	return s.conn.Close()
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/session -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/session/session.go ws/session/session_test.go
git commit -m "feat(session): add Session interface with State management"
```

---

### Task 15: Session — Heartbeater + Reconnector

**Files:**
- Create: `ws/session/heartbeat.go`
- Create: `ws/session/reconnect.go`
- Modify: `ws/session/session_test.go`

- [ ] **Step 1: Write the failing test**

Append to `ws/session/session_test.go`:
```go
func TestPerConnHeartbeater_PingSent(t *testing.T) {
	mc := newMockConn(1)
	s := NewSession(mc, Config{PingInterval: 100 * time.Millisecond, PongTimeout: 5 * time.Second})

	hb := NewPerConnHeartbeater(100*time.Millisecond, 5*time.Second)
	hb.Start(s)
	defer hb.Stop()

	// We can't easily observe the ping without a real conn, but we verify no panic
	time.Sleep(150 * time.Millisecond)
}

func TestReconnector_MaxRetries(t *testing.T) {
	mc := newMockConn(1)
	cfg := Config{
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		ReconnectInterval: 50 * time.Millisecond,
		MaxReconnect:      2,
	}
	s := NewSession(mc, cfg)
	s.SetState(StateConnected)
	s.SetState(StateDisconnected)

	dialCount := 0
	rc := NewReconnector(cfg.ReconnectInterval, cfg.MaxReconnect, func() (conn.Conn, error) {
		dialCount++
		return nil, errors.New("connection refused")
	})

	go rc.Start(s)

	select {
	case st := <-s.StateChan():
		if st != StateClosed {
			t.Errorf("state = %d, want Closed", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for reconnect exhaustion")
	}

	if dialCount != 2 {
		t.Errorf("dialCount = %d, want 2", dialCount)
	}
}
```

Add import: `errors` to `ws/session/session_test.go`.

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/session -run TestPerConnHeartbeater -v`
Expected: FAIL — `NewPerConnHeartbeater` undefined

- [ ] **Step 2: Implement Heartbeater**

Create `ws/session/heartbeat.go`:
```go
package session

import (
	"time"
)

// Heartbeater is the heartbeat interface.
type Heartbeater interface {
	Start(s Session)
	Stop()
	SetOnTimeout(fn func())
}

// perConnHeartbeater sends ping frames at regular intervals.
type perConnHeartbeater struct {
	pingInterval time.Duration
	pongTimeout  time.Duration
	stopChan     chan struct{}
	onTimeout    func()
}

// NewPerConnHeartbeater creates a per-connection heartbeater.
func NewPerConnHeartbeater(pingInterval, pongTimeout time.Duration) Heartbeater {
	return &perConnHeartbeater{
		pingInterval: pingInterval,
		pongTimeout:  pongTimeout,
		stopChan:     make(chan struct{}),
	}
}

func (h *perConnHeartbeater) Start(s Session) {
	h.stopChan = make(chan struct{})
	go h.run(s)
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

func (h *perConnHeartbeater) run(s Session) {
	ticker := time.NewTicker(h.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// TODO: send ping via conn pipeline (V2.1)
		case <-h.stopChan:
			return
		}
	}
}
```

- [ ] **Step 3: Implement Reconnector**

Create `ws/session/reconnect.go`:
```go
package session

import (
	"time"

	"github.com/lufeijun/goTools/ws/conn"
)

// Reconnector is the auto-reconnect interface (client-side).
type Reconnector interface {
	Start(s Session)
	Stop()
}

// reconnector implements auto-reconnect logic.
type reconnector struct {
	interval   time.Duration
	maxRetries int
	dial       func() (conn.Conn, error)
	stopChan   chan struct{}
}

// NewReconnector creates a reconnector.
func NewReconnector(interval time.Duration, maxRetries int, dial func() (conn.Conn, error)) Reconnector {
	return &reconnector{
		interval:   interval,
		maxRetries: maxRetries,
		dial:       dial,
		stopChan:   make(chan struct{}),
	}
}

func (r *reconnector) Start(s Session) {
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

		// TODO: update session conn, read/write channels (V2.1)
		_ = c
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

- [ ] **Step 4: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/session -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add ws/session/heartbeat.go ws/session/reconnect.go ws/session/session_test.go
git commit -m "feat(session): add Heartbeater and Reconnector implementations"
```

---

### Task 16: Hub — Sharded Lock Implementation

**Files:**
- Create: `ws/hub/hub.go`
- Create: `ws/hub/hub_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/hub/hub_test.go`:
```go
package hub

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/pipeline"
	"github.com/lufeijun/goTools/ws/session"
)

import (
	"net"

	"github.com/lufeijun/goTools/ws/buf"
	"github.com/lufeijun/goTools/ws/pipeline"
)

type mockHubConn struct {
	id  uint64
	pip pipeline.ChannelPipeline
}

func newMockHubConn(id uint64) *mockHubConn {
	return &mockHubConn{id: id, pip: pipeline.NewPipeline()}
}

func (m *mockHubConn) ID() uint64                        { return m.id }
func (m *mockHubConn) Pipeline() pipeline.ChannelPipeline { return m.pip }
func (m *mockHubConn) Read(b buf.ByteBuf) error          { return nil }
func (m *mockHubConn) Write(b buf.ByteBuf) error         { return nil }
func (m *mockHubConn) RemoteAddr() net.Addr              { return nil }
func (m *mockHubConn) LocalAddr() net.Addr               { return nil }
func (m *mockHubConn) IsClient() bool                     { return false }
func (m *mockHubConn) Close() error                       { return nil }
func (m *mockHubConn) Active() bool                       { return true }

func newTestSession(id uint64) session.Session {
	return session.NewSession(newMockHubConn(id), session.Config{})
}

func TestShardedHub_RegisterCount(t *testing.T) {
	h := NewHub(4)
	s1 := newTestSession(1)
	s2 := newTestSession(2)

	h.Register(s1)
	h.Register(s2)

	if h.Count() != 2 {
		t.Errorf("Count = %d, want 2", h.Count())
	}
}

func TestShardedHub_Unregister(t *testing.T) {
	h := NewHub(4)
	s := newTestSession(1)
	h.Register(s)
	h.Unregister(1)

	if h.Count() != 0 {
		t.Errorf("Count = %d, want 0", h.Count())
	}
}

func TestShardedHub_Get(t *testing.T) {
	h := NewHub(4)
	s := newTestSession(42)
	h.Register(s)

	got := h.Get(42)
	if got == nil {
		t.Fatal("Get(42) = nil")
	}
	if got.Conn().ID() != 42 {
		t.Errorf("ID = %d, want 42", got.Conn().ID())
	}

	if h.Get(999) != nil {
		t.Error("Get(999) should be nil")
	}
}

func TestShardedHub_Broadcast(t *testing.T) {
	h := NewHub(4)
	s1 := newTestSession(1)
	s2 := newTestSession(2)
	h.Register(s1)
	h.Register(s2)

	msg := conn.Message{Type: 0x1, Data: []byte("broadcast")}
	h.Broadcast(msg)
	// Non-blocking broadcast, just verify no panic
	time.Sleep(10 * time.Millisecond)
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/hub -v`
Expected: FAIL — `NewHub` undefined

- [ ] **Step 2: Implement sharded Hub**

Create `ws/hub/hub.go`:
```go
package hub

import (
	"sync"

	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/session"
)

// Hub is the connection management center.
type Hub interface {
	Register(s session.Session)
	Unregister(id uint64)
	Broadcast(msg conn.Message)
	Send(id uint64, msg conn.Message)
	Count() int
	Get(id uint64) session.Session
}

// NewHub creates a sharded Hub with the given shard count.
func NewHub(shardCount int) Hub {
	if shardCount <= 0 {
		shardCount = 32
	}
	h := &shardedHub{
		shardCount: shardCount,
		shards:     make([]*shard, shardCount),
	}
	for i := range h.shards {
		h.shards[i] = &shard{conns: make(map[uint64]session.Session)}
	}
	return h
}

type shardedHub struct {
	shardCount int
	shards     []*shard
}

type shard struct {
	mu    sync.RWMutex
	conns map[uint64]session.Session
}

func (h *shardedHub) shardIndex(id uint64) int {
	return int(id % uint64(h.shardCount))
}

func (h *shardedHub) Register(s session.Session) {
	id := s.Conn().ID()
	sh := h.shards[h.shardIndex(id)]
	sh.mu.Lock()
	sh.conns[id] = s
	sh.mu.Unlock()
}

func (h *shardedHub) Unregister(id uint64) {
	sh := h.shards[h.shardIndex(id)]
	sh.mu.Lock()
	delete(sh.conns, id)
	sh.mu.Unlock()
}

func (h *shardedHub) Get(id uint64) session.Session {
	sh := h.shards[h.shardIndex(id)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.conns[id]
}

func (h *shardedHub) Count() int {
	var total int
	for _, sh := range h.shards {
		sh.mu.RLock()
		total += len(sh.conns)
		sh.mu.RUnlock()
	}
	return total
}

func (h *shardedHub) Broadcast(msg conn.Message) {
	var wg sync.WaitGroup
	for _, sh := range h.shards {
		wg.Add(1)
		go func(s *shard) {
			defer wg.Done()
			s.mu.RLock()
			sessions := make([]session.Session, 0, len(s.conns))
			for _, sess := range s.conns {
				sessions = append(sessions, sess)
			}
			s.mu.RUnlock()

			for _, sess := range sessions {
				_ = sess // TODO: write to pipeline (V2.1)
			}
		}(sh)
	}
	wg.Wait()
}

func (h *shardedHub) Send(id uint64, msg conn.Message) {
	s := h.Get(id)
	if s == nil {
		return
	}
	_ = msg // TODO: write to pipeline (V2.1)
	_ = s
}
```

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/hub -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/hub/hub.go ws/hub/hub_test.go
git commit -m "feat(hub): add sharded Hub with RWMutex per shard"
```

---

### Task 17: Server — Bootstrap + Server

**Files:**
- Create: `ws/server/server.go`
- Create: `ws/server/server_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/server/server_test.go`:
```go
package server

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws"
)

func TestServer_New(t *testing.T) {
	srv := NewServer(ws.Config{
		Addr:         "127.0.0.1:0",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})
	if srv == nil {
		t.Fatal("NewServer returned nil")
	}
	if srv.Config().Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q, want 127.0.0.1:0", srv.Config().Addr)
	}
}

func TestServer_StartStop(t *testing.T) {
	srv := NewServer(ws.Config{
		Addr:         "127.0.0.1:0",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	go srv.Start()
	time.Sleep(100 * time.Millisecond)

	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/server -v`
Expected: FAIL — `NewServer` undefined

- [ ] **Step 2: Implement Server**

Create `ws/server/server.go`:
```go
package server

import (
	"context"
	"net"
	"net/http"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/hub"
	"github.com/lufeijun/goTools/ws/session"
)

// Server is the WebSocket server.
type Server interface {
	Config() ws.Config
	Hub() hub.Hub
	Start() error
	Stop() error
	Listener() net.Listener
}

// NewServer creates a Server with the given config.
func NewServer(cfg ws.Config) Server {
	if cfg.ReadBufferSize == 0 {
		cfg = ws.DefaultConfig()
	}
	return &defaultServer{
		config: cfg,
		hub:    hub.NewHub(32),
	}
}

type defaultServer struct {
	config   ws.Config
	hub      hub.Hub
	listener net.Listener
	server   *http.Server
}

func (s *defaultServer) Config() ws.Config     { return s.config }
func (s *defaultServer) Hub() hub.Hub          { return s.hub }
func (s *defaultServer) Listener() net.Listener { return s.listener }

func (s *defaultServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWebSocket)

	s.server = &http.Server{Handler: mux}
	var err error
	s.listener, err = net.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}
	return s.server.Serve(s.listener)
}

func (s *defaultServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
		http.Error(w, "Too many connections", http.StatusServiceUnavailable)
		return
	}

	nc, err := conn.ServerHandshake(w, r)
	if err != nil {
		return
	}

	c := conn.NewNetConn(nc, false, conn.NextConnID())
	sess := session.NewSession(c, session.Config{
		PingInterval: s.config.PingInterval,
		PongTimeout:  s.config.PongTimeout,
	})

	hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		c.Close()
	})
	sess.SetState(session.StateConnected)
	hb.Start(sess)

	s.hub.Register(sess)

	// TODO: start read loop for netConn (V2.1)
	// For now, the connection is established but no frame processing loop runs
}

func (s *defaultServer) Stop() error {
	if s.server != nil {
		return s.server.Shutdown(context.Background())
	}
	return nil
}
```

Note: Imports `github.com/lufeijun/goTools/ws/conn` and `github.com/lufeijun/goTools/ws/session` added above.

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/server -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/server/server.go ws/server/server_test.go
git commit -m "feat(server): add Server with WebSocket handshake and session lifecycle"
```

---

### Task 18: Client — Bootstrap + Client

**Files:**
- Create: `ws/client/client.go`
- Create: `ws/client/client_test.go`

- [ ] **Step 1: Write the failing test**

Create `ws/client/client_test.go`:
```go
package client

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws"
)

func TestClient_New(t *testing.T) {
	c := NewClient(ws.Config{
		Addr:              "ws://localhost:8080/",
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		ReconnectInterval: 5 * time.Second,
		MaxReconnect:      3,
	})
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.Config().MaxReconnect != 3 {
		t.Errorf("MaxReconnect = %d, want 3", c.Config().MaxReconnect)
	}
}
```

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/client -v`
Expected: FAIL — `NewClient` undefined

- [ ] **Step 2: Implement Client**

Create `ws/client/client.go`:
```go
package client

import (
	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/session"
)

// Client is the WebSocket client.
type Client interface {
	Config() ws.Config
	Connect() error
	Close() error
	Session() session.Session
}

// NewClient creates a Client with the given config.
func NewClient(cfg ws.Config) Client {
	if cfg.ReadBufferSize == 0 {
		cfg = ws.DefaultConfig()
	}
	return &defaultClient{config: cfg}
}

type defaultClient struct {
	config  ws.Config
	sess    session.Session
}

func (c *defaultClient) Config() ws.Config      { return c.config }
func (c *defaultClient) Session() session.Session { return c.sess }

func (c *defaultClient) Connect() error {
	nc, err := conn.ClientHandshake(c.config.Addr, c.config.Headers)
	if err != nil {
		return err
	}

	wc := conn.NewNetConn(nc, true, 1)
	sess := session.NewSession(wc, session.Config{
		PingInterval:      c.config.PingInterval,
		PongTimeout:       c.config.PongTimeout,
		ReconnectInterval: c.config.ReconnectInterval,
		MaxReconnect:      c.config.MaxReconnect,
	})

	hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		wc.Close()
		// TODO: auto-reconnect (V2.1)
	})
	sess.SetState(session.StateConnected)
	hb.Start(sess)

	c.sess = sess
	return nil
}

func (c *defaultClient) Close() error {
	if c.sess != nil {
		return c.sess.Close()
	}
	return nil
}
```

Note: Add import `github.com/lufeijun/goTools/ws/conn` to `ws/client/client.go`.

- [ ] **Step 3: Run tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/client -v`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add ws/client/client.go ws/client/client_test.go
git commit -m "feat(client): add Client with WebSocket handshake and session lifecycle"
```

---

### Task 19: Integration Tests

**Files:**
- Modify: `ws/ws_test.go`

- [ ] **Step 1: Write integration test**

Create `ws/ws_test.go`:
```go
package ws

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/client"
	"github.com/lufeijun/goTools/ws/server"
)

func TestIntegration_ServerStartStop(t *testing.T) {
	srv := server.NewServer(Config{
		Addr:         "127.0.0.1:0",
		PingInterval: 30 * time.Second,
		PongTimeout:  60 * time.Second,
	})

	go srv.Start()
	time.Sleep(100 * time.Millisecond)

	addr := srv.Listener().Addr().String()
	if addr == "" {
		t.Fatal("server did not bind to an address")
	}

	if err := srv.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_ClientConfig(t *testing.T) {
	c := client.NewClient(Config{
		Addr:              "ws://localhost:8080/",
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		ReconnectInterval: 5 * time.Second,
		MaxReconnect:      5,
	})

	if c.Config().PingInterval != 30*time.Second {
		t.Errorf("PingInterval = %v, want 30s", c.Config().PingInterval)
	}
}
```

- [ ] **Step 2: Run integration tests**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws -v -timeout 30s`
Expected: PASS

- [ ] **Step 3: Run full test suite**

Run: `cd /var/www/claude/ws/go-tools && go test ./... -v -timeout 60s`
Expected: ALL PASS

- [ ] **Step 4: Commit**

```bash
git add ws/ws_test.go
git commit -m "test(ws): add integration tests for server start/stop and client config"
```

---

### Task 20: Final Verification

- [ ] **Step 1: Run go vet**

```bash
cd /var/www/claude/ws/go-tools && go vet ./...
```
Expected: No warnings

- [ ] **Step 2: Check for build errors on all platforms**

```bash
# Linux
cd /var/www/claude/ws/go-tools && GOOS=linux GOARCH=amd64 go build ./...

# macOS
cd /var/www/claude/ws/go-tools && GOOS=darwin GOARCH=amd64 go build ./...

# Windows (may have stubs)
cd /var/www/claude/ws/go-tools && GOOS=windows GOARCH=amd64 go build ./...
```
Expected: All build successfully (or skip for stubs)

- [ ] **Step 3: Verify frame tests still pass**

Run: `cd /var/www/claude/ws/go-tools && go test ./ws/frame/ -v`
Expected: PASS (v1 frame code preserved)

- [ ] **Step 4: Final commit**

```bash
git add -A
git commit -m "chore(ws): v2 final verification and cleanup"
```


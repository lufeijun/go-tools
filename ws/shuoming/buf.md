# ws/buf — 引用计数字节缓冲区

`ws/buf` 是 v2 引入的缓冲区子系统，提供**引用计数 ByteBuf** 和**分级对象池**，替代 Go 原生 `[]byte`，解决高并发场景下的内存分配、零拷贝和生命周期管理问题。

---

## 设计背景

在 v1 中，框架使用 `sync.Pool` 对 `[]byte` 做两级复用。v2 面临更高并发（十万到百万连接），需要：

1. **读写指针分离** — 避免每次读操作都切片新数组
2. **引用计数** — 同一份数据在多个 goroutine/连接间传递时，防止提前释放
3. **零拷贝切片** — `Slice()` 创建共享底层数组的新视图，不复制数据
4. **分级对象池** — 按容量分级回收，减少大对象 GC 压力

---

## 核心接口

### ByteBuf

```go
type ByteBuf interface {
    // 读操作
    ReadableBytes() int
    ReadBytes(n int) []byte
    ReadAll() []byte
    Skip(n int)
    Peek(n int) []byte

    // 写操作
    WritableBytes() int
    Write(p []byte) (int, error)
    WriteByte(b byte) error
    EnsureWritable(min int)

    // 零拷贝切片（共享底层数组，引用计数 +1）
    Slice(start, length int) ByteBuf

    // 引用计数
    Retain() ByteBuf
    Release()
    RefCount() int

    // 内部访问
    Bytes() []byte
    ReaderIndex() int
    WriterIndex() int
    SetReaderIndex(int)
    SetWriterIndex(int)
}
```

### Pool

```go
type Pool interface {
    Get(capacity int) ByteBuf
    Put(ByteBuf)
}
```

---

## 默认实现：byteBuf

`byteBuf` 是 `ByteBuf` 的默认实现，位于 `bytebuf.go`。

### 内部结构

```go
type byteBuf struct {
    data         []byte   // 底层字节数组
    readerIndex  int      // 读指针
    writerIndex  int      // 写指针
    refCount     int32    // 引用计数（原子操作）
    pool         Pool     // 所属对象池（可为 nil）
}
```

### 读写指针模型

```
[0  1  2  3  4  5  6  7  8  9]   ← 底层 data 数组（capacity = 10）
        ↑              ↑
   readerIndex     writerIndex

ReadableBytes() = writerIndex - readerIndex = 5
WritableBytes() = capacity - writerIndex = 4
```

- `Write()` 从 `writerIndex` 开始写入，然后后移 `writerIndex`
- `ReadBytes(n)` 从 `readerIndex` 开始读取 `n` 字节，然后后移 `readerIndex`
- `Peek(n)` 同 `ReadBytes` 但不移动 `readerIndex`
- `Skip(n)` 直接后移 `readerIndex`

### 自动扩容

`EnsureWritable(min)` 会在 `WritableBytes() < min` 时自动扩容：
- 优先尝试 `cap(data) * 2`
- 若仍不足，则按 `needed` 精确分配
- 通过 `make` + `copy` 迁移数据

---

## 引用计数规则

引用计数是 `byteBuf` 最核心的安全机制，使用 `sync/atomic` 保证线程安全。

### 生命周期

| 操作 | 引用计数变化 | 说明 |
|---|---|---|
| `NewByteBuf()` / `Pool.Get()` | `refCount = 1` | 创建时初始化为 1 |
| `Retain()` | `+1` | 有其他对象引用时调用 |
| `Slice()` | 原始 `+1`，新视图 `= 1` | 新视图依赖原始 buf |
| `Release()` | `-1` | 减少引用；归零时回收到 Pool |

### 安全保护

```go
func (b *byteBuf) Release() {
    rc := atomic.AddInt32(&b.refCount, -1)
    if rc == 0 {
        // 回收到 Pool
    } else if rc < 0 {
        panic("double free detected")
    }
}
```

- **Double-free 保护**：`Release()` 后 `refCount < 0` 直接 `panic`
- **已释放操作保护**：`Retain()` 时若 `refCount <= 1` 也 `panic`
- 开发阶段通过 `go test` + `go vet` 覆盖所有路径

### 使用示例

```go
package main

import (
    "fmt"
    "github.com/lufeijun/goTools/ws/buf"
)

func main() {
    // 1. 创建 ByteBuf，容量 64 字节，引用计数 = 1
    bb := buf.NewByteBuf(64)

    // 2. 写入数据
    bb.Write([]byte("hello world"))
    fmt.Printf("可读字节数: %d\n", bb.ReadableBytes()) // 11

    // 3. Peek — 不移动读指针
    peek := bb.Peek(5)
    fmt.Printf("Peek(5): %s\n", string(peek))        // hello
    fmt.Printf("Peek 后可读: %d\n", bb.ReadableBytes()) // 还是 11

    // 4. ReadBytes — 移动读指针
    data := bb.ReadBytes(5)
    fmt.Printf("ReadBytes(5): %s\n", string(data))    // hello
    fmt.Printf("读取后可读: %d\n", bb.ReadableBytes())  // 6

    // 5. 零拷贝切片
    sliced := bb.Slice(0, 5) // 取前 5 个字节
    sliced.Release()         // 释放切片视图

    // 6. 释放原始 ByteBuf
    bb.Release()
}
```

---

## 分级对象池：bufPool

`bufPool` 是 `Pool` 的默认实现，位于 `pool.go`，使用 `sync.Pool` 按容量分级管理。

### 分级策略

```go
func NewPool(smallSize, defaultSize, largeSize int) Pool
```

| 级别 | 容量范围 | 默认池大小 | 用途 |
|---|---|---|---|
| small | `<= 512B` | 4096 | 控制帧、心跳帧 |
| default | `<= 4096B` | 1024 | 普通文本/二进制消息 |
| large | `<= 65536B` | 256 | 大消息、批量数据 |
| 直接分配 | `> largeSize` | — | 超大消息，GC 回收 |

### 为什么分级？

- `sync.Pool` 不限制对象大小，大对象和小对象混池会导致内存碎片
- WebSocket 消息长度分布极不均匀：大部分 <1KB，偶尔有大文件传输
- 分级后，小对象池命中率极高，大对象不占用小对象池空间

### Get / Put 流程

```go
// Get：按需求容量选择级别，优先从 sync.Pool 复用
b := p.Get(100)   // 从 smallPool 取
b := p.Get(5000)  // 从 defaultPool 取
b := p.Get(200000)// 直接 make，不归 Pool

// Put：按底层数组 cap 放回对应级别
b.Release()       // 如果 b 来自 Pool，自动归还
```

注意：`Release()` 时会检查 `pool != nil`，只有从 Pool 取出的才会归还。

---

## 与 frame 层的协作

`frame.ReadFrame` 和 `frame.WriteFrame` 直接操作 `ByteBuf` 的 `ReaderIndex`/`WriterIndex`，避免多次内存拷贝：

- **读方向**：`eventloop` 读数据直接写入 `ByteBuf` → `FrameDecoder` 解析时通过 `Slice()` 共享 Payload
- **写方向**：`FrameEncoder` 直接写入发送队列的 `ByteBuf`，合并 header + payload 后单次 `write()`
- **广播**：同一份 `ByteBuf` 通过 `Retain()` 增加引用计数，发送到多个连接后各 `Release()`

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `bytebuf.go` | `ByteBuf` 接口 + `byteBuf` 默认实现 |
| `pool.go` | `Pool` 接口 + `bufPool` 分级对象池 |
| `bytebuf_test.go` | 读写、切片、引用计数、double-free panic 测试 |
| `pool_test.go` | 分级 Get/Put、复用测试 |

---

## 注意事项

1. **必须成对使用 Retain/Release** — 每个 `Retain()`、`Slice()`、`Pool.Get()` 都必须对应一个 `Release()`
2. **不要在 Release 后操作 ByteBuf** — 会触发 panic，这是设计上的刻意保护
3. **NewByteBuf 不归 Pool** — 只有通过 `Pool.Get()` 创建的才关联 Pool，`NewByteBuf` 创建的 `Release()` 后直接丢弃
4. **Slice 会 Retain 原始 buf** — 释放顺序不影响安全，但建议先 `Release` 切片再 `Release` 原始 buf

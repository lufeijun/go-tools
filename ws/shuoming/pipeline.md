# ws/pipeline — 处理器链

`ws/pipeline` 是 v2 最核心的抽象之一，借鉴 Netty 的 `ChannelPipeline` 模型，为每个连接维护一条**入站（Inbound）**和**出站（Outbound）**处理器链。业务逻辑以 Handler 插件的形式组装到链中，数据在链上流动时被逐层处理。

---

## 设计背景

v1 使用 Channel 式 API（`ReadChan()` / `WriteChan()`），每个连接 2-3 个 goroutine。v2 改为事件驱动后，需要一种不依赖 goroutine 的数据处理模型：

- **Pipeline** 在连接建立时组装，运行时事件遍历**不加锁**
- **Handler** 是业务代码的载体，可插拔、可复用、可测试
- **Context** 提供链式调用的上下文，Handler 通过它将事件传递给下一个节点

---

## 核心接口

### ChannelPipeline

```go
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
```

### ChannelHandler（基接口）

```go
type ChannelHandler interface {
    Name() string
}
```

### InboundHandler（入站处理器）

处理从网络读进来的数据：

```go
type InboundHandler interface {
    ChannelHandler
    ChannelRead(ctx Context, msg interface{})
    ChannelActive(ctx Context)
    ChannelInactive(ctx Context)
    ExceptionCaught(ctx Context, err error)
}
```

### OutboundHandler（出站处理器）

处理要发往网络的数据：

```go
type OutboundHandler interface {
    ChannelHandler
    Write(ctx Context, msg interface{})
    Flush(ctx Context)
}
```

### Context

Handler 在 Pipeline 中的执行上下文：

```go
type Context interface {
    Pipeline() ChannelPipeline
    FireChannelRead(msg interface{})   // 触发下一个 InboundHandler
    FireChannelWrite(msg interface{})  // 触发下一个 OutboundHandler（往回走）
    FireChannelActive()
    FireChannelInactive()
    Write(msg interface{})             // 触发 Outbound 链（从当前位置往回走）
    Flush()
}
```

---

## 数据流向

### Inbound 链（网络 → 业务）

```
eventloop 读数据 → ByteBuf → Head → FrameDecoder → HeartbeatHandler → BizHandler → Tail
```

- 遍历方向：**从头到尾**（Head → Tail）
- 触发方法：`ctx.FireChannelRead(msg)`

### Outbound 链（业务 → 网络）

```
BizHandler.Write → FrameEncoder → Head.Write → eventloop 写数据
```

- 遍历方向：**从尾到头**（Tail → Head）
- 触发方法：`ctx.Write(msg)` 或 `ctx.FireChannelWrite(msg)`

---

## 默认实现：defaultPipeline

`pipeline.go` 中的 `defaultPipeline` 是标准实现。

### 内部结构

```go
type defaultPipeline struct {
    mu   sync.RWMutex    // 仅保护构建阶段（Add/Remove）
    head *handlerContext // 哨兵头节点
    tail *handlerContext // 哨兵尾节点
    ctxs map[string]*handlerContext // name → context 索引
}

type handlerContext struct {
    pipeline *defaultPipeline
    name     string
    handler  ChannelHandler
    prev     *handlerContext
    next     *handlerContext
}
```

使用**双向链表**存储 Handler，保证事件遍历时的顺序性和 O(1) 的节点查找。

### 构建阶段（加锁）

```go
p.AddLast("frameDecoder", &FrameDecoder{})
p.AddLast("biz", &BizHandler{})
p.AddLast("frameEncoder", &FrameEncoder{})
```

- `AddFirst` / `AddLast` / `Remove` 使用 `sync.RWMutex` 保护
- 运行时事件遍历（`FireChannelRead` 等）**不加锁**，避免竞争
- 重复 name、nil handler、空 name 都会 `panic`，提前暴露配置错误

### 事件遍历（无锁）

```go
func (c *handlerContext) invokeChannelRead(msg interface{}) {
    next := c.findNextInbound() // 沿 next 指针向后找下一个 InboundHandler
    if next != nil {
        next.handler.(InboundHandler).ChannelRead(next, msg)
    }
}
```

```go
func (c *handlerContext) invokeChannelWrite(msg interface{}) {
    prev := c.findPrevOutbound() // 沿 prev 指针向前找下一个 OutboundHandler
    if prev != nil {
        prev.handler.(OutboundHandler).Write(prev, msg)
    }
}
```

---

## 关键区别：Write vs FireChannelWrite

| 方法 | 遍历起点 | 用途 |
|---|---|---|
| `ctx.Write(msg)` | **当前 Handler 位置**往回走 | 在 InboundHandler 中想发送回复时使用 |
| `ctx.FireChannelWrite(msg)` | **Tail** 往回走 | 从 Pipeline 外部触发 Outbound 链 |

**示例：**

```go
// EchoHandler 是 InboundHandler，收到消息后想回写
type EchoHandler struct{}

func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*conn.Message); ok {
        // 使用 ctx.Write，从 EchoHandler 的位置往回走 Outbound 链
        ctx.Write(&conn.Message{Type: m.Type, Data: m.Data})
    }
    ctx.FireChannelRead(msg) // 继续传给下一个 InboundHandler
}
```

---

## 完整示例：组装 Pipeline

```go
package main

import (
    "fmt"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/pipeline"
)

// FrameDecoder：ByteBuf → Message（假设的解码 Handler）
type FrameDecoder struct{}
func (h *FrameDecoder) Name() string { return "frameDecoder" }
func (h *FrameDecoder) ChannelRead(ctx pipeline.Context, msg interface{}) {
    fmt.Println("【FrameDecoder】正在解码帧...")
    ctx.FireChannelRead(msg)
}
func (h *FrameDecoder) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *FrameDecoder) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *FrameDecoder) ExceptionCaught(ctx pipeline.Context, err error) {}

// BizHandler：处理业务消息
type BizHandler struct{}
func (h *BizHandler) Name() string { return "biz" }
func (h *BizHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*conn.Message); ok {
        fmt.Printf("【BizHandler】收到: %s\n", string(m.Data))
    }
    ctx.FireChannelRead(msg)
}
func (h *BizHandler) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *BizHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *BizHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

// FrameEncoder：Message → ByteBuf（假设的编码 Handler）
type FrameEncoder struct{}
func (h *FrameEncoder) Name() string { return "frameEncoder" }
func (h *FrameEncoder) Write(ctx pipeline.Context, msg interface{}) {
    fmt.Println("【FrameEncoder】正在编码帧...")
    ctx.FireChannelWrite(msg)
}
func (h *FrameEncoder) Flush(ctx pipeline.Context) {}

func main() {
    p := pipeline.NewPipeline()

    // Inbound：按顺序添加
    p.AddLast("frameDecoder", &FrameDecoder{})
    p.AddLast("biz", &BizHandler{})

    // Outbound：写数据时从尾到头遍历
    p.AddLast("frameEncoder", &FrameEncoder{})

    // 模拟收到消息
    p.FireChannelRead(&conn.Message{Type: 0x1, Data: []byte("hello")})

    // 模拟发送消息
    p.FireChannelWrite(&conn.Message{Type: 0x1, Data: []byte("world")})
}
```

**输出：**

```
【FrameDecoder】正在解码帧...
【BizHandler】收到: hello
【FrameEncoder】正在编码帧...
```

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `handler.go` | `ChannelPipeline`、`ChannelHandler`、`InboundHandler`、`OutboundHandler`、`Context` 接口 |
| `pipeline.go` | `defaultPipeline`、`handlerContext` 实现，链表遍历逻辑 |
| `handler_test.go` | 接口合规性测试 |
| `pipeline_test.go` | Inbound/Outbound 链、Add/Remove、Active/Inactive、异常路径测试 |
| `race_test.go` | 并发竞争检测 |

---

## 注意事项

1. **Pipeline 构建后不要修改** — 运行时事件遍历无锁，若运行时 Add/Remove 会导致链表竞争（虽然 `mu` 保护 map，但遍历逻辑无锁）
2. **必须调用 FireChannelRead / FireChannelWrite** — 否则事件会"断"在当前 Handler
3. **InboundHandler 中调用 ctx.Write() 发送回复** — 这是从 Inbound 切换为 Outbound 的标准方式
4. **ExceptionCaught 尚未完全实现** — V2.0 中 `FireExceptionCaught` 为空实现，V2.1 完善
5. **每个连接有独立的 Pipeline** — `server`/`client` 在创建 `netConn`/`epollConn` 时自动 `pipeline.NewPipeline()`

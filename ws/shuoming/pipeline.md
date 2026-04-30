# ws/pipeline — 处理器链

`ws/pipeline` 是 v2 最核心的抽象之一，借鉴 Netty 的 `ChannelPipeline` 模型，为每个连接维护一条**入站（Inbound）**和**出站（Outbound）**处理器链。业务逻辑以 Handler 插件的形式组装到链中，数据在链上流动时被逐层处理。

---

## 设计背景

v1 使用 Channel 式 API（`ReadChan()` / `WriteChan()`），每个连接 2-3 个 goroutine。v2 改为事件驱动后，需要一种不依赖 goroutine 的数据处理模型：

- **Pipeline** 在连接建立时组装，运行时事件遍历**不加锁**
- **Handler** 是业务代码的载体，可插拔、可复用、可测试
- **Context** 提供链式调用的上下文，Handler 通过它将事件传递给下一个节点

### 为什么不用 channel？

v2 的目标是让空闲连接零 goroutine 开销。channel 模型需要至少一个 goroutine 做读写循环，无法满足这一要求。Pipeline 采用**纯函数调用链**模型，事件触发时沿链顺序调用 Handler，无需常驻 goroutine。

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
eventloop 读数据 → ByteBuf → Head → FrameCodec → HeartbeatHandler → BizHandler → Tail
```

- 遍历方向：**从头到尾**（Head → Tail）
- 触发方法：`ctx.FireChannelRead(msg)`

### Outbound 链（业务 → 网络）

```
BizHandler.Write → FrameCodec → Head.Write → eventloop 写数据
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
    head *handlerContext  // 哨兵头节点
    tail *handlerContext  // 哨兵尾节点
    ctxs map[string]*handlerContext // name → context 索引
}

type handlerContext struct {
    pipeline     *defaultPipeline
    name         string
    handler      ChannelHandler
    prev         *handlerContext
    next         *handlerContext
    nextInbound  atomic.Pointer[handlerContext] // 预缓存下一个 InboundHandler
    prevOutbound atomic.Pointer[handlerContext] // 预缓存上一个 OutboundHandler
}
```

使用**双向链表**存储 Handler，保证构建阶段的顺序性和 O(1) 的节点查找。运行时事件遍历不遍历链表，而是通过**预缓存的原子指针**直接跳转。

### 构建阶段（加锁）

```go
p.AddLast("frameDecoder", &FrameCodec{})
p.AddLast("biz", &BizHandler{})
p.AddLast("frameEncoder", &FrameCodec{})
```

- `AddFirst` / `AddLast` / `Remove` 使用 `sync.RWMutex` 保护
- 每次修改后调用 `rebuild()`，重新计算所有节点的 `nextInbound` / `prevOutbound`
- 运行时事件遍历（`FireChannelRead` 等）**不加锁**，通过原子指针读取
- 重复 name、nil handler、空 name 都会 `panic`，提前暴露配置错误

### 预编译 handler 数组（P3 优化 1.5）

**原始问题：** 每次事件遍历双向链表，O(N) 指针跳转，cache miss 严重。

**优化方案：** `rebuild()` 在 Add/Remove 时预计算每个节点的下一个 Inbound/Outbound Handler：

```go
func (p *defaultPipeline) rebuild() {
    var lastInbound *handlerContext
    for ctx := p.head.next; ctx != p.tail; ctx = ctx.next {
        if _, ok := ctx.handler.(InboundHandler); ok {
            if lastInbound != nil {
                lastInbound.nextInbound.Store(ctx)
            } else {
                p.head.nextInbound.Store(ctx)
            }
            lastInbound = ctx
        }
    }
    if lastInbound != nil {
        lastInbound.nextInbound.Store(nil)
    } else {
        p.head.nextInbound.Store(nil)
    }
    // outbound 同理...
}
```

**事件遍历（无锁 + O(1)）：**

```go
func (c *handlerContext) invokeChannelRead(msg interface{}) {
    next := c.nextInbound.Load() // 原子加载，O(1)
    if next != nil {
        next.handler.(InboundHandler).ChannelRead(next, msg)
    }
}
```

**收益：**
- 事件分发从链表遍历变为 O(1) 原子加载
- 零锁竞争（`rebuild` 在写锁中修改指针，事件遍历无锁读原子指针）
- 更好的 CPU cache locality（原子指针直接命中）

### 并发安全

`race_test.go` 验证以下并发场景：

```go
func TestRace(t *testing.T) {
    p := NewPipeline()
    h := &recorderInbound{testHandler: testHandler{name: "h"}}
    p.AddLast("h", h)

    // goroutine 1: 持续触发事件
    go func() {
        for i := 0; i < 1000; i++ {
            p.FireChannelRead(i)
        }
    }()

    // goroutine 2: 持续 Add/Remove
    go func() {
        for i := 0; i < 1000; i++ {
            p.Remove("h")
            p.AddLast("h", h)
        }
    }()
}
```

通过 `go test -race` 验证无 data race。

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

// FrameCodec：帧编解码 Handler（已内置在 conn 包）
type FrameCodec struct{}
func (h *FrameCodec) Name() string { return "codec" }
func (h *FrameCodec) ChannelRead(ctx pipeline.Context, msg interface{}) {
    fmt.Println("【FrameCodec】解码帧...")
    ctx.FireChannelRead(msg)
}
func (h *FrameCodec) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *FrameCodec) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *FrameCodec) ExceptionCaught(ctx pipeline.Context, err error) {}
func (h *FrameCodec) Write(ctx pipeline.Context, msg interface{}) {
    fmt.Println("【FrameCodec】编码帧...")
    ctx.FireChannelWrite(msg)
}
func (h *FrameCodec) Flush(ctx pipeline.Context) {}

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

func main() {
    p := pipeline.NewPipeline()

    // Inbound：按顺序添加
    p.AddLast("codec", &FrameCodec{})
    p.AddLast("biz", &BizHandler{})

    // Outbound：写数据时从尾到头遍历
    // FrameCodec 同时实现了 OutboundHandler，已在上面添加

    // 模拟收到消息
    p.FireChannelRead(&conn.Message{Type: 0x1, Data: []byte("hello")})

    // 模拟发送消息
    p.FireChannelWrite(&conn.Message{Type: 0x1, Data: []byte("world")})
}
```

**输出：**

```
【FrameCodec】解码帧...
【BizHandler】收到: hello
【FrameCodec】编码帧...
```

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `handler.go` | `ChannelPipeline`、`ChannelHandler`、`InboundHandler`、`OutboundHandler`、`Context` 接口 |
| `pipeline.go` | `defaultPipeline`、`handlerContext` 实现，预编译 rebuild，原子指针事件遍历 |
| `handler_test.go` | 接口合规性测试 |
| `pipeline_test.go` | Inbound/Outbound 链、Add/Remove、Active/Inactive、异常路径测试 |
| `race_test.go` | 并发竞争检测（Add/Remove + FireChannelRead 并发） |

---

## 注意事项

1. **Pipeline 构建阶段可修改，运行时事件遍历无锁** — `Add/Remove` 在 `mu` 保护下修改链表并调用 `rebuild`，运行时通过原子指针读取
2. **必须调用 FireChannelRead / FireChannelWrite** — 否则事件会"断"在当前 Handler
3. **InboundHandler 中调用 ctx.Write() 发送回复** — 这是从 Inbound 切换为 Outbound 的标准方式
4. **ExceptionCaught 尚未完全实现** — V2.1 会完善默认异常传播链（日志 → FireChannelInactive → 关闭连接）
5. **每个连接有独立的 Pipeline** — `server`/`client` 在创建 `netConn`/`epollConn` 时自动 `pipeline.NewPipeline()`
6. **预编译指针在 Add/Remove 后自动重建** — 无需手动调用 rebuild

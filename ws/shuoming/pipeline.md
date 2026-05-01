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

| 方法 | 说明 |
|---|---|
| `AddFirst(name, handler)` | 在链的头部（Head 之后）插入 Handler，返回 Pipeline 自身方便链式调用 |
| `AddLast(name, handler)` | 在链的尾部（Tail 之前）插入 Handler，返回 Pipeline 自身 |
| `Remove(name)` | 按 name 移除 Handler |
| `FireChannelRead(msg)` | 触发入站读事件，从 Head 开始沿链向 Tail 方向传播 |
| `FireChannelWrite(msg)` | 触发出站写事件，从 Tail 开始沿链向 Head 方向传播 |
| `FireChannelActive()` | 触发连接激活事件（连接建立时） |
| `FireChannelInactive()` | 触发连接断开事件 |
| `FireExceptionCaught(err)` | 触发异常事件 |

### ChannelHandler（基接口）

```go
type ChannelHandler interface {
    Name() string
}
```

- 所有 Handler 的基接口
- `Name()` 返回 Handler 在 Pipeline 中的唯一名称，用于查找和移除

### InboundHandler（入站处理器）

处理从网络读进来的数据（入站方向：Head → Tail）：

```go
type InboundHandler interface {
    ChannelHandler
    ChannelRead(ctx Context, msg interface{})
    ChannelActive(ctx Context)
    ChannelInactive(ctx Context)
    ExceptionCaught(ctx Context, err error)
}
```

| 方法 | 说明 |
|---|---|
| `ChannelRead(ctx, msg)` | 收到数据时调用。`msg` 是入站消息（如 `*conn.Message`）。必须调用 `ctx.FireChannelRead(msg)` 将事件传递给下一个 InboundHandler，否则事件会"断"在这里 |
| `ChannelActive(ctx)` | 连接建立时调用。必须调用 `ctx.FireChannelActive()` 传递 |
| `ChannelInactive(ctx)` | 连接断开时调用。必须调用 `ctx.FireChannelInactive()` 传递 |
| `ExceptionCaught(ctx, err)` | 发生异常时调用。必须调用 `ctx.FireExceptionCaught(err)` 传递 |

### OutboundHandler（出站处理器）

处理要发往网络的数据（出站方向：Tail → Head）：

```go
type OutboundHandler interface {
    ChannelHandler
    Write(ctx Context, msg interface{})
    Flush(ctx Context)
}
```

| 方法 | 说明 |
|---|---|
| `Write(ctx, msg)` | 写数据时调用。`msg` 是出站消息。Handler 可以修改消息、转换类型，然后调用 `ctx.Write(msg)` 或 `ctx.FireChannelWrite(msg)` 继续传递 |
| `Flush(ctx)` | 刷新缓冲区。当前实现中大多为空方法 |

### Context

Handler 在 Pipeline 中的执行上下文：

```go
type Context interface {
    Pipeline() ChannelPipeline
    FireChannelRead(msg interface{})   // 触发下一个 InboundHandler
    FireChannelWrite(msg interface{})  // 触发下一个 OutboundHandler（往回走）
    FireChannelActive()
    FireChannelInactive()
    FireExceptionCaught(err error)
    Write(msg interface{})             // 触发 Outbound 链（从当前位置往回走）
    Flush()
}
```

**关键区别：`Write` vs `FireChannelWrite`**

| 方法 | 遍历起点 | 用途 |
|---|---|---|
| `ctx.Write(msg)` | **当前 Handler 的 prevOutbound** 往回走 | 在 InboundHandler 中想发送回复时使用，从当前 Handler 位置出发 |
| `ctx.FireChannelWrite(msg)` | **当前 Handler 的 prevOutbound** 往回走 | 在 OutboundHandler 中完成处理后，将消息传递给前一个 OutboundHandler |

**注意**：在当前实现中，`ctx.Write` 和 `ctx.FireChannelWrite` 都调用同一个 `invokeChannelWrite` 方法，效果相同——都是通过 `prevOutbound` 找到前一个 OutboundHandler 并调用其 `Write` 方法。区别主要在语义上：`Write` 表示"我要发消息"，`FireChannelWrite` 表示"我处理完了，继续往下传"。

---

## 数据流向

### Inbound 链（网络 → 业务）

```
eventloop 读数据 → ByteBuf → Head → ConnWriter(跳过，非 InboundHandler) → FrameCodec(跳过，非 InboundHandler) → BizHandler → Tail
```

- 遍历方向：**从头到尾**（Head → Tail）
- 只有实现了 `InboundHandler` 的 Handler 会被入站事件访问
- ConnWriter 和 FrameCodec 只实现了 `OutboundHandler`，入站事件会跳过它们
- 触发方法：`ctx.FireChannelRead(msg)`

### Outbound 链（业务 → 网络）

当业务 Handler（如 EchoHandler）调用 `ctx.Write(msg)` 时：

```
BizHandler.ctx.Write(msg)
  → prevOutbound 找到 FrameCodec
  → FrameCodec.Write(msg → Frame → ByteBuf → ctx.Write(bb))
    → prevOutbound 找到 ConnWriter
    → ConnWriter.Write(bb → Conn.Write(bb))
      → 数据写入网络
```

- 遍历方向：**从尾到头**（Tail → Head）
- 触发方法：`ctx.Write(msg)` 或 `ctx.FireChannelWrite(msg)`
- 每个 Handler 的 `prevOutbound` 指针指向它左边最近的 OutboundHandler

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
p.AddFirst("headWriter", &ConnWriter{Conn: c})
p.AddLast("frameCodec", &FrameCodec{IsClient: false})
p.AddLast("biz", &EchoHandler{})
```

- `AddFirst` / `AddLast` / `Remove` 使用 `sync.RWMutex` 保护
- 每次修改后调用 `rebuild()`，重新计算所有节点的 `nextInbound` / `prevOutbound`
- 运行时事件遍历（`FireChannelRead` 等）**不加锁**，通过原子指针读取
- 重复 name、nil handler、空 name 都会 `panic`，提前暴露配置错误

---

## prevOutbound 全 Handler 机制（关键变更）

### 最重要的变更

**每个 Handler（不仅仅是 OutboundHandler）都会获得一个 `prevOutbound` 指针。**

这是 Pipeline 出站机制最关键的设计。如果只有 OutboundHandler 才有 `prevOutbound`，那么非 Outbound 类型的 Handler（如纯 InboundHandler 的业务 Handler）调用 `ctx.Write(msg)` 时，`prevOutbound` 为 nil，消息会**静默丢失**，没有任何错误提示。

### rebuild 算法

```go
func (p *defaultPipeline) rebuild() {
    // ---- Inbound 链预计算 ----
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

    // ---- Outbound 链预计算 ----
    var lastOutbound *handlerContext
    for ctx := p.head.next; ctx != p.tail; ctx = ctx.next {
        ctx.prevOutbound.Store(lastOutbound)   // 每个 Handler 都设置 prevOutbound！
        if _, ok := ctx.handler.(OutboundHandler); ok {
            lastOutbound = ctx
        }
    }
    p.tail.prevOutbound.Store(lastOutbound)    // Tail 也设置
}
```

**算法详解**：

1. 从 Head 的下一个节点开始，沿链向 Tail 方向遍历
2. 对每个节点，将其 `prevOutbound` 设为 `lastOutbound`（当前遍历到的、最近的 OutboundHandler）
3. 如果当前节点本身就是 OutboundHandler，更新 `lastOutbound` 为当前节点
4. 最后将 Tail 的 `prevOutbound` 也设好

**这意味着**：
- 每个 Handler 的 `prevOutbound` 指向它**左边（靠近 Head 方向）最近的 OutboundHandler**
- 非OutboundHandler 的 `prevOutbound` 不是 nil，而是指向左边最近的 OutboundHandler
- 当任何 Handler 调用 `ctx.Write(msg)` 时，`invokeChannelWrite` 加载 `prevOutbound`，委托给那个 OutboundHandler

### invokeChannelWrite

```go
func (c *handlerContext) invokeChannelWrite(msg interface{}) {
    prev := c.prevOutbound.Load()   // 原子加载 prevOutbound
    if prev != nil {
        prev.handler.(OutboundHandler).Write(prev, msg)
    }
}
```

- 通过 `prevOutbound` 原子指针直接找到前一个 OutboundHandler，O(1) 跳转
- 无需遍历链表，无需加锁
- 如果 `prevOutbound` 为 nil（没有 OutboundHandler），消息被静默丢弃

### 具体示例

假设 Pipeline 的布局为：

```
[Head] → [ConnWriter(Outbound)] → [FrameCodec(Outbound)] → [EchoHandler(Inbound-only)] → [Tail]
```

rebuild 后各节点的 `prevOutbound`：

| 节点 | prevOutbound | 原因 |
|---|---|---|
| ConnWriter | nil | 它左边（Head 方向）没有 OutboundHandler |
| FrameCodec | ConnWriter | 它左边最近的 OutboundHandler 是 ConnWriter |
| EchoHandler | FrameCodec | 它左边最近的 OutboundHandler 是 FrameCodec |
| Tail | FrameCodec | 它左边最近的 OutboundHandler 也是 FrameCodec |

当 EchoHandler 收到消息后调用 `ctx.Write(msg)` 的完整流程：

1. `EchoHandler.ctx.Write(msg)` → `invokeChannelWrite(msg)`
2. 加载 `EchoHandler.prevOutbound` → 得到 `FrameCodec`
3. 调用 `FrameCodec.Write(ctx, msg)` — 编码 Message → Frame → ByteBuf
4. `FrameCodec` 内部调用 `ctx.Write(bb)` → `invokeChannelWrite(bb)`
5. 加载 `FrameCodec.prevOutbound` → 得到 `ConnWriter`
6. 调用 `ConnWriter.Write(ctx, bb)` — 将 ByteBuf 写入 `Conn.Write(bb)`
7. 数据写入网络

**如果没有 prevOutbound 全 Handler 机制**：

EchoHandler 是纯 InboundHandler，如果只有 OutboundHandler 才有 prevOutbound，那么 EchoHandler 的 prevOutbound 为 nil。当 EchoHandler 调用 `ctx.Write(msg)` 时，`invokeChannelWrite` 发现 `prevOutbound` 为 nil，消息被静默丢弃——业务代码写了回复，但实际上什么都没发出去。

---

## ConnWriter 在 Pipeline 中的角色

### 自动添加

server/client 在创建连接后，自动将 ConnWriter 作为第一个 Handler 添加到 Pipeline：

```go
Pipeline.AddFirst("headWriter", &ConnWriter{Conn: c})
```

### 为什么 ConnWriter 必须存在？

ConnWriter 是 Pipeline 出站链的**终点**（从 Tail 往 Head 方向看，它是第一个 OutboundHandler）。它的职责是将 ByteBuf（最终出站产物）转换为实际的 `Conn.Write()` 调用，将数据写入网络。

**没有 ConnWriter 的后果**：

假设 Pipeline 只有 `[FrameCodec → BizHandler]`，当 BizHandler 调用 `ctx.Write(msg)`：

1. `FrameCodec.Write` 将 Message 编码为 ByteBuf
2. `FrameCodec` 调用 `ctx.Write(bb)`
3. `FrameCodec.prevOutbound` 为 nil（左边没有 OutboundHandler）
4. ByteBuf 被静默丢弃

所以 ConnWriter 是必不可少的，它是出站链的"出口"。

### ConnWriter 代码

```go
type ConnWriter struct {
    Conn Conn
}

func (cw *ConnWriter) Name() string { return "headWriter" }

func (cw *ConnWriter) Write(ctx pipeline.Context, msg interface{}) {
    if bb, ok := msg.(buf.ByteBuf); ok {
        _ = cw.Conn.Write(bb)   // 将 ByteBuf 写入底层连接
        return
    }
    ctx.FireChannelWrite(msg)   // 非 ByteBuf 类型继续传递
}

func (cw *ConnWriter) Flush(ctx pipeline.Context) {}
```

- ConnWriter 只实现了 `OutboundHandler`，没有实现 `InboundHandler`
- 入站事件会跳过 ConnWriter（`nextInbound` 不指向它）
- 出站事件：ConnWriter 检查消息类型，如果是 ByteBuf 就写入连接，否则继续传递

---

## 预编译 Handler 指针

### 原始问题

每次事件遍历双向链表，O(N) 指针跳转，cache miss 严重。

### 优化方案

`rebuild()` 在 Add/Remove 时预计算每个节点的下一个 Inbound/Outbound Handler，存储为原子指针。

**Inbound 链（nextInbound）**：

- 只有 OutboundHandler 的节点不会被 `nextInbound` 链包含
- 例如：`[Head] → ConnWriter(Outbound) → FrameCodec(Outbound) → BizHandler(Inbound) → [Tail]`
- Head.nextInbound → BizHandler（跳过了 ConnWriter 和 FrameCodec）

**Outbound 链（prevOutbound）**：

- 每个 Handler（无论是否为 OutboundHandler）都有 `prevOutbound`
- 指向左边最近的 OutboundHandler

**事件遍历（无锁 + O(1)）**：

```go
// Inbound
func (c *handlerContext) invokeChannelRead(msg interface{}) {
    next := c.nextInbound.Load() // 原子加载，O(1)
    if next != nil {
        next.handler.(InboundHandler).ChannelRead(next, msg)
    }
}

// Outbound
func (c *handlerContext) invokeChannelWrite(msg interface{}) {
    prev := c.prevOutbound.Load() // 原子加载，O(1)
    if prev != nil {
        prev.handler.(OutboundHandler).Write(prev, msg)
    }
}
```

**收益**：

- 事件分发从链表遍历变为 O(1) 原子加载
- 零锁竞争（`rebuild` 在写锁中修改指针，事件遍历无锁读原子指针）
- 更好的 CPU cache locality（原子指针直接命中）

---

## 并发安全

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

核心保证：
- `AddFirst` / `AddLast` / `Remove` 在写锁内修改链表并调用 `rebuild`
- `rebuild` 修改原子指针，原子操作保证可见性
- 事件遍历通过 `atomic.Pointer.Load` 读取，无锁

---

## 关键区别：Write vs FireChannelWrite

| 方法 | 语义 | 内部实现 |
|---|---|---|
| `ctx.Write(msg)` | "我要发消息" — 从当前 Handler 位置出发 | 调用 `invokeChannelWrite(msg)`，通过 `prevOutbound` 找到前一个 OutboundHandler |
| `ctx.FireChannelWrite(msg)` | "我处理完了，继续传递" — 从当前 Handler 位置出发 | 同样调用 `invokeChannelWrite(msg)` |

**注意**：在当前实现中，两者的内部实现完全相同（都调用 `invokeChannelWrite`），但语义不同：

- `ctx.Write(msg)` 通常在 InboundHandler 中使用——收到消息后需要回复
- `ctx.FireChannelWrite(msg)` 通常在 OutboundHandler 中使用——完成编码/转换后，将结果传递给下一个 OutboundHandler

---

## 完整示例：ConnWriter + FrameCodec + BizHandler

```go
package main

import (
    "fmt"
    "github.com/lufeijun/goTools/ws/buf"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/frame"
    "github.com/lufeijun/goTools/ws/pipeline"
)

// ---------- ConnWriter：出站链的头，将 ByteBuf 写入网络 ----------

type DemoConnWriter struct {
    conn conn.Conn
}

func (cw *DemoConnWriter) Name() string { return "headWriter" }
func (cw *DemoConnWriter) Write(ctx pipeline.Context, msg interface{}) {
    if bb, ok := msg.(buf.ByteBuf); ok {
        _ = cw.conn.Write(bb)
        return
    }
    ctx.FireChannelWrite(msg)
}
func (cw *DemoConnWriter) Flush(ctx pipeline.Context) {}

// ---------- FrameCodec：帧编解码 Handler ----------

type DemoFrameCodec struct {
    IsClient bool
}

func (fc *DemoFrameCodec) Name() string { return "frameCodec" }
func (fc *DemoFrameCodec) Write(ctx pipeline.Context, msg interface{}) {
    m, ok := msg.(*conn.Message)
    if !ok {
        ctx.FireChannelWrite(msg)
        return
    }
    f := frame.Frame{
        FIN:     true,
        Opcode:  frame.Opcode(m.Type),
        Payload: m.Data,
        Masked:  fc.IsClient,
    }
    if fc.IsClient {
        f.MaskKey = frame.GenerateMaskKey()
    }
    size := 14 + len(m.Data)
    bb := buf.NewByteBuf(size)
    _ = frame.WriteFrameTo(bb, f)
    ctx.Write(bb)  // ByteBuf 继续传递给前一个 OutboundHandler（ConnWriter）
}
func (fc *DemoFrameCodec) Flush(ctx pipeline.Context) {}

// ---------- EchoHandler：业务 Handler（纯 InboundHandler）----------

type EchoHandler struct{}

func (h *EchoHandler) Name() string { return "echo" }
func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*conn.Message); ok {
        fmt.Printf("【EchoHandler】收到: %s\n", string(m.Data))
        // 回复：从 EchoHandler 的位置出发，通过 prevOutbound 找到 FrameCodec
        ctx.Write(&conn.Message{Type: m.Type, Data: m.Data})
    }
    ctx.FireChannelRead(msg)
}
func (h *EchoHandler) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *EchoHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *EchoHandler) ExceptionCaught(ctx pipeline.Context, err error) {
    ctx.FireExceptionCaught(err)
}

// ---------- 组装 Pipeline ----------

func main() {
    p := pipeline.NewPipeline()

    // 添加顺序很重要！
    // ConnWriter 在最前面（Head 之后），FrameCodec 在中间，EchoHandler 在最后
    p.AddFirst("headWriter", &DemoConnWriter{})  // OutboundHandler
    p.AddLast("frameCodec", &DemoFrameCodec{})    // OutboundHandler
    p.AddLast("echo", &EchoHandler{})             // InboundHandler

    // Pipeline 布局：
    // [Head] → ConnWriter(Outbound) → FrameCodec(Outbound) → EchoHandler(Inbound) → [Tail]

    // rebuild 后各节点的 prevOutbound：
    // ConnWriter.prevOutbound = nil
    // FrameCodec.prevOutbound = ConnWriter
    // EchoHandler.prevOutbound = FrameCodec

    // 模拟入站消息
    p.FireChannelRead(&conn.Message{Type: 0x1, Data: []byte("hello")})
    // 入站路径：Head → 跳过 ConnWriter(非 InboundHandler) → 跳过 FrameCodec(非 InboundHandler)
    //         → EchoHandler.ChannelRead → Tail

    // 出站路径（EchoHandler 中调用 ctx.Write）：
    // EchoHandler.ctx.Write(msg) → FrameCodec.Write(msg→ByteBuf) → ConnWriter.Write(ByteBuf→网络)
}
```

**出站数据流详解**：

```
EchoHandler.ctx.Write(&conn.Message{...})
  │
  ▼ invokeChannelWrite: 加载 EchoHandler.prevOutbound → FrameCodec
FrameCodec.Write(ctx, &conn.Message{...})
  │  编码: Message → Frame → ByteBuf
  │  调用 ctx.Write(bb)
  ▼ invokeChannelWrite: 加载 FrameCodec.prevOutbound → ConnWriter
ConnWriter.Write(ctx, bb)
  │  类型匹配: bb 是 buf.ByteBuf
  │  调用 Conn.Write(bb)
  ▼
数据写入网络
```

---

## 文件清单

| 文件 | 内容 |
|---|---|
| `handler.go` | `ChannelPipeline`、`ChannelHandler`、`InboundHandler`、`OutboundHandler`、`Context` 接口定义 |
| `pipeline.go` | `defaultPipeline`、`handlerContext` 实现，`rebuild` 预编译算法，原子指针事件遍历，`AddFirst`/`AddLast`/`Remove`，`FireChannelRead`/`FireChannelWrite` 等 |
| `handler_test.go` | 接口合规性测试 |
| `pipeline_test.go` | Inbound/Outbound 链、Add/Remove、Active/Inactive、异常路径测试 |
| `race_test.go` | 并发竞争检测（Add/Remove + FireChannelRead 并发） |

---

## 注意事项

1. **Pipeline 构建阶段可修改，运行时事件遍历无锁** — `Add/Remove` 在 `mu` 保护下修改链表并调用 `rebuild`，运行时通过原子指针读取
2. **必须调用 FireChannelRead / FireChannelWrite** — 否则事件会"断"在当前 Handler
3. **每个 Handler 都有 prevOutbound** — 非OutboundHandler 的 `prevOutbound` 指向左边最近的 OutboundHandler，这使得 InboundHandler 中调用 `ctx.Write()` 可以正确找到出站链
4. **InboundHandler 中调用 ctx.Write() 发送回复** — 这是从 Inbound 切换为 Outbound 的标准方式，`prevOutbound` 机制确保消息能正确到达出站链
5. **ConnWriter 是 Pipeline 的出口** — 没有它，ByteBuf 无处可去。server/client 会自动添加
6. **ExceptionCaught 尚未完全实现** — V2.1 会完善默认异常传播链（日志 → FireChannelInactive → 关闭连接）
7. **每个连接有独立的 Pipeline** — `server`/`client` 在创建 `netConn`/`epollConn` 时自动 `pipeline.NewPipeline()`
8. **预编译指针在 Add/Remove 后自动重建** — 无需手动调用 rebuild
9. **重复 name 会 panic** — 这是设计决策，提前暴露配置错误，避免运行时难以排查的问题

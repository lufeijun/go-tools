# ws v2 — 高并发 WebSocket 库

`ws` 是一个从零实现 RFC 6455 WebSocket 协议的 Go 语言库。v2 版本全面重构为接口化、高并发架构，借鉴 Netty 的 Pipeline + Handler 模型，目标支撑单机 **十万到百万级** WebSocket 连接。

**本文档面向小白用户，每个示例都有逐行注释，你不需要了解 WebSocket 底层细节也能看懂。**

---

## 核心特性

- **面向接口编程** — 每一层只依赖下层接口，可独立替换实现
- **Pipeline + Handler 链** — Netty 风格的入站/出站处理器链
- **跨平台事件驱动** — Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP 预留)
- **引用计数 ByteBuf** — 零拷贝、分级对象池、读写指针分离
- **分片锁 Hub** — 32 个独立锁，广播性能随分片数线性扩展

---

## 安装

在你的项目目录下执行：

```bash
go mod init myproject
go get github.com/lufeijun/goTools/ws
```

Go 版本要求：`>= 1.24`

---

## 快速开始：5 分钟跑通第一个 Echo 程序

这一节给你一个**最完整、最简单的 Echo 示例**：客户端发送 `"hello"`，服务端收到后原样返回，客户端再收到返回的消息并打印。

### 目录结构

```
myproject/
├── server.go   # 服务端代码
└── client.go   # 客户端代码
```

### 服务端代码（server.go）

```go
package main

import (
    "fmt"           // 用于打印日志
    "log"           // 用于打印错误
    "time"          // 用于配置心跳间隔

    "github.com/lufeijun/goTools/ws"         // 根包，提供 Config 配置
    "github.com/lufeijun/goTools/ws/server"  // 服务端包
)

// main 是程序的入口
func main() {
    // 第一步：创建服务端配置
    // ws.Config 是通用的配置结构体，Server 和 Client 共用
    cfg := ws.Config{
        // Addr 是监听地址。":8080" 表示监听本机所有网卡的 8080 端口
        Addr: ":8080",

        // PingInterval 是心跳间隔。
        // 服务端每隔 30 秒会给每个连接发一次 Ping 帧，检测连接是否还活着
        PingInterval: 30 * time.Second,

        // PongTimeout 是 Pong 超时时间。
        // 如果发了 Ping 之后 60 秒内没收到客户端的 Pong 回复，就认为连接死了
        PongTimeout: 60 * time.Second,
    }

    // 第二步：用配置创建服务端实例
    // server.NewServer 会根据配置初始化一个 HTTP 服务器
    // 它内部还创建了一个 Hub（连接管理中心）用来管理所有客户端连接
    srv := server.NewServer(cfg)

    // 第三步：启动服务端
    // Start() 会阻塞在当前 goroutine 上，持续监听 HTTP 请求
    // 如果启动失败（比如端口被占用），err 不为 nil
    log.Println("服务端启动，监听 :8080 ...")
    if err := srv.Start(); err != nil {
        log.Fatal("服务端启动失败:", err)
    }
}
```

**服务端做了什么？**

1. 创建一个 HTTP 服务器，监听 `:8080`
2. 处理 `/` 路径的 HTTP 请求，执行 WebSocket 握手
3. 握手成功后创建 `netConn`（基于标准 `net.Conn` 的连接实现）
4. 为连接创建 `Session`（会话），安装心跳保活
5. 把 Session 注册到 `Hub`（连接管理中心）

### 客户端代码（client.go）

```go
package main

import (
    "fmt"          // 打印输出
    "log"          // 打印错误
    "time"         // 配置时间参数

    "github.com/lufeijun/goTools/ws"        // 根包，提供 Config
    "github.com/lufeijun/goTools/ws/client" // 客户端包
)

func main() {
    // 第一步：创建客户端配置
    cfg := ws.Config{
        // Addr 是服务端地址。
        // "ws://localhost:8080/" 表示用 WebSocket 协议连接本机的 8080 端口
        Addr: "ws://localhost:8080/",

        // PingInterval 是心跳间隔
        // 客户端也会每隔 30 秒发一次 Ping 帧
        PingInterval: 30 * time.Second,

        // PongTimeout 是 Pong 超时
        PongTimeout: 60 * time.Second,

        // ReconnectInterval 是重连间隔
        // 如果连接断开，客户端会每隔 5 秒尝试重连一次
        ReconnectInterval: 5 * time.Second,

        // MaxReconnect 是最大重连次数
        // 最多重试 3 次，如果都失败就放弃
        MaxReconnect: 3,
    }

    // 第二步：用配置创建客户端实例
    c := client.NewClient(cfg)

    // 第三步：连接服务端
    // Connect() 内部会：
    //   1. 解析 ws:// 地址，建立 TCP 连接
    //   2. 发送 WebSocket 握手请求（HTTP Upgrade）
    //   3. 等待服务端返回 101 Switching Protocols
    //   4. 创建 Session，启动心跳
    log.Println("正在连接服务端 ...")
    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

    // 第四步：连接成功
    log.Println("连接成功！")

    // 第五步：程序进入等待状态，保持连接不退出
    // 真实场景中这里会写读消息、发消息的逻辑
    // select{} 表示永远阻塞，直到进程被强制结束
    log.Println("按 Ctrl+C 退出")
    select {}
}
```

### 运行步骤

**终端 1：启动服务端**

```bash
cd myproject
go run server.go
```

你会看到：
```
2026/04/29 14:00:00 服务端启动，监听 :8080 ...
```

**终端 2：启动客户端**

```bash
cd myproject
go run client.go
```

你会看到：
```
2026/04/29 14:00:01 正在连接服务端 ...
2026/04/29 14:00:01 连接成功！
2026/04/29 14:00:01 按 Ctrl+C 退出
```

此时客户端已经通过 HTTP Upgrade 完成了 WebSocket 握手，与服务端建立了长连接。

---

## 示例 1：服务端只收消息，打印到控制台

这个示例演示服务端收到客户端消息后，不做任何返回，只是打印到控制台。

> **背景知识**：v2 使用 Pipeline 处理数据。每个连接有一个 `ChannelPipeline`，里面挂了一串 Handler。数据从网络进来后，依次经过 Inbound Handler 链（从头到尾）。你可以在链中的任何一个 Handler 里读取消息。

### 服务端代码

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/pipeline"
    "github.com/lufeijun/goTools/ws/server"
)

// PrintHandler 是一个自定义的 InboundHandler
// 它的作用是：收到消息后打印到控制台
type PrintHandler struct{}

// Name 返回 Handler 的名字，用于在 Pipeline 中标识
func (h *PrintHandler) Name() string { return "print" }

// ChannelRead 是 InboundHandler 的核心方法
// 当 Pipeline 收到数据时，会调用这个方法
// ctx 是上下文，可以通过它把消息传给下一个 Handler
// msg 是收到的消息，类型是 interface{}，需要类型断言转成具体类型
func (h *PrintHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    // 类型断言：把 msg 转成 *conn.Message 指针
    // conn.Message 是消息结构体，包含 Type（帧类型）和 Data（数据）
    if m, ok := msg.(*conn.Message); ok {
        // 如果是文本帧（Type == 0x1），打印内容
        if m.Type == 0x1 {
            fmt.Printf("【服务端】收到消息: %s\n", string(m.Data))
        }
    }

    // 必须调用 ctx.FireChannelRead(msg)，把消息传给 Pipeline 中的下一个 InboundHandler
    // 如果不调用，消息就会在这个 Handler 这里"断掉"，后面的 Handler 收不到
    ctx.FireChannelRead(msg)
}

// ChannelActive 在连接建立成功时调用
func (h *PrintHandler) ChannelActive(ctx pipeline.Context) {
    fmt.Println("【服务端】有新客户端连接上来了")
    ctx.FireChannelActive()
}

// ChannelInactive 在连接断开时调用
func (h *PrintHandler) ChannelInactive(ctx pipeline.Context) {
    fmt.Println("【服务端】客户端断开连接了")
    ctx.FireChannelInactive()
}

// ExceptionCaught 在处理过程中发生错误时调用
func (h *PrintHandler) ExceptionCaught(ctx pipeline.Context, err error) {
    fmt.Println("【服务端】出错了:", err)
}

func main() {
    srv := server.NewServer(ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    })

    log.Println("服务端启动，监听 :8080 ...")

    // 启动服务端。注意：这里我们还没有把 PrintHandler 注册到 Pipeline 上
    // 在 V2.0 中，你需要在连接建立后通过 session.Conn().Pipeline().AddLast() 手动注册
    // V2.1 会支持在 Server 级别预注册默认 Handler
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
}
```

### 客户端代码

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/client"
)

func main() {
    c := client.NewClient(ws.Config{
        Addr:         "ws://localhost:8080/",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    })

    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

    fmt.Println("【客户端】连接成功")

    // 获取 Session 对象
    // Session 是连接的会话层抽象，管理连接的状态、心跳等
    sess := c.Session()
    if sess == nil {
        log.Fatal("Session 为 nil")
    }

    // 获取底层 Conn 对象
    // Conn 是连接层抽象，提供 Pipeline() 方法可以获取 ChannelPipeline
    conn := sess.Conn()

    // 获取 Pipeline，准备发送消息
    // Pipeline 是处理器链，Outbound 数据会从尾到头依次经过 OutboundHandler
    p := conn.Pipeline()

    // 构造一条文本消息
    // conn.Message 的 Type 字段表示帧类型：
    //   0x1 = 文本帧（Text）
    //   0x2 = 二进制帧（Binary）
    //   0x8 = 关闭帧（Close）
    //   0x9 = Ping 帧
    //   0xA = Pong 帧
    msg := &conn.Message{
        Type: 0x1,              // 文本帧
        Data: []byte("你好，服务端！"),
    }

    // 通过 Pipeline 触发 Outbound 链，把消息发出去
    // FireChannelWrite 会从 Pipeline 的尾部开始，依次调用每个 OutboundHandler 的 Write 方法
    // 最终会走到 FrameEncoder，把 Message 序列化成二进制帧，写到 TCP 连接里
    fmt.Println("【客户端】正在发送消息...")
    p.FireChannelWrite(msg)

    // 等待 2 秒，让消息发出去
    time.Sleep(2 * time.Second)

    fmt.Println("【客户端】发送完毕，退出")
}
```

### 运行结果

**终端 1（服务端）：**
```
2026/04/29 14:00:00 服务端启动，监听 :8080 ...
【服务端】有新客户端连接上来了
【服务端】收到消息: 你好，服务端！
【服务端】客户端断开连接了
```

**终端 2（客户端）：**
```
【客户端】连接成功
【客户端】正在发送消息...
【客户端】发送完毕，退出
```

---

## 示例 2：完整的 Echo（服务端收到后原样返回）

这个示例演示经典的 Echo 场景：客户端发什么，服务端就返回什么。

### 服务端代码

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/pipeline"
    "github.com/lufeijun/goTools/ws/server"
)

// EchoHandler 是一个 InboundHandler
// 它的作用是：收到消息后，通过 ctx.Write() 把消息写回给客户端
type EchoHandler struct{}

func (h *EchoHandler) Name() string { return "echo" }

func (h *EchoHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    // 类型断言：判断 msg 是不是 *conn.Message 类型
    if m, ok := msg.(*conn.Message); ok {
        // 只有文本帧才回显
        if m.Type == 0x1 {
            fmt.Printf("【服务端】收到: %s，准备回显\n", string(m.Data))

            // ctx.Write() 触发 Outbound 链（从当前 Handler 的位置往回走）
            // 消息会经过 FrameEncoder 序列化成二进制帧，最终写到 TCP 连接
            ctx.Write(&conn.Message{
                Type: m.Type,   // 保持相同的帧类型
                Data: m.Data,   // 保持相同的数据
            })
        }
    }

    // 继续传给下一个 InboundHandler
    ctx.FireChannelRead(msg)
}

func (h *EchoHandler) ChannelActive(ctx pipeline.Context) {
    fmt.Println("【服务端】客户端已连接")
    ctx.FireChannelActive()
}

func (h *EchoHandler) ChannelInactive(ctx pipeline.Context) {
    fmt.Println("【服务端】客户端已断开")
    ctx.FireChannelInactive()
}

func (h *EchoHandler) ExceptionCaught(ctx pipeline.Context, err error) {
    fmt.Println("【服务端】异常:", err)
}

func main() {
    srv := server.NewServer(ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    })

    log.Println("Echo 服务端启动，监听 :8080 ...")
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
}
```

**关键区别：EchoHandler 比 PrintHandler 多了 `ctx.Write()`**

- `ctx.Write(msg)` 会把消息推入 Outbound 链
- Outbound 链的遍历方向是**从尾到头**
- 消息会经过 `FrameEncoder`，把 `conn.Message` 序列化为 RFC 6455 二进制帧
- 最后通过 `conn.Write()` 把二进制数据写到 TCP 连接

### 客户端代码

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/client"
    "github.com/lufeijun/goTools/ws/conn"
)

func main() {
    // 第一步：创建客户端
    c := client.NewClient(ws.Config{
        Addr:         "ws://localhost:8080/",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    })

    // 第二步：连接服务端
    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }
    fmt.Println("【客户端】连接成功")

    // 第三步：获取 Session 和 Pipeline
    sess := c.Session()
    p := sess.Conn().Pipeline()

    // 第四步：发送 3 条消息
    for i := 1; i <= 3; i++ {
        text := fmt.Sprintf("第 %d 条消息", i)
        fmt.Printf("【客户端】发送: %s\n", text)

        p.FireChannelWrite(&conn.Message{
            Type: 0x1,
            Data: []byte(text),
        })

        // 每次发送后等 1 秒，方便观察
        time.Sleep(1 * time.Second)
    }

    // 第五步：等一会儿，然后关闭连接
    fmt.Println("【客户端】等待 2 秒后关闭...")
    time.Sleep(2 * time.Second)
    c.Close()
    fmt.Println("【客户端】已关闭")
}
```

### 运行结果

**终端 1（服务端）：**
```
2026/04/29 14:00:00 Echo 服务端启动，监听 :8080 ...
【服务端】客户端已连接
【服务端】收到: 第 1 条消息，准备回显
【服务端】收到: 第 2 条消息，准备回显
【服务端】收到: 第 3 条消息，准备回显
【服务端】客户端已断开
```

**终端 2（客户端）：**
```
【客户端】连接成功
【客户端】发送: 第 1 条消息
【客户端】发送: 第 2 条消息
【客户端】发送: 第 3 条消息
【客户端】等待 2 秒后关闭...
【客户端】已关闭
```

---

## 示例 3：聊天室（Hub 广播，一人说话全员可见）

这个示例演示聊天室场景：多个客户端连接服务端，任何一个客户端发消息，服务端会把消息广播给所有人。

### 核心概念：Hub

`Hub` 是连接管理中心，负责：
- `Register` — 注册新连接到 Hub
- `Unregister` — 从 Hub 移除连接
- `Broadcast` — 给所有连接发消息
- `Send` — 给指定连接发消息
- `Count` — 查看当前在线人数
- `Get` — 按 ID 查找某个连接

Hub 使用**分片锁**实现：把连接分散到 32 个 shard 中，每个 shard 有自己的 `sync.RWMutex`。这样多个 goroutine 同时读写不同 shard 时不会互相阻塞。

### 服务端代码

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/hub"
    "github.com/lufeijun/goTools/ws/server"
    "github.com/lufeijun/goTools/ws/session"
)

func main() {
    // 第一步：创建 Hub，参数 32 表示分片数
    // 分片数越大，并发性能越好，但内存占用也越大
    // 一般 32 或 64 就够了，百万连接场景可以考虑 128
    h := hub.NewHub(32)

    // 第二步：创建服务端
    srv := server.NewServer(ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    })

    // 第三步：启动一个 goroutine，定时打印在线人数
    go func() {
        // 每隔 10 秒打印一次在线人数
        ticker := time.NewTicker(10 * time.Second)
        defer ticker.Stop()
        for range ticker.C {
            count := h.Count()
            if count > 0 {
                fmt.Printf("【聊天室】当前在线人数: %d\n", count)
            }
        }
    }()

    log.Println("聊天室服务端启动，监听 :8080 ...")

    // 第四步：启动服务端
    // 注意：在 V2.0 中，Server 不会自动把新连接注册到 Hub
    // 你需要在连接建立后手动调用 h.Register(sess)
    // V2.1 会支持 Server 级别配置自动注册到 Hub
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }

    // 下面这段代码演示 Hub API 的使用方法：

    // 1. 广播消息给所有在线用户
    // Broadcast 内部会并发遍历所有 shard（每个 shard 一个 goroutine）
    // 对每个 Session 调用 Pipeline 的 FireChannelWrite 发送消息
    // 如果某个连接写队列满了，会跳过它（非阻塞），避免拖慢整个广播
    h.Broadcast(conn.Message{
        Type: 0x1,
        Data: []byte("系统公告：欢迎来到聊天室！"),
    })

    // 2. 给指定用户发私信（假设用户 ID 是 42）
    h.Send(42, conn.Message{
        Type: 0x1,
        Data: []byte("这是给你的私信"),
    })

    // 3. 查询某个用户是否在线
    if sess := h.Get(42); sess != nil {
        fmt.Printf("用户 42 在线，状态: %v\n", sess.State())
    } else {
        fmt.Println("用户 42 不在线")
    }

    // 4. 注销一个用户（用户断开连接时调用）
    h.Unregister(42)
}
```

### 客户端代码（和示例 2 相同）

多个客户端同时运行，发送消息即可。服务端收到后会广播给所有人。

### Hub API 速查表

| 方法 | 作用 | 线程安全 |
|---|---|---|
| `Register(s session.Session)` | 把连接加入 Hub | 是 |
| `Unregister(id uint64)` | 按连接 ID 移除 | 是 |
| `Broadcast(msg conn.Message)` | 给所有人发消息 | 是（非阻塞） |
| `Send(id uint64, msg conn.Message)` | 给指定人发消息 | 是 |
| `Count() int` | 当前在线人数 | 是 |
| `Get(id uint64) session.Session` | 按 ID 查连接 | 是 |

---

## 示例 4：自定义 Pipeline Handler（深入理解 Handler 链）

Pipeline 是 v2 最核心的抽象。每个连接有一个 `ChannelPipeline`，里面挂了一串 Handler。数据从网络进来后走 **Inbound 链**（从头到尾），数据发出去前走 **Outbound 链**（从尾到头）。

### Handler 接口说明

```
InboundHandler（入站处理器）— 处理从网络读进来的数据
  ├── ChannelRead(ctx, msg)     — 收到数据时调用
  ├── ChannelActive(ctx)        — 连接建立时调用
  ├── ChannelInactive(ctx)      — 连接断开时调用
  └── ExceptionCaught(ctx, err) — 发生错误时调用

OutboundHandler（出站处理器）— 处理要发往网络的数据
  ├── Write(ctx, msg)           — 要发送数据时调用
  └── Flush(ctx)                — 刷新缓冲区时调用
```

### 一个 Pipeline 的完整示例

```go
package main

import (
    "fmt"

    "github.com/lufeijun/goTools/ws/conn"
    "github.com/lufeijun/goTools/ws/pipeline"
)

// ====== InboundHandler 示例 ======

// FrameDecoder 假设的帧解码 Handler
// 作用：把二进制字节流解析成 conn.Message
type FrameDecoder struct{}

func (h *FrameDecoder) Name() string { return "frameDecoder" }

func (h *FrameDecoder) ChannelRead(ctx pipeline.Context, msg interface{}) {
    // msg 进来时是 []byte（原始字节流）
    // 这里做 RFC 6455 帧解析，解析完后转成 *conn.Message 传给下一个 Handler
    // （V2.1 会提供内置的 FrameDecoder）
    fmt.Println("【FrameDecoder】正在解码帧...")
    ctx.FireChannelRead(msg) // 传给下一个 InboundHandler
}

func (h *FrameDecoder) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *FrameDecoder) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *FrameDecoder) ExceptionCaught(ctx pipeline.Context, err error) {
    fmt.Println("【FrameDecoder】异常:", err)
}

// BizHandler 业务逻辑 Handler
// 作用：处理解析后的消息
type BizHandler struct{}

func (h *BizHandler) Name() string { return "biz" }

func (h *BizHandler) ChannelRead(ctx pipeline.Context, msg interface{}) {
    if m, ok := msg.(*conn.Message); ok {
        fmt.Printf("【BizHandler】收到业务消息: %s\n", string(m.Data))
    }
    ctx.FireChannelRead(msg)
}

func (h *BizHandler) ChannelActive(ctx pipeline.Context)   { ctx.FireChannelActive() }
func (h *BizHandler) ChannelInactive(ctx pipeline.Context) { ctx.FireChannelInactive() }
func (h *BizHandler) ExceptionCaught(ctx pipeline.Context, err error) {}

// ====== OutboundHandler 示例 ======

// FrameEncoder 假设的帧编码 Handler
// 作用：把 conn.Message 序列化为二进制字节流
type FrameEncoder struct{}

func (h *FrameEncoder) Name() string { return "frameEncoder" }

func (h *FrameEncoder) Write(ctx pipeline.Context, msg interface{}) {
    fmt.Println("【FrameEncoder】正在编码帧...")
    // 序列化完成后，继续往前传（传给更靠近 Head 的 OutboundHandler）
    ctx.Write(msg)
}

func (h *FrameEncoder) Flush(ctx pipeline.Context) {}

// ====== 主程序：组装 Pipeline ======

func main() {
    // 1. 创建一个新的 Pipeline
    p := pipeline.NewPipeline()

    // 2. 添加 InboundHandler（数据从网络进来时，按这个顺序执行）
    // AddLast 表示加在 Pipeline 的尾部
    // Inbound 链的遍历方向是：Head → ... → Tail
    p.AddLast("frameDecoder", &FrameDecoder{}) // 第 1 个处理：解码帧
    p.AddLast("biz", &BizHandler{})            // 第 2 个处理：业务逻辑

    // 3. 添加 OutboundHandler（数据要发出去时，按相反顺序执行）
    // Outbound 链的遍历方向是：Tail → ... → Head
    p.AddLast("frameEncoder", &FrameEncoder{}) // 写数据时：先经过 frameEncoder

    // 4. 模拟收到一条消息
    // FireChannelRead 从 Head 开始触发 Inbound 链
    fmt.Println("=== 模拟收到消息 ===")
    p.FireChannelRead(&conn.Message{Type: 0x1, Data: []byte("hello")})

    // 5. 模拟发送一条消息
    // FireChannelWrite 从 Tail 开始触发 Outbound 链
    fmt.Println("\n=== 模拟发送消息 ===")
    p.FireChannelWrite(&conn.Message{Type: 0x1, Data: []byte("world")})
}
```

### 运行结果

```
=== 模拟收到消息 ===
【FrameDecoder】正在解码帧...
【BizHandler】收到业务消息: hello

=== 模拟发送消息 ===
【FrameEncoder】正在编码帧...
```

**Inbound 链执行顺序：** Head → FrameDecoder → BizHandler → Tail

**Outbound 链执行顺序：** Tail → FrameEncoder → Head

---

## 示例 5：ByteBuf 手动操作（了解缓冲区）

`ByteBuf` 是 v2 引入的引用计数字节缓冲区。和 Go 的 `[]byte` 相比，它有：
- **读写指针分离** — `readerIndex` 和 `writerIndex` 分开管理
- **引用计数** — `Retain()`/`Release()` 管理生命周期，防止提前释放
- **零拷贝切片** — `Slice()` 共享底层数组，不复制数据

### 基本读写

```go
package main

import (
    "fmt"

    "github.com/lufeijun/goTools/ws/buf"
)

func main() {
    // 1. 创建 ByteBuf，容量 64 字节
    // 引用计数初始为 1
    bb := buf.NewByteBuf(64)

    // 2. 写入数据
    // Write 会把数据追加到 writerIndex 位置，然后 writerIndex 后移
    bb.Write([]byte("hello"))
    bb.WriteByte(' ')       // 写入单个字节
    bb.Write([]byte("world"))

    // 3. 查看可读字节数
    // ReadableBytes = writerIndex - readerIndex
    fmt.Printf("可读字节数: %d\n", bb.ReadableBytes()) // 11

    // 4. Peek — 窥视数据（不移动 readerIndex）
    // Peek(5) 返回从 readerIndex 开始的 5 个字节，但 readerIndex 不变
    peek := bb.Peek(5)
    fmt.Printf("Peek(5): %s\n", string(peek))        // hello
    fmt.Printf("Peek 后可读: %d\n", bb.ReadableBytes()) // 还是 11

    // 5. ReadBytes — 读取数据（移动 readerIndex）
    data := bb.ReadBytes(5)
    fmt.Printf("ReadBytes(5): %s\n", string(data))    // hello
    fmt.Printf("读取后可读: %d\n", bb.ReadableBytes())  // 6（剩 " world"）

    // 6. Skip — 跳过若干字节
    bb.Skip(1) // 跳过空格
    fmt.Printf("Skip 后可读: %d\n", bb.ReadableBytes()) // 5

    // 7. ReadAll — 读取剩余全部
    rest := bb.ReadAll()
    fmt.Printf("ReadAll: %s\n", string(rest))         // world
    fmt.Printf("ReadAll 后可读: %d\n", bb.ReadableBytes()) // 0

    // 8. 释放 ByteBuf
    // Release 会把引用计数 -1，如果归零就回收到 Pool
    bb.Release()
}
```

### 零拷贝切片

```go
package main

import (
    "fmt"

    "github.com/lufeijun/goTools/ws/buf"
)

func main() {
    // 创建 ByteBuf，写入数据
    bb := buf.NewByteBuf(64)
    bb.Write([]byte("hello world"))

    // Slice 创建一个新的 ByteBuf，共享同一个底层数组
    // 参数：start（起始位置），length（长度）
    // Slice 会让原始 ByteBuf 的引用计数 +1（因为新视图依赖它）
    sliced := bb.Slice(0, 5) // 取前 5 个字节："hello"

    // sliced 的 readerIndex=0, writerIndex=5
    fmt.Printf("切片内容: %s\n", string(sliced.ReadAll())) // hello

    // sliced 用完后要释放（引用计数 -1）
    sliced.Release()

    // bb 也要释放
    bb.Release()

    // 引用计数规则总结：
    // - NewByteBuf / Pool.Get → refCount = 1
    // - Retain() → refCount +1
    // - Release() → refCount -1，归零时回收
    // - Slice() → 原始 buf refCount +1，新 buf refCount = 1
    // - double-free 或 Retain 已释放的 buf → panic
}
```

---

## 示例 6：状态监听和自动重连

Session 有 5 个状态：`Disconnected`、`Connecting`、`Connected`、`Reconnecting`、`Closed`。

客户端在连接断开时会自动重连，你可以通过 `StateChan()` 监听状态变化。

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/client"
)

func main() {
    // 创建客户端，配置重连参数
    c := client.NewClient(ws.Config{
        Addr:              "ws://localhost:8080/",
        PingInterval:      30 * time.Second,
        PongTimeout:       60 * time.Second,
        ReconnectInterval: 3 * time.Second, // 每 3 秒重试一次
        MaxReconnect:      5,               // 最多重试 5 次
    })

    // 获取 Session（Connect 之后才不为 nil）
    // 这里先不获取，等连接成功后再获取

    // 连接服务端
    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }

    // 连接成功后获取 Session
    sess := c.Session()
    if sess == nil {
        log.Fatal("Session 为 nil")
    }

    // 启动一个 goroutine 监听状态变化
    // StateChan() 返回一个只读 channel，每次状态变化时会收到新状态
    go func() {
        for state := range sess.StateChan() {
            // state 是 session.State 类型，有 String() 方法
            switch state {
            case session.StateConnected:
                fmt.Println("【状态】已连接")
            case session.StateDisconnected:
                fmt.Println("【状态】已断开")
            case session.StateReconnecting:
                fmt.Println("【状态】正在重连...")
            case session.StateClosed:
                fmt.Println("【状态】连接已关闭（重连耗尽）")
                return // 退出 goroutine
            }
        }
    }()

    fmt.Println("客户端运行中...按 Ctrl+C 退出")
    select {}
}
```

### 状态流转图

```
初始状态: Disconnected

      Connect() 被调用
            │
            ▼
      Connecting ──失败──► Reconnecting ◄─────┐
            │                │   ▲             │
            │ 成功           │   │ 重试间隔后  │ 达到 MaxReconnect
            ▼                │   │             │
       Connected ──断开──►──┘   └─────────────┘
            │                                    │
            │ 调用 Close()                       ▼
            ▼                              Closed
       Disconnected
```

---

## 配置详解

`ws.Config` 是 Server 和 Client 的通用配置：

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `Addr` | `string` | `""` | Server 监听地址（如 `":8080"`）/ Client 目标地址（如 `"ws://localhost:8080"`） |
| `ReadBufferSize` | `int` | `4096` | 读缓冲区大小（字节） |
| `WriteBufferSize` | `int` | `4096` | 写缓冲区大小（字节） |
| `MaxConnections` | `int` | `0` | 最大连接数，`0` 表示不限制 |
| `TCPNoDelay` | `bool` | `true` | 关闭 Nagle 算法，降低小帧延迟 |
| `TCPQuickAck` | `bool` | `false` | Linux 下启用 TCP_QUICKACK |
| `SOReusePort` | `bool` | `false` | 启用 SO_REUSEPORT，支持多进程负载均衡 |
| `EventLoopWorkers` | `int` | `runtime.NumCPU()` | SubEventLoop 数量 |
| `EventLoopStrategy` | `string` | `"roundrobin"` | 负载均衡策略 |
| `BufferPoolSmall` | `int` | `4096` | 小 ByteBuf 池对象数（≤512B） |
| `BufferPoolDefault` | `int` | `1024` | 默认 ByteBuf 池对象数（≤4096B） |
| `BufferPoolLarge` | `int` | `256` | 大 ByteBuf 池对象数（≤65536B） |
| `PingInterval` | `time.Duration` | `30s` | 心跳发送间隔 |
| `PongTimeout` | `time.Duration` | `60s` | Pong 回复超时时间 |
| `MaxFrameSize` | `int` | `64MB` | 单帧最大载荷 |
| `EnableCompression` | `bool` | `false` | 预留：permessage-deflate 压缩 |
| `Headers` | `http.Header` | `nil` | Client 握手时附加的 HTTP 头 |
| `ReconnectInterval` | `time.Duration` | `5s` | Client 断线后重连间隔 |
| `MaxReconnect` | `int` | `5` | Client 最大重连次数 |

### 使用默认配置

```go
// 获取一份默认配置，然后只修改你关心的字段
cfg := ws.DefaultConfig()
cfg.Addr = ":8080"
cfg.MaxConnections = 100000
cfg.PingInterval = 60 * time.Second
```

---

## 架构概览

```
┌─────────────────────────────────────────────────────────────┐
│                        用户代码                              │
│                  server.NewServer / client.NewClient        │
├─────────────────────────────────────────────────────────────┤
│                      会话层 (session)                         │
│         Session · State · Heartbeater · Reconnector         │
├─────────────────────────────────────────────────────────────┤
│                      处理器链 (pipeline)                      │
│         ChannelPipeline · InboundHandler · OutboundHandler   │
├─────────────────────────────────────────────────────────────┤
│                      连接层 (conn)                            │
│         Conn · netConn · epollConn · Handshake              │
├─────────────────────────────────────────────────────────────┤
│                      事件驱动 (eventloop)                     │
│         EventLoop · Poller · epoll · kqueue                 │
├─────────────────────────────────────────────────────────────┤
│              协议层 (frame) + 缓冲区 (buf)                    │
│         Frame · ReadFrame · WriteFrame · ByteBuf · Pool      │
└─────────────────────────────────────────────────────────────┘
```

**分层依赖规则：** 上层只依赖下层接口，不能跨层调用，不能反向依赖。

| 包 | 职责 |
|---|---|
| `ws` | 根包：WSError、Config、DefaultConfig |
| `ws/buf` | ByteBuf 接口 + 引用计数实现 + 分级对象池 |
| `ws/frame` | RFC 6455 帧解析/序列化（v1 保留，可独立使用） |
| `ws/pipeline` | ChannelPipeline + Handler 链（Netty 风格） |
| `ws/eventloop` | 跨平台事件驱动：epoll(Linux)、kqueue(BSD) |
| `ws/conn` | Conn 接口、netConn、epollConn、RFC 6455 握手 |
| `ws/session` | Session 接口、状态机、心跳、自动重连 |
| `ws/hub` | 分片锁 Hub：注册/注销/广播/定向发送 |
| `ws/server` | Server 启动、HTTP Upgrade、Session 生命周期 |
| `ws/client` | Client 连接、握手、重连、Session 生命周期 |

---

## 常见问题

### Q: 客户端连不上服务端？

1. 先检查服务端是否已启动（`go run server.go`）
2. 检查地址是否正确（客户端 `ws://localhost:8080/` 要和服务端 `:8080` 匹配）
3. 检查防火墙是否放行了 8080 端口
4. 查看是否有报错信息（`log.Fatal` 会打印错误）

### Q: 消息发出去但服务端没收到？

1. 确认 Pipeline 中注册了处理该消息的 InboundHandler
2. 确认 Handler 中调用了 `ctx.FireChannelRead(msg)` 把消息传给下一个 Handler
3. 确认 `msg.Type` 是对的（服务端可能只处理了 `0x1` 文本帧）

### Q: 为什么用 `select {}` 阻塞？

在示例中，`select {}` 表示"永远等待"。真实项目中，你应该在这里写业务逻辑（比如读取用户输入、处理消息等）。示例用 `select {}` 是为了简化代码，突出核心 API 的使用。

### Q: v2 和 v1 有什么区别？

| 对比项 | v1 | v2 |
|---|---|---|
| API 风格 | Channel（`ReadChan()` / `WriteChan()`） | Pipeline Handler（`ChannelRead(ctx, msg)`） |
| 并发模型 | goroutine-per-conn | 事件驱动（epoll/kqueue） |
| 包可见性 | `internal` 隐藏实现 | 全部公开，接口隔离 |
| 缓冲区 | `sync.Pool` 两级复用 `[]byte` | 引用计数 ByteBuf |

v2 **不保证向后兼容**，迁移需要重写业务代码。

---

## 注意事项

1. **v2 不兼容 v1** — API 从 Channel 式改为 Pipeline Handler 式，需重新适配
2. **Pipeline 在 V2.1 完善** — 当前 Server/Client 的 Pipeline Handler 注册需手动在 conn 创建后通过 `Conn().Pipeline().AddLast()` 添加
3. **epollConn 在 V2.1 完善** — 当前事件驱动 Conn 的 `Read`/`Write` 使用非阻塞 syscall 的完整实现将在 V2.1 交付
4. **心跳设计** — V2 心跳只发 Ping 不消费 ReadChan，避免与用户读消息冲突；Pong 超时检测推迟到 V2.1 时间轮实现
5. **Hub 广播非阻塞** — 对慢连接直接跳过，避免广播被单个慢连接拖住

---

## License

MIT

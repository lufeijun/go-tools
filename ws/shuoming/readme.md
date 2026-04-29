# goTools/ws — 高并发 WebSocket 库 v2

`goTools/ws` 是一个从零实现 RFC 6455 WebSocket 协议的 Go 语言库。v2 版本在 v1 的功能完整性基础上全面重构为高并发架构，目标支撑单机 **十万到百万级** WebSocket 连接。

---

## 项目定位

本库定位为**生产级、可扩展、高性能**的 WebSocket 基础框架，适用于实时消息推送、在线游戏、即时通讯、物联网网关等需要海量长连接的场景。

---

## 核心特性

| 特性 | 说明 |
|------|------|
| **面向接口编程** | 每一层只依赖下层接口，可独立替换实现 |
| **Pipeline + Handler 链** | Netty 风格的入站/出站处理器链，业务逻辑以插件方式组装 |
| **跨平台事件驱动** | Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP 预留) |
| **引用计数 ByteBuf** | 零拷贝、分级对象池、读写指针分离，替代 Go 原生 `[]byte` |
| **分片锁 Hub** | 32 个独立 `sync.RWMutex`，广播性能随分片数线性扩展 |
| **统一错误体系** | `WSError` 携带错误码、可读消息、底层 Cause、连接 ID，支持 `errors.Is` 链式判断 |
| **零 goroutine 空闲开销** | 事件驱动模型下，空闲连接不绑定任何 goroutine |

---

## 架构分层

```
┌─────────────────────────────────────────────────────────────┐
│                        用户代码                              │
│                  server.NewServer / client.NewClient        │
├─────────────────────────────────────────────────────────────┤
│                      会话层 (session)                         │
│         Session 接口 · State · Heartbeater · Reconnector     │
├─────────────────────────────────────────────────────────────┤
│                      处理器链 (pipeline)                      │
│         ChannelPipeline · InboundHandler · OutboundHandler   │
├─────────────────────────────────────────────────────────────┤
│                      连接层 (conn)                            │
│         Conn 接口 · netConn · epollConn · Handshake          │
├─────────────────────────────────────────────────────────────┤
│                      事件驱动 (eventloop)                     │
│         EventLoop · Poller · epollPoller · kqueuePoller      │
├─────────────────────────────────────────────────────────────┤
│              协议层 (frame) + 缓冲区 (buf)                    │
│         Frame · ReadFrame · WriteFrame · ByteBuf · Pool      │
└─────────────────────────────────────────────────────────────┘
```

**分层依赖规则：** 上层只能依赖下层接口，不能跨层调用，不能反向依赖。例如 `session` 依赖 `conn.Conn` 接口和 `pipeline.ChannelPipeline` 接口，但不知道 `netConn` 或 `epollConn` 的具体实现。

---

## 包结构一览

| 包 | 职责 | 关键文件 |
|---|---|---|
| `ws` | 根包：WSError、Config、DefaultConfig | `ws.go` |
| `ws/buf` | ByteBuf 接口 + 引用计数实现 + 分级对象池 | `bytebuf.go`, `pool.go` |
| `ws/frame` | RFC 6455 帧解析/序列化（v1 保留，可独立使用） | `frame.go`, `mask.go`, `pool.go` |
| `ws/pipeline` | ChannelPipeline + Handler 链（Netty 风格） | `pipeline.go`, `handler.go` |
| `ws/eventloop` | 跨平台事件驱动：epoll(Linux)、kqueue(BSD) | `eventloop.go`, `epoll_linux.go`, `kqueue_bsd.go` |
| `ws/conn` | Conn 接口、netConn、epollConn、RFC 6455 握手 | `conn.go`, `netconn.go`, `epollconn.go`, `handshake.go`, `codec.go` |
| `ws/session` | Session 接口、状态机、心跳、自动重连 | `session.go`, `heartbeat.go`, `reconnect.go` |
| `ws/hub` | 分片锁 Hub：注册/注销/广播/定向发送 | `hub.go` |
| `ws/server` | Server 启动、HTTP Upgrade、Session 生命周期 | `server.go` |
| `ws/client` | Client 连接、握手、重连、Session 生命周期 | `client.go` |
| `demo/` | 完整可运行的 Echo 示例（服务端 + 客户端） | `demo/server/server.go`, `demo/client/client.go` |

---

## v1 vs v2 关键差异

| 对比项 | v1 | v2 |
|---|---|---|
| 并发模型 | goroutine-per-conn（2-3 goroutine/连接） | 跨平台事件驱动（epoll/kqueue），连接不绑定常驻 goroutine |
| API 风格 | Channel 式（`ReadChan()` / `WriteChan()`） | Pipeline + Handler 链（Netty 风格） |
| 包可见性 | `internal` 隐藏实现细节 | 全部公开，接口隔离实现 |
| 缓冲区 | `sync.Pool` 两级复用 `[]byte` | 引用计数 ByteBuf，支持零拷贝、池化、读写指针分离 |
| 心跳 | per-conn ticker goroutine | 接口化，`perConnHeartbeater` 可替换为时间轮 |
| Hub | 单 goroutine + channel | 分片锁（sharded lock），32 个 `sync.RWMutex` |
| 兼容性 | — | **不保证向后兼容**，全新 API |

---

## 快速开始

### 安装

```bash
go mod init myproject
go get github.com/lufeijun/goTools/ws
```

Go 版本要求：`>= 1.24`

### 服务端示例

```go
package main

import (
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/server"
)

func main() {
    cfg := ws.Config{
        Addr:         ":8080",
        PingInterval: 30 * time.Second,
        PongTimeout:  60 * time.Second,
    }
    srv := server.NewServer(cfg)
    log.Println("服务端启动，监听 :8080 ...")
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
}
```

### 客户端示例

```go
package main

import (
    "log"
    "time"

    "github.com/lufeijun/goTools/ws"
    "github.com/lufeijun/goTools/ws/client"
)

func main() {
    cfg := ws.Config{
        Addr:              "ws://localhost:8080/",
        PingInterval:      30 * time.Second,
        PongTimeout:       60 * time.Second,
        ReconnectInterval: 5 * time.Second,
        MaxReconnect:      3,
    }
    c := client.NewClient(cfg)
    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }
    log.Println("连接成功！")
    select {}
}
```

---

## 演进路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 接口化重构 + Pipeline + 事件驱动（epoll/kqueue） | 十万连接 |
| V2.1 | Windows IOCP + 时间轮心跳 + 非阻塞 syscall 读写 | 跨平台完整 |
| V2.2 | goroutine 池化（业务计算池 + I/O 池分离） | 五十万连接 |
| V2.3 | sendfile/splice 零拷贝、SO_REUSEPORT | 百万连接 |

---

## License

MIT

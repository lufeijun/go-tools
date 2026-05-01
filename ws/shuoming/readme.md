# goTools/ws — 高并发 WebSocket 库 v2

`goTools/ws` 是一个从零实现 RFC 6455 WebSocket 协议的 Go 语言库。v2 版本在 v1 的功能完整性基础上全面重构为高并发架构，目标支撑单机 **十万到百万级** WebSocket 连接。

---

## 项目定位

本库定位为**生产级、可扩展、高性能**的 WebSocket 基础框架，适用于实时消息推送、在线游戏、即时通讯、物联网网关等需要海量长连接的场景。

---

## 核心特性

| 特性 | 说明 |
|------|------|
| **Pipeline + Handler 链** | Netty 风格的入站/出站处理器链，业务逻辑以插件方式组装 |
| **双 I/O 模式** | net 模式（goroutine-per-conn）和 epoll 模式（事件驱动 Reactor），通过 `ws.Config.Mode` 切换 |
| **跨平台事件驱动** | Linux(epoll)、macOS/FreeBSD(kqueue)、Windows(IOCP 预留) |
| **引用计数 ByteBuf** | 零拷贝、分级对象池、读写指针分离，替代 Go 原生 `[]byte` |
| **分片锁 Hub** | 32 个独立 `sync.RWMutex` + 缓存行对齐 + 固定 worker pool，广播性能随分片数线性扩展 |
| **统一错误体系** | `WSError` 携带错误码、可读消息、底层 Cause、连接 ID，支持 `errors.Is` 链式判断 |
| **共享时间轮心跳** | 128 slots 单级时间轮，1 个 goroutine 管理所有连接的心跳超时 |
| **客户端自动重连** | 指数退避策略（1s -> 2s -> 4s ... 最大 60s） |
| **零 goroutine 空闲开销** | epoll 模式下，空闲连接不绑定任何常驻 goroutine |

---

## 架构分层

```
+-----------------------------------------------------------------+
|                          用户代码                                 |
|                server.NewServer / client.NewClient               |
+-----------------------------------------------------------------+
|                        会话层 (session)                            |
|           Session 接口 . State . Heartbeater . Reconnector        |
+-----------------------------------------------------------------+
|                        处理器链 (pipeline)                         |
|           ChannelPipeline . InboundHandler . OutboundHandler      |
+-----------------------------------------------------------------+
|                        连接层 (conn)                               |
|           Conn 接口 . netConn . epollConn . Handshake . Codec     |
+-----------------------------+-----------------------------------+
|     net 模式                |         epoll 模式                  |
|  netAcceptor               |  epollAcceptor (主从 Reactor)       |
|  goroutine-per-conn        |  EventLoopGroup (Sub-Reactor)      |
|  frame.ReadFrame (阻塞)    |  IncrementalParser (非阻塞)         |
+-----------------------------+-----------------------------------+
|                        事件驱动 (eventloop)                        |
|           EventLoop . Poller . EventLoopGroup . epollPoller       |
+-----------------------------------------------------------------+
|              协议层 (frame) + 缓冲区 (buf)                         |
|           Frame . ReadFrame . IncrementalParser . ByteBuf . Pool  |
+-----------------------------------------------------------------+
```

**双 I/O 模式说明：**

- **net 模式（默认）**：使用 `netAcceptor` + `netConn`，每个连接一个 goroutine 阻塞读取帧（`frame.ReadFrame`）。适合连接数 <10 万的场景，简单可靠。
- **epoll 模式（Linux）**：使用 `epollAcceptor` + `EpollConn`，主从 Reactor 模型，`EventLoopGroup` 管理 N 个 sub-loop，非阻塞 `IncrementalParser` 增量解析帧。适合 10-100 万连接的场景，空闲连接零 goroutine 开销。

通过 `ws.Config.Mode` 切换：

```go
cfg := ws.DefaultConfig()
cfg.Mode = ws.ModeEpoll   // 或 ws.ModeNet（默认）
```

**分层依赖规则：** 上层只能依赖下层接口，不能跨层调用，不能反向依赖。例如 `session` 依赖 `conn.Conn` 接口和 `pipeline.ChannelPipeline` 接口，但不知道 `netConn` 或 `epollConn` 的存在。

---

## 包结构一览

| 包 | 职责 | 关键文件 |
|---|---|---|
| `ws` | 根包：WSError、Config、DefaultConfig、IOMode | `ws.go` |
| `ws/buf` | ByteBuf 接口 + 引用计数实现 + 分级对象池 | `bytebuf.go`, `pool.go` |
| `ws/frame` | RFC 6455 帧解析/序列化（含零拷贝路径 + IncrementalParser） | `frame.go`, `parser.go`, `mask.go`, `pool.go` |
| `ws/pipeline` | ChannelPipeline + Handler 链（预编译数组） | `pipeline.go`, `handler.go` |
| `ws/eventloop` | 跨平台事件驱动 + EventLoopGroup + goroutine pool 调度 | `eventloop.go`, `group.go`, `epoll_linux.go`, `kqueue_bsd.go` |
| `ws/conn` | Conn 接口、netConn、epollConn、握手、编解码器、TCP 参数 | `conn.go`, `netconn.go`, `epollconn.go`, `handshake.go`, `codec.go`, `tcp.go` |
| `ws/session` | Session 接口、状态机（发布/订阅）、时间轮心跳、自动重连 | `session.go`, `heartbeat.go`, `timingwheel.go`, `reconnect.go` |
| `ws/hub` | 分片锁 Hub：注册/注销/广播/定向发送/批量关闭 | `hub.go` |
| `ws/server` | Server 启动、Acceptor（双模式）、Session 生命周期 | `server.go`, `acceptor.go`, `acceptor_epoll_linux.go` |
| `ws/client` | Client 连接（双模式）、握手、重连、Session 生命周期 | `client.go`, `epoll_linux.go` |
| `demo/` | 完整可运行的 Echo 示例 + 聊天室示例 | `demo/server/`, `demo/client/`, `demo/chat/` |

---

## v1 vs v2 关键差异

| 对比项 | v1 | v2 |
|---|---|---|
| 并发模型 | goroutine-per-conn（2-3 goroutine/连接） | 双 I/O 模式：net 模式（goroutine-per-conn）+ epoll 模式（事件驱动 Reactor） |
| API 风格 | Channel 式（`ReadChan()` / `WriteChan()`） | Pipeline + Handler 链（Netty 风格） |
| 包可见性 | `internal` 隐藏实现细节 | 全部公开，接口隔离实现 |
| 缓冲区 | `sync.Pool` 两级复用 `[]byte` | 引用计数 ByteBuf，支持零拷贝、池化、读写指针分离 |
| 心跳 | per-conn ticker goroutine | 共享时间轮（单 goroutine 管理所有连接） |
| Hub | 单 goroutine + channel | 分片锁 + 缓存行对齐 + 固定 worker pool + CloseAll |
| 自动重连 | 无 | 指数退避（1s->2x->60s） |
| 状态通知 | 单一 channel | 发布/订阅模式（多 goroutine 独立订阅） |
| 帧解析 | 仅阻塞 ReadFrame | 阻塞 ReadFrame + 非阻塞 IncrementalParser |
| 兼容性 | — | **不保证向后兼容**，全新 API |

---

## 快速开始

### 安装

```bash
go mod init myproject
go get github.com/lufeijun/goTools/ws
```

Go 版本要求：`>= 1.25`

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
    srv, err := server.NewServer(cfg)
    if err != nil {
        log.Fatal(err)
    }
    log.Println("服务端启动，监听 :8080 ...")
    if err := srv.Start(); err != nil {
        log.Fatal(err)
    }
}
```

### epoll 模式服务端

```go
func main() {
    cfg := ws.DefaultConfig()
    cfg.Addr = ":8080"
    cfg.Mode = ws.ModeEpoll   // 启用 epoll 模式（仅 Linux）

    srv, err := server.NewServer(cfg)
    if err != nil {
        log.Fatal(err)
    }
    log.Println("epoll 模式服务端启动，监听 :8080 ...")
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
    c, err := client.NewClient(cfg)
    if err != nil {
        log.Fatal(err)
    }
    if err := c.Connect(); err != nil {
        log.Fatal("连接失败:", err)
    }
    log.Println("连接成功！")
    select {}
}
```

---

## 优化清单（实现状态）

| 优先级 | 编号 | 优化项 | 状态 |
|---|---|---|---|
| P0 | 4.1 | ReadFrame MaxFrameSize 校验 | Done |
| P0 | 4.2 | 分片重组累积长度上限 | Done |
| P0 | 4.6 | Hub.Send / Broadcast 实现 | Done |
| P0 | 5.1 | 心跳时间轮 | Done |
| P0 | 3.1 | epollConn 非阻塞 I/O | Done |
| P0 | 3.2 | TCP 参数实际生效 | Done |
| P1 | 4.3 | Handshake 超时 | Done |
| P1 | 4.4 | Close 帧 RFC 合规 | Done |
| P1 | 4.5 | stateChan / serveConn 泄漏 | Done |
| P1 | 3.3 | 读缓冲批量预读（bufio） | Done |
| P1 | 1.1 | 心跳时间轮 | Done |
| P1 | 2.3 | frame 层 ByteBuf 化 | Done |
| P2 | 5.2 | 移除 serveConn goroutine（epoll 模式） | Done |
| P2 | 5.3 | Hub shard false sharing | Done |
| P2 | 1.2 | Hub broadcast worker pool | Done |
| P2 | 1.4 | EventLoop handler 查找优化 | Done |
| P2 | 5.5 | EventLoop.Stop 资源清理 | Done |
| P2 | 5.6 | 客户端自动重连 | Done |
| P2 | 5.9 | Hub CloseAll 批量关闭 | Done |
| P2 | 5.10 | Session StateChan 发布/订阅模式 | Done |
| P2 | 5.11 | IncrementalParser 非阻塞帧解析 | Done |
| P2 | 5.12 | EventLoopGroup + round-robin | Done |
| P2 | 5.13 | Acceptor 双模式（net/epoll） | Done |
| P2 | 5.14 | NewEpollPoller 导出 | Done |
| P3 | 1.3 | EventLoop 异步 dispatch | Done |
| P3 | 1.5 | Pipeline 预编译数组 | Done |
| P3 | 2.2 | Peek+Skip 零拷贝帧解析 | Done |
| P3 | 2.4 | mask 原地 XOR | Done |
| P3 | 2.5 | ByteBuf 扩容对齐 | Done |
| P3 | 5.7 | benchmark 基线 | Done |
| P3 | 5.8 | ByteBuf 线程安全文档 | Done |

---

## 演进路线图

| 阶段 | 内容 | 目标 |
|---|---|---|
| V2.0 | 接口化重构 + Pipeline + 双 I/O 模式（net/epoll）+ 主从 Reactor + 全部 P0/P1/P2/P3 优化落地 | 十万连接 |
| V2.1 | epollConn 完善非阻塞读写 + Windows IOCP + sendfile/splice | 跨平台完整 |
| V2.2 | goroutine 池化精细调优 + 业务计算池分离 + 写合并 | 五十万连接 |
| V2.3 | 内核旁路（DPDK/AF_XDP）调研 | 百万连接 |

---

## License

MIT

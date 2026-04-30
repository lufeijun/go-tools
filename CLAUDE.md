# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is `go-tools`, a Go module (`github.com/lufeijun/goTools`) containing a custom WebSocket library at `ws/`. The `ws` package is a from-scratch implementation of RFC 6455, currently at v2 — a high-concurrency, Netty-inspired rewrite targeting 100K–1M concurrent connections per machine.

Go version: `1.25.0`.

## Common Commands

```bash
# Run all tests
go test ./...

# Run tests for the ws module only
go test ./ws/...

# Run a specific test
go test ./ws/pipeline -run TestPipelineOrder

# Build all packages
go build ./...

# Run the demo server (listens on :8080)
go run demo/server/server.go

# Run the demo client (interactive echo client)
go run demo/client/client.go

# Format and vet
go fmt ./...
go vet ./...
```

## Architecture

The `ws` library is organized into strict layers. **Upper layers only depend on lower-layer interfaces; no cross-layer or reverse dependencies.**

```
server / client       → entry points; both use ws.Config
session               → Session interface, state machine, heartbeat, reconnect
pipeline              → ChannelPipeline + InboundHandler / OutboundHandler chain (Netty-style)
conn                  → Conn interface, netConn, epollConn, RFC 6455 handshake
eventloop             → EventLoop / Poller abstractions; epoll (Linux), kqueue (BSD)
frame + buf           → Frame parsing/serialization, reference-counted ByteBuf
```

### Key Design Decisions

- **Event-driven, not goroutine-per-conn:** v2 uses epoll/kqueue with a main/sub Reactor model. The goal is near-zero idle goroutine overhead. Currently `netConn` is the active implementation; `epollConn` is targeted for V2.1.
- **Pipeline + Handler chain:** Inbound events flow Head → Tail; outbound events flow Tail → Head. Handlers are added at setup time with `AddFirst` / `AddLast`. Runtime event traversal is lock-free.
- **Reference-counted ByteBuf:** `buf.ByteBuf` uses `Retain()` / `Release()` for lifecycle management and `Slice()` for zero-copy views. The default implementation uses a tiered `sync.Pool` (≤512B, ≤4096B, ≤65536B, direct).
- **Sharded Hub:** `hub.NewHub(shardCount)` splits connections across N shards, each with its own `sync.RWMutex`. Default shard count is 32. `Broadcast()` spawns a goroutine per shard.
- **Unified errors:** `ws.WSError` carries a numeric code (protocol 1xxx, network 2xxx, app 3xxx), message, cause, and optional ConnID. Use `errors.Is` / `errors.As` with `WSError.Unwrap()`.

### Critical Interfaces

- **`conn.Conn`** — `ID()`, `Pipeline()`, `Read(ByteBuf)`, `Write(ByteBuf)`, `Close()`.
- **`pipeline.ChannelPipeline`** — `AddFirst(name, handler)`, `AddLast(name, handler)`, `FireChannelRead(msg)`, `FireChannelWrite(msg)`.
- **`pipeline.InboundHandler`** — implements `ChannelRead(ctx, msg)`, `ChannelActive(ctx)`, `ChannelInactive(ctx)`, `ExceptionCaught(ctx, err)`.
- **`pipeline.OutboundHandler`** — implements `Write(ctx, msg)`, `Flush(ctx)`.
- **`session.Session`** — wraps a `conn.Conn`, exposes state machine (`StateChan()`), heartbeat, and reconnect config.
- **`hub.Hub`** — `Register`, `Unregister`, `Broadcast`, `Send`, `Count`, `Get`.

### Frame Handling

`frame.Frame` is the low-level RFC 6455 structure. `frame.ReadFrame(r io.Reader)` and `frame.WriteFrame(w io.Writer, f Frame)` operate on raw I/O and handle fragmentation.

`conn.FrameCodec` is an `OutboundHandler` that encodes `*conn.Message` into `frame.Frame` and writes it to an `io.Writer`. The demo and current server/client code register a `FrameCodec` on each connection’s Pipeline so that user handlers can call `ctx.Write(&conn.Message{...})` instead of manually constructing frames.

### Connection Lifecycle (Current Server Implementation)

1. `server.Server` listens via standard `net/http`.
2. On WebSocket upgrade, `conn.ServerHandshake` hijacks the connection.
3. A `netConn` is created, wrapped in a `session.Session`, heartbeat is started, and the session is registered with the Hub.
4. `conn.FrameCodec` is added to the Pipeline.
5. `srv.OnConnect` callback fires; user handlers (e.g., `EchoHandler`) should be added here via `sess.Conn().Pipeline().AddLast("name", handler)`.
6. `serveConn` loops calling `frame.ReadFrame` and fires inbound messages through the Pipeline.

### Configuration

`ws.Config` is shared by Server and Client. Use `ws.DefaultConfig()` for sensible defaults. Key fields:
- `Addr` — server bind address (`:8080`) or client target (`ws://localhost:8080/`)
- `EventLoopWorkers` — defaults to `runtime.NumCPU()`
- `PingInterval` / `PongTimeout` — defaults 30s / 60s
- `MaxFrameSize` — default 64MB
- `BufferPoolSmall/Default/Large` — pool capacities for ByteBuf tiers

## Testing

Every `ws` subpackage has tests. Platform-specific eventloop tests use build tags (`//go:build linux` / `//go:build darwin || freebsd || openbsd`). The root `ws` package and all subpackages should pass on their respective platforms.

## Demo

`demo/server/server.go` and `demo/client/client.go` provide an interactive echo example. The server attaches an `EchoHandler` in `OnConnect`, prepends a server timestamp, and echoes back. The client reads from stdin and prints server replies.

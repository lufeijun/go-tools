package ws_test

import (
	"errors"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/client"
	"github.com/lufeijun/goTools/ws/server"
)

func TestWSError_Error(t *testing.T) {
	e := &ws.WSError{
		Code:    ws.ErrCodeProtocolError,
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
	e := &ws.WSError{
		Code:    ws.ErrCodeReadTimeout,
		Message: "read timeout",
		Cause:   cause,
	}
	if !errors.Is(e, cause) {
		t.Error("errors.Is should match wrapped cause")
	}
}

func TestWSError_WithConnID(t *testing.T) {
	e := &ws.WSError{Code: ws.ErrCodeInternalError, Message: "fail"}
	e2 := e.WithConnID(99)
	if e2.ConnID != 99 {
		t.Errorf("ConnID = %d, want 99", e2.ConnID)
	}
	if e.ConnID != 0 {
		t.Error("original WSError should not be modified")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := ws.DefaultConfig()
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

func TestIntegration_ServerStartStop(t *testing.T) {
	srv := server.NewServer(ws.Config{
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
	c := client.NewClient(ws.Config{
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

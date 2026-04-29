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

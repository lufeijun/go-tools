package client

import (
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws"
)

func TestClient_New(t *testing.T) {
	c, err := NewClient(ws.Config{
		Addr:              "ws://localhost:8080/",
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		ReconnectInterval: 5 * time.Second,
		MaxReconnect:      3,
	})
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.Config().MaxReconnect != 3 {
		t.Errorf("MaxReconnect = %d, want 3", c.Config().MaxReconnect)
	}
}

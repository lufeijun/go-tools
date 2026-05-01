//go:build linux

package server

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws"
)

func TestEpollAcceptor_ListenAndAccept(t *testing.T) {
	cfg := ws.DefaultConfig()
	cfg.Addr = "127.0.0.1:0"
	cfg.Mode = ws.ModeEpoll

	a := newEpollAcceptor(cfg)
	if err := a.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	addr := a.Addr()

	nc, err := net.DialTimeout("tcp", addr.String(), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	req := "GET / HTTP/1.1\r\n" +
		"Host: " + addr.String() + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	nc.Write([]byte(req))

	c, rawNc, err := a.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer c.Close()

	if rawNc != nil {
		t.Error("expected nil net.Conn in epoll mode")
	}

	br := make([]byte, 1024)
	nc.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := nc.Read(br)
	resp := string(br[:n])
	if !strings.Contains(resp, "101") {
		limit := len(resp)
		if limit > 100 {
			limit = 100
		}
		t.Errorf("expected 101 response, got: %s", resp[:limit])
	}
}

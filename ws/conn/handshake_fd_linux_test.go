//go:build linux

package conn

import (
	"bufio"
	"golang.org/x/sys/unix"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClientHandshakeFD(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			t.Logf("read request: %v", err)
			return
		}
		if !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			t.Error("not ws upgrade")
			return
		}

		resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + computeAcceptKey(req.Header.Get("Sec-WebSocket-Key")) + "\r\n\r\n"
		conn.Write([]byte(resp))
	}()

	fd, err := DialNonBlock(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)

	time.Sleep(50 * time.Millisecond)

	err = ClientHandshakeFD(fd, "ws://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatalf("ClientHandshakeFD: %v", err)
	}
	<-done
}

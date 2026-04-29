package conn

import (
	"bytes"
	"io"
	"net"
	"net/http/httptest"
	"testing"

	"github.com/lufeijun/goTools/ws/buf"
)

func TestNetConn_ReadWrite(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	c := NewNetConn(server, false, 1)
	defer c.Close()

	// Test Write: write from conn, read on client side
	go func() {
		bb := buf.NewByteBuf(64)
		defer bb.Release()
		bb.Write([]byte("hello"))
		if err := c.Write(bb); err != nil {
			t.Errorf("write error: %v", err)
		}
	}()

	got := make([]byte, 5)
	_, err := io.ReadFull(client, got)
	if err != nil {
		t.Fatalf("client read error: %v", err)
	}
	if !bytes.Equal(got, []byte("hello")) {
		t.Errorf("client read = %q, want hello", got)
	}

	// Test Read: write from client side, read on conn
	go client.Write([]byte("world"))

	bb := buf.NewByteBuf(64)
	defer bb.Release()

	if err := c.Read(bb); err != nil {
		t.Fatalf("read error: %v", err)
	}
	if !bytes.Equal(bb.ReadAll(), []byte("world")) {
		t.Errorf("read = %q, want world", bb.ReadAll())
	}
}

func TestNetConn_ID(t *testing.T) {
	client1, server1 := net.Pipe()
	client2, server2 := net.Pipe()
	defer client1.Close()
	defer client2.Close()
	defer server1.Close()
	defer server2.Close()

	c1 := NewNetConn(server1, false, 1)
	c2 := NewNetConn(server2, false, 2)

	if c1.ID() != 1 {
		t.Errorf("c1.ID = %d, want 1", c1.ID())
	}
	if c2.ID() != 2 {
		t.Errorf("c2.ID = %d, want 2", c2.ID())
	}
}

func TestNetConn_IsClient(t *testing.T) {
	_, server := net.Pipe()
	defer server.Close()

	sc := NewNetConn(server, false, 1)
	if sc.IsClient() {
		t.Error("server conn should not be client")
	}

	_, client := net.Pipe()
	defer client.Close()

	cc := NewNetConn(client, true, 2)
	if !cc.IsClient() {
		t.Error("client conn should be client")
	}
}

func TestServerHandshake_InvalidRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()
	_, err := ServerHandshake(w, req)
	if err == nil {
		t.Error("expected error for invalid handshake")
	}
}

func TestNetConn_Close(t *testing.T) {
	_, server := net.Pipe()
	defer server.Close()

	c := NewNetConn(server, false, 1)
	if !c.Active() {
		t.Error("conn should be active")
	}
	if err := c.Close(); err != nil {
		t.Errorf("close error: %v", err)
	}
	if c.Active() {
		t.Error("conn should be inactive after close")
	}
	// Double close should not error
	if err := c.Close(); err != nil {
		t.Errorf("double close error: %v", err)
	}
}

func TestComputeAcceptKey(t *testing.T) {
	// RFC 6455 example
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	expected := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := computeAcceptKey(key); got != expected {
		t.Errorf("computeAcceptKey = %q, want %q", got, expected)
	}
}

func TestEpollConn_Interface(t *testing.T) {
	// We can't test real epoll without a real fd, but we can verify the struct
	// implements EventDrivenConn interface.
	var _ EventDrivenConn = (*epollConn)(nil)
}

package conn

import (
	"bytes"
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

	// Write from client side
	go client.Write([]byte("hello"))

	bb := buf.NewByteBuf(64)
	defer bb.Release()

	c.Read(bb)
	if !bytes.Equal(bb.ReadAll(), []byte("hello")) {
		t.Errorf("read = %q, want hello", bb.ReadAll())
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
}

func TestServerHandshake_InvalidRequest(t *testing.T) {
	req := httptest.NewRequest("GET", "/ws", nil)
	w := httptest.NewRecorder()
	_, err := ServerHandshake(w, req)
	if err == nil {
		t.Error("expected error for invalid handshake")
	}
}

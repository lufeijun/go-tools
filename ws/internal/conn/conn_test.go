package conn

import (
	"net"
	"testing"
	"time"

	"github.com/lufeijun/goTools/ws/frame"
)

func TestGoroutineConn_SendReceive(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	defer srvConn.Close()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	msg := Message{Type: frame.OpcodeText, Data: []byte("hello from client")}

	go func() {
		cliConn.WriteChan() <- msg
	}()

	select {
	case received := <-srvConn.ReadChan():
		if received.Type != frame.OpcodeText {
			t.Errorf("Type = %d, want %d", received.Type, frame.OpcodeText)
		}
		if string(received.Data) != "hello from client" {
			t.Errorf("Data = %q, want %q", string(received.Data), "hello from client")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestGoroutineConn_Bidirectional(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	defer srvConn.Close()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	cliConn.WriteChan() <- Message{Type: frame.OpcodeText, Data: []byte("ping")}

	select {
	case msg := <-srvConn.ReadChan():
		if string(msg.Data) != "ping" {
			t.Errorf("server got %q, want %q", string(msg.Data), "ping")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	srvConn.WriteChan() <- Message{Type: frame.OpcodeText, Data: []byte("pong")}

	select {
	case msg := <-cliConn.ReadChan():
		if string(msg.Data) != "pong" {
			t.Errorf("client got %q, want %q", string(msg.Data), "pong")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}
}

func TestGoroutineConn_AutoPong(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	defer srvConn.Close()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	cliConn.WriteChan() <- Message{Type: frame.OpcodePing, Data: []byte("heartbeat")}

	select {
	case msg := <-srvConn.ReadChan():
		if msg.Type != frame.OpcodePing {
			t.Errorf("got type %d, want Ping", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout")
	}

	select {
	case msg := <-cliConn.ReadChan():
		if msg.Type != frame.OpcodePong {
			t.Errorf("auto pong type = %d, want Pong", msg.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for auto pong")
	}
}

func TestGoroutineConn_Close(t *testing.T) {
	client, server := net.Pipe()

	srvConn := newGoroutineConn(server, false)
	id := srvConn.ID()

	cliConn := newGoroutineConn(client, true)
	defer cliConn.Close()

	if id == 0 {
		t.Error("ID should not be zero")
	}

	err := srvConn.Close()
	if err != nil {
		t.Fatal(err)
	}

	srvConn.Close() // second Close should not panic
}

func TestGoroutineConn_IDIncrement(t *testing.T) {
	client1, server1 := net.Pipe()
	client2, server2 := net.Pipe()

	conn1 := newGoroutineConn(server1, false)
	conn2 := newGoroutineConn(server2, false)

	if conn1.ID() >= conn2.ID() {
		t.Errorf("conn2.ID (%d) should be greater than conn1.ID (%d)", conn2.ID(), conn1.ID())
	}

	client1.Close()
	client2.Close()
	conn1.Close()
	conn2.Close()
}

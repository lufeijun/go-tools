//go:build linux

package conn

import (
	"os"
	"testing"

	"github.com/lufeijun/goTools/ws/eventloop"
	"github.com/lufeijun/goTools/ws/frame"
	"golang.org/x/sys/unix"
)

func TestEpollConn_SetOnFrame(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	ec := NewEpollConn(int(r.Fd()), false, NextConnID())
	defer ec.Close()

	var received []frame.Frame
	ec.SetOnFrame(func(f frame.Frame) {
		received = append(received, f)
	})

	frameData := []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}
	w.Write(frameData)
	// We need to make the pipe non-blocking so unix.Read doesn't block
	// after we've read the data once
	unix.SetNonblock(int(r.Fd()), true)
	ec.OnEvent(eventloop.EventRead)

	if len(received) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(received))
	}
	if string(received[0].Payload) != "Hello" {
		t.Errorf("Payload = %q, want Hello", string(received[0].Payload))
	}
}

func TestEpollConn_Interface(t *testing.T) {
	var _ EventDrivenConn = (*EpollConn)(nil)
}

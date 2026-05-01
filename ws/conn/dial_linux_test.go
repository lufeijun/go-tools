//go:build linux

package conn

import (
	"net"
	"testing"
	"golang.org/x/sys/unix"
)

func TestDialNonBlock_TCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	defer ln.Close()

	fd, err := DialNonBlock(ln.Addr().String())
	if err != nil { t.Fatal(err) }
	defer unix.Close(fd)

	conn, err := ln.Accept()
	if err != nil { t.Fatal(err) }
	defer conn.Close()

	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil { t.Fatal(err) }
	if flags&unix.O_NONBLOCK == 0 { t.Error("fd is not non-blocking") }
}

package conn

import (
	"context"
	"errors"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// ApplyTCPOptions sets TCP_NODELAY and TCP_QUICKACK on the underlying socket.
func ApplyTCPOptions(c net.Conn, noDelay, quickAck bool) error {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil
	}
	rawConn, err := sc.SyscallConn()
	if err != nil {
		return err
	}

	var nodelayErr error
	rawConn.Control(func(fd uintptr) {
		if noDelay {
			nodelayErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
		} else {
			nodelayErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NODELAY, 0)
		}
	})
	if nodelayErr != nil {
		return nodelayErr
	}

	if quickAck {
		var quickErr error
		rawConn.Control(func(fd uintptr) {
			quickErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
		})
		if quickErr != nil {
			return quickErr
		}
	}

	return nil
}

// ListenTCPWithReusePort creates a TCP listener with SO_REUSEPORT enabled.
func ListenTCPWithReusePort(address string) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var err error
			c.Control(func(fd uintptr) {
				err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			})
			return err
		},
	}
	return lc.Listen(context.Background(), "tcp", address)
}

// SyscallConn returns the syscall.RawConn of the underlying net.Conn.
func (d *drainConn) SyscallConn() (syscall.RawConn, error) {
	if sc, ok := d.Conn.(syscall.Conn); ok {
		return sc.SyscallConn()
	}
	return nil, errors.New("underlying conn does not support syscall.Conn")
}

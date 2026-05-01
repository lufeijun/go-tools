//go:build linux

package conn

import (
    "fmt"
    "net"
    "golang.org/x/sys/unix"
)

func DialNonBlock(addr string) (int, error) {
    tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
    if err != nil { return -1, fmt.Errorf("resolve addr: %w", err) }

    fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
    if err != nil { return -1, fmt.Errorf("socket: %w", err) }

    sa := &unix.SockaddrInet4{Port: tcpAddr.Port}
    if tcpAddr.IP != nil && tcpAddr.IP.To4() != nil {
        copy(sa.Addr[:], tcpAddr.IP.To4())
    }

    err = unix.Connect(fd, sa)
    if err != nil && err != unix.EINPROGRESS {
        unix.Close(fd)
        return -1, fmt.Errorf("connect: %w", err)
    }
    return fd, nil
}

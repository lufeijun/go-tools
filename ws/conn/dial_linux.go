//go:build linux

package conn

import (
	"fmt"
	"golang.org/x/sys/unix"
	"net"
)

func DialNonBlock(addr string) (int, error) {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return -1, fmt.Errorf("resolve addr: %w", err)
	}

	if ip4 := tcpAddr.IP.To4(); ip4 != nil {
		return dialNonBlock4(tcpAddr.Port, ip4)
	}
	return dialNonBlock6(tcpAddr.Port, tcpAddr.IP.To16(), tcpAddr.Zone)
}

func dialNonBlock4(port int, ip net.IP) (int, error) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	sa := &unix.SockaddrInet4{Port: port}
	copy(sa.Addr[:], ip)
	if err := unix.Connect(fd, sa); err != nil && err != unix.EINPROGRESS {
		unix.Close(fd)
		return -1, fmt.Errorf("connect: %w", err)
	}
	return fd, nil
}

func dialNonBlock6(port int, ip net.IP, zone string) (int, error) {
	fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("socket: %w", err)
	}
	sa := &unix.SockaddrInet6{Port: port}
	copy(sa.Addr[:], ip)
	if zone != "" {
		ifi, err := net.InterfaceByName(zone)
		if err != nil {
			unix.Close(fd)
			return -1, fmt.Errorf("resolve zone %q: %w", zone, err)
		}
		sa.ZoneId = uint32(ifi.Index)
	}
	if err := unix.Connect(fd, sa); err != nil && err != unix.EINPROGRESS {
		unix.Close(fd)
		return -1, fmt.Errorf("connect: %w", err)
	}
	return fd, nil
}

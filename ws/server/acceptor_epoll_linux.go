//go:build linux

package server

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/eventloop"
	"golang.org/x/sys/unix"
)

type epollAcceptor struct {
	config    ws.Config
	listenFd  int
	addr      net.TCPAddr
	connCh    chan *acceptedConn
	elg       eventloop.EventLoopGroup
	mainLoop  eventloop.EventLoop
	running   int32
	closeOnce sync.Once
}

func newEpollAcceptor(cfg ws.Config) *epollAcceptor {
	return &epollAcceptor{
		config: cfg,
		connCh: make(chan *acceptedConn, 128),
	}
}

func (a *epollAcceptor) Addr() net.Addr {
	return &a.addr
}

func (a *epollAcceptor) Listen(addr string) error {
	tcpAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return fmt.Errorf("resolve addr: %w", err)
	}

	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}

	if a.config.SOReusePort {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)

	sa := &unix.SockaddrInet4{Port: tcpAddr.Port}
	if tcpAddr.IP != nil && tcpAddr.IP.To4() != nil {
		copy(sa.Addr[:], tcpAddr.IP.To4())
	}

	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return fmt.Errorf("bind: %w", err)
	}

	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return fmt.Errorf("listen: %w", err)
	}

	rawAddr, _ := unix.Getsockname(fd)
	if sa4, ok := rawAddr.(*unix.SockaddrInet4); ok {
		a.addr.Port = sa4.Port
		a.addr.IP = net.IP(sa4.Addr[:])
	}

	a.listenFd = fd

	workers := a.config.EventLoopWorkerCount()
	a.elg = eventloop.NewEventLoopGroup(workers, func() eventloop.Poller {
		return eventloop.NewEpollPoller()
	})
	if err := a.elg.Start(); err != nil {
		unix.Close(fd)
		return fmt.Errorf("start eventloop group: %w", err)
	}

	mainPoller := eventloop.NewEpollPoller()
	a.mainLoop = eventloop.NewEventLoop(mainPoller)
	if err := a.mainLoop.Register(fd, &acceptHandler{acceptor: a}); err != nil {
		a.elg.Stop()
		unix.Close(fd)
		return fmt.Errorf("register listen fd: %w", err)
	}

	atomic.StoreInt32(&a.running, 1)
	go a.mainLoop.Run()

	return nil
}

func (a *epollAcceptor) Accept() (conn.Conn, net.Conn, error) {
	ac := <-a.connCh
	return ac.c, ac.nc, nil
}

func (a *epollAcceptor) Close() error {
	a.closeOnce.Do(func() {
		atomic.StoreInt32(&a.running, 0)
		if a.mainLoop != nil {
			a.mainLoop.Stop()
		}
		if a.elg != nil {
			a.elg.Stop()
		}
		if a.listenFd != 0 {
			unix.Close(a.listenFd)
		}
		close(a.connCh)
	})
	return nil
}

func (a *epollAcceptor) handleNewConn(clientFd int) {
	defer func() {
		if r := recover(); r != nil {
			unix.Close(clientFd)
		}
	}()

	handshakeFd, err := conn.ServerHandshakeFD(clientFd)
	if err != nil {
		unix.Close(clientFd)
		return
	}

	if a.config.TCPNoDelay {
		unix.SetsockoptInt(handshakeFd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	}
	if a.config.TCPQuickAck {
		unix.SetsockoptInt(handshakeFd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
	}

	ec := conn.NewEpollConn(handshakeFd, false, conn.NextConnID())
	if a.config.MaxFrameSize > 0 {
		ec.SetMaxFrameSize(a.config.MaxFrameSize)
	}

	el := a.elg.Next()
	ec.SetEventLoop(el)
	el.Register(handshakeFd, &conn.EventHandlerAdapter{Conn: ec})

	a.connCh <- &acceptedConn{c: ec, nc: nil}
}

type acceptHandler struct {
	acceptor *epollAcceptor
}

func (h *acceptHandler) OnEvent(fd int, events uint32) {
	if events&(eventloop.EventError|eventloop.EventHup) != 0 {
		return
	}
	if events&eventloop.EventRead == 0 {
		return
	}

	for atomic.LoadInt32(&h.acceptor.running) == 1 {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				return
			}
			return
		}
		h.acceptor.handleNewConn(nfd)
	}
}

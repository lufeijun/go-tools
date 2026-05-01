//go:build linux

package client

import (
	"fmt"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/eventloop"
	"golang.org/x/sys/unix"
)

func newEventLoopGroupForClient(cfg ws.Config) eventloop.EventLoopGroup {
	workers := cfg.EventLoopWorkerCount()
	return eventloop.NewEventLoopGroup(workers, func() eventloop.Poller {
		return eventloop.NewEpollPoller()
	})
}

func (c *defaultClient) doConnectEpoll() error {
	// Extract host:port from the ws:// URL
	u, host := parseWSAddr(c.config.Addr)
	if host == "" {
		return fmt.Errorf("invalid addr: %s", c.config.Addr)
	}

	fd, err := conn.DialNonBlock(host)
	if err != nil {
		return fmt.Errorf("dial non-block: %w", err)
	}

	// Wait for connection to complete
	if err := waitForConnect(fd); err != nil {
		unix.Close(fd)
		return fmt.Errorf("connect wait: %w", err)
	}

	// Perform WebSocket handshake on the raw fd
	if err := conn.ClientHandshakeFD(fd, u, c.config.Headers); err != nil {
		unix.Close(fd)
		return fmt.Errorf("client handshake fd: %w", err)
	}

	// Apply TCP options
	if c.config.TCPNoDelay {
		unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	}
	if c.config.TCPQuickAck {
		unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_QUICKACK, 1)
	}

	ec := conn.NewEpollConn(fd, true, conn.NextConnID())
	if c.config.MaxFrameSize > 0 {
		ec.SetMaxFrameSize(c.config.MaxFrameSize)
	}

	el := c.elg.Next()
	ec.SetEventLoop(el)
	el.Register(fd, &conn.EventHandlerAdapter{Conn: ec})

	sess := c.initSession(ec)

	go c.serveConnEpoll(sess)

	c.sess = sess
	if c.onConnect != nil {
		c.onConnect(sess)
	}
	return nil
}

func waitForConnect(fd int) error {
	poller := eventloop.NewEpollPoller()
	if err := poller.Open(); err != nil {
		return err
	}
	defer poller.Close()

	// Add fd for write events to detect connect completion
	if err := poller.Add(fd, eventloop.EventWrite); err != nil {
		return err
	}

	events, err := poller.Wait(-1)
	if err != nil {
		return err
	}
	_ = events

	// Check for connection error via SO_ERROR
	val, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ERROR)
	if err != nil {
		return err
	}
	if val != 0 {
		return fmt.Errorf("connect failed: errno %d", val)
	}
	return nil
}

func parseWSAddr(rawURL string) (string, string) {
	// Simple parser: extract host:port from ws://host:port/path
	u := rawURL
	if len(u) > 5 && u[:5] == "ws://" {
		u = u[5:]
	} else if len(u) > 6 && u[:6] == "wss://" {
		u = u[6:]
	}
	host := u
	for i := 0; i < len(u); i++ {
		if u[i] == '/' {
			host = u[:i]
			break
		}
	}
	return rawURL, host
}

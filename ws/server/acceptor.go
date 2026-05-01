package server

import (
	"net"
	"net/http"
	"sync"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
)

// Acceptor abstracts how the server listens for and accepts connections.
type Acceptor interface {
	Listen(addr string) error
	Accept() (conn.Conn, net.Conn, error) // net.Conn is nil in epoll mode
	Close() error
}

// acceptedConn holds a connection accepted by netAcceptor.
type acceptedConn struct {
	c  conn.Conn
	nc net.Conn
}

// netAcceptor uses net/http + Hijack for the standard WebSocket upgrade flow.
type netAcceptor struct {
	config   ws.Config
	server   *http.Server
	listener net.Listener
	connCh   chan *acceptedConn
	once     sync.Once
}

func newNetAcceptor(cfg ws.Config) *netAcceptor {
	return &netAcceptor{
		config: cfg,
		connCh: make(chan *acceptedConn, 128),
	}
}

func (a *netAcceptor) Addr() net.Addr {
	if a.listener != nil {
		return a.listener.Addr()
	}
	return nil
}

func (a *netAcceptor) Listener() net.Listener {
	return a.listener
}

func (a *netAcceptor) Listen(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleUpgrade)

	a.server = &http.Server{Handler: mux}

	var err error
	if a.config.SOReusePort {
		a.listener, err = conn.ListenTCPWithReusePort(addr)
	} else {
		a.listener, err = net.Listen("tcp", addr)
	}
	if err != nil {
		return err
	}

	go a.server.Serve(a.listener)
	return nil
}

func (a *netAcceptor) Accept() (conn.Conn, net.Conn, error) {
	ac := <-a.connCh
	return ac.c, ac.nc, nil
}

func (a *netAcceptor) Close() error {
	a.once.Do(func() {
		if a.server != nil {
			a.server.Close()
		}
		close(a.connCh)
	})
	return nil
}

func (a *netAcceptor) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	nc, err := conn.ServerHandshake(w, r)
	if err != nil {
		return
	}
	if err := conn.ApplyTCPOptions(nc, a.config.TCPNoDelay, a.config.TCPQuickAck); err != nil {
		nc.Close()
		return
	}

	c := conn.NewNetConn(nc, false, conn.NextConnID())
	a.connCh <- &acceptedConn{c: c, nc: nc}
}

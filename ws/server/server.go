package server

import (
	"context"
	"net"
	"net/http"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/hub"
	"github.com/lufeijun/goTools/ws/session"
)

// Server is the WebSocket server.
type Server interface {
	Config() ws.Config
	Hub() hub.Hub
	Start() error
	Stop() error
	Listener() net.Listener
}

// NewServer creates a Server with the given config.
func NewServer(cfg ws.Config) Server {
	if cfg.ReadBufferSize == 0 {
		defaults := ws.DefaultConfig()
		cfg.ReadBufferSize = defaults.ReadBufferSize
		cfg.WriteBufferSize = defaults.WriteBufferSize
		cfg.PingInterval = defaults.PingInterval
		cfg.PongTimeout = defaults.PongTimeout
		cfg.MaxFrameSize = defaults.MaxFrameSize
		cfg.EventLoopWorkers = defaults.EventLoopWorkers
		cfg.EventLoopStrategy = defaults.EventLoopStrategy
		cfg.BufferPoolSmall = defaults.BufferPoolSmall
		cfg.BufferPoolDefault = defaults.BufferPoolDefault
		cfg.BufferPoolLarge = defaults.BufferPoolLarge
		cfg.ReconnectInterval = defaults.ReconnectInterval
		cfg.MaxReconnect = defaults.MaxReconnect
	}
	return &defaultServer{
		config: cfg,
		hub:    hub.NewHub(32),
	}
}

type defaultServer struct {
	config   ws.Config
	hub      hub.Hub
	listener net.Listener
	server   *http.Server
}

func (s *defaultServer) Config() ws.Config     { return s.config }
func (s *defaultServer) Hub() hub.Hub          { return s.hub }
func (s *defaultServer) Listener() net.Listener { return s.listener }

func (s *defaultServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWebSocket)

	s.server = &http.Server{Handler: mux}
	var err error
	s.listener, err = net.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}
	return s.server.Serve(s.listener)
}

func (s *defaultServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
		http.Error(w, "Too many connections", http.StatusServiceUnavailable)
		return
	}

	nc, err := conn.ServerHandshake(w, r)
	if err != nil {
		return
	}

	c := conn.NewNetConn(nc, false, conn.NextConnID())
	sess := session.NewSession(c, session.Config{
		PingInterval: s.config.PingInterval,
		PongTimeout:  s.config.PongTimeout,
	})

	hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		c.Close()
	})
	sess.SetState(session.StateConnected)
	hb.Start(sess)

	s.hub.Register(sess)

	// TODO: start read loop for netConn (V2.1)
	// For now, the connection is established but no frame processing loop runs
}

func (s *defaultServer) Stop() error {
	if s.server != nil {
		return s.server.Shutdown(context.Background())
	}
	return nil
}

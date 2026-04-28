package ws

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type ServerConfig struct {
	Addr             string
	PingInterval     time.Duration
	PongTimeout      time.Duration
	MaxConnections   int
	HandshakeTimeout time.Duration
	ReadBufferSize   int
	WriteBufferSize  int
}

func defaultServerConfig(cfg ServerConfig) ServerConfig {
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 60 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.ReadBufferSize == 0 {
		cfg.ReadBufferSize = 4096
	}
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = 4096
	}
	return cfg
}

type Server struct {
	config   ServerConfig
	hub      *Hub
	listener net.Listener
	connChan chan *session.Session
	server   *http.Server
}

func NewServer(cfg ServerConfig) *Server {
	cfg = defaultServerConfig(cfg)
	return &Server{
		config:   cfg,
		hub:      NewHub(),
		connChan: make(chan *session.Session, 64),
	}
}

func (s *Server) Hub() *Hub                        { return s.hub }
func (s *Server) ConnChan() <-chan *session.Session { return s.connChan }
func (s *Server) Listener() net.Listener            { return s.listener }

func (s *Server) ListenAndServe() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
			http.Error(w, "Too many connections", http.StatusServiceUnavailable)
			return
		}

		c, err := conn.ServerHandshake(w, r)
		if err != nil {
			return
		}

		sess := session.NewSession(c, session.SessionConfig{
			PingInterval: s.config.PingInterval,
			PongTimeout:  s.config.PongTimeout,
		})
		hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
		sess.SetHeartbeater(hb)
		sess.SetState(session.StateConnected)
		hb.Start(c)

		s.connChan <- sess
	})

	s.server = &http.Server{Handler: mux}

	var err error
	s.listener, err = net.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}

	return s.server.Serve(s.listener)
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.hub.Stop()
	if s.server != nil {
		return s.server.Shutdown(ctx)
	}
	return nil
}

package server

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/frame"
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
	OnConnect(fn func(session.Session))
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
	config    ws.Config
	hub       hub.Hub
	listener  net.Listener
	server    *http.Server
	onConnect func(session.Session)
}

func (s *defaultServer) Config() ws.Config     { return s.config }
func (s *defaultServer) Hub() hub.Hub          { return s.hub }
func (s *defaultServer) Listener() net.Listener { return s.listener }
func (s *defaultServer) OnConnect(fn func(session.Session)) { s.onConnect = fn }

func (s *defaultServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleWebSocket)

	s.server = &http.Server{Handler: mux}
	var err error
	if s.config.SOReusePort {
		s.listener, err = conn.ListenTCPWithReusePort(s.config.Addr)
	} else {
		s.listener, err = net.Listen("tcp", s.config.Addr)
	}
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
	if err := conn.ApplyTCPOptions(nc, s.config.TCPNoDelay, s.config.TCPQuickAck); err != nil {
		nc.Close()
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

	// Add frame codec so handlers can write *Message back as WebSocket frames.
	sess.Conn().Pipeline().AddLast("codec", &conn.FrameCodec{Writer: nc, IsClient: false})

	if s.onConnect != nil {
		s.onConnect(sess)
	}

	go s.serveConn(sess, nc)
}

func (s *defaultServer) serveConn(sess session.Session, nc net.Conn) {
	defer func() {
		sess.Close()
		s.hub.Unregister(sess.Conn().ID())
	}()

	readTimeout := s.config.PongTimeout * 2
	if readTimeout == 0 {
		readTimeout = 120 * time.Second
	}

	br := bufio.NewReaderSize(nc, 65536)

	for {
		nc.SetReadDeadline(time.Now().Add(readTimeout))
		f, err := frame.ReadFrameLimit(br, s.config.MaxFrameSize)
		if err != nil {
			return
		}

		switch f.Opcode {
		case frame.OpcodeText, frame.OpcodeBinary:
			msg := &conn.Message{Type: byte(f.Opcode), Data: f.Payload}
			sess.Conn().Pipeline().FireChannelRead(msg)

		case frame.OpcodePing:
			_ = frame.WriteFrame(nc, frame.NewPongFrame(f.Payload))

		case frame.OpcodeClose:
			code := uint16(1000)
			reason := ""
			if len(f.Payload) >= 2 {
				code = binary.BigEndian.Uint16(f.Payload[:2])
				reason = string(f.Payload[2:])
			}
			_ = frame.WriteFrame(nc, frame.NewCloseFrame(code, reason))
			return
		}
	}
}

func (s *defaultServer) Stop() error {
	if s.server != nil {
		return s.server.Shutdown(context.Background())
	}
	return nil
}

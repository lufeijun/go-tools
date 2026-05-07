package server

import (
	"bufio"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/hub"
	"github.com/lufeijun/goTools/ws/session"
)

type Server interface {
	Config() ws.Config
	Hub() hub.Hub
	Start() error
	Stop() error
	Listener() net.Listener
	OnConnect(fn func(session.Session))
}

func NewServer(cfg ws.Config) (Server, error) {
	if err := cfg.ValidateMode(); err != nil {
		return nil, err
	}
	defaults := ws.DefaultConfig()
	if cfg.ReadBufferSize == 0 {
		cfg.ReadBufferSize = defaults.ReadBufferSize
	}
	if cfg.WriteBufferSize == 0 {
		cfg.WriteBufferSize = defaults.WriteBufferSize
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = defaults.PingInterval
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = defaults.PongTimeout
	}
	if cfg.MaxFrameSize == 0 {
		cfg.MaxFrameSize = defaults.MaxFrameSize
	}
	if cfg.EventLoopWorkers == 0 {
		cfg.EventLoopWorkers = defaults.EventLoopWorkers
	}
	if cfg.EventLoopStrategy == "" {
		cfg.EventLoopStrategy = defaults.EventLoopStrategy
	}
	if cfg.BufferPoolSmall == 0 {
		cfg.BufferPoolSmall = defaults.BufferPoolSmall
	}
	if cfg.BufferPoolDefault == 0 {
		cfg.BufferPoolDefault = defaults.BufferPoolDefault
	}
	if cfg.BufferPoolLarge == 0 {
		cfg.BufferPoolLarge = defaults.BufferPoolLarge
	}
	if cfg.ReconnectInterval == 0 {
		cfg.ReconnectInterval = defaults.ReconnectInterval
	}
	if cfg.MaxReconnect == 0 {
		cfg.MaxReconnect = defaults.MaxReconnect
	}

	acceptor := newAcceptorForMode(cfg)

	return &defaultServer{
		config:   cfg,
		hub:      hub.NewHub(32),
		acceptor: acceptor,
	}, nil
}

type defaultServer struct {
	config    ws.Config
	hub       hub.Hub
	acceptor  Acceptor
	onConnect func(session.Session)
	wg        sync.WaitGroup
}

func (s *defaultServer) Config() ws.Config                  { return s.config }
func (s *defaultServer) Hub() hub.Hub                       { return s.hub }
func (s *defaultServer) OnConnect(fn func(session.Session)) { s.onConnect = fn }

func (s *defaultServer) Listener() net.Listener {
	if na, ok := s.acceptor.(*netAcceptor); ok {
		return na.Listener()
	}
	return nil
}

func (s *defaultServer) Start() error {
	if err := s.acceptor.Listen(s.config.Addr); err != nil {
		return err
	}
	go s.acceptLoop()
	return nil
}

func (s *defaultServer) acceptLoop() {
	for {
		c, nc, err := s.acceptor.Accept()
		if err != nil {
			return
		}

		if s.config.MaxConnections > 0 && s.hub.Count() >= s.config.MaxConnections {
			c.Close()
			continue
		}

		sess := s.initSession(c, nc)
		s.wg.Add(1)
		go s.serveConn(sess, nc)
	}
}

func (s *defaultServer) initSession(c conn.Conn, nc net.Conn) session.Session {
	sess := session.NewSession(c, session.Config{
		PingInterval: s.config.PingInterval,
		PongTimeout:  s.config.PongTimeout,
	})

	hb := session.NewPerConnHeartbeater(s.config.PingInterval, s.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		c.Close()
	})
	sess.SetHeartbeater(hb)
	sess.SetState(session.StateConnecting)
	sess.SetState(session.StateConnected)
	hb.Start(sess)

	s.hub.Register(sess)

	sess.Conn().Pipeline().AddFirst("headWriter", &conn.ConnWriter{Conn: c})
	sess.Conn().Pipeline().AddLast("codec", &conn.FrameCodec{IsClient: false})

	if nc == nil {
		s.setupEpollFrameCallback(c, sess)
	}

	if s.onConnect != nil {
		s.onConnect(sess)
	}

	return sess
}

func (s *defaultServer) setupEpollFrameCallback(c conn.Conn, sess session.Session) {
	if edc, ok := c.(conn.EventDrivenConn); ok {
		edc.SetOnClose(func() {
			sess.SetState(session.StateDisconnected)
		})
		edc.SetOnFrame(func(f frame.Frame) {
			switch f.Opcode {
			case frame.OpcodeText, frame.OpcodeBinary:
				msg := &conn.Message{Type: byte(f.Opcode), Data: f.Payload}
				sess.Conn().Pipeline().FireChannelRead(msg)
			case frame.OpcodePing:
				pongMsg := &conn.Message{Type: byte(frame.OpcodePong), Data: f.Payload}
				sess.Conn().Pipeline().FireChannelWrite(pongMsg)
			case frame.OpcodeClose:
				code := uint16(1000)
				reason := ""
				if len(f.Payload) >= 2 {
					code = binary.BigEndian.Uint16(f.Payload[:2])
					reason = string(f.Payload[2:])
				}
				closeMsg := &conn.Message{Type: byte(frame.OpcodeClose), Status: code, Data: []byte(reason)}
				sess.Conn().Pipeline().FireChannelWrite(closeMsg)
				c.Close()
			}
		})
	}
}

func (s *defaultServer) serveConn(sess session.Session, nc net.Conn) {
	defer func() {
		sess.SetState(session.StateDisconnected)
		sess.Conn().Pipeline().FireChannelInactive()
		sess.Close()
		s.hub.Unregister(sess.Conn().ID())
		s.wg.Done()
	}()

	if nc == nil {
		// epoll mode: frame reading driven by eventloop, this goroutine waits for close
		for st := range sess.StateChan() {
			if st == session.StateClosed || st == session.StateDisconnected {
				return
			}
		}
		return
	}

	s.serveConnNet(sess, nc)
}

func (s *defaultServer) serveConnNet(sess session.Session, nc net.Conn) {
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
	if err := s.acceptor.Close(); err != nil {
		return err
	}
	// Close all registered connections so serveConn goroutines can exit.
	s.hub.CloseAll()
	s.wg.Wait()
	// Shut down hub background workers.
	s.hub.Close()
	return nil
}

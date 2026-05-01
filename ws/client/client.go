package client

import (
	"bufio"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/eventloop"
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/session"
)

type Client interface {
	Config() ws.Config
	Connect() error
	Close() error
	Session() session.Session
	OnConnect(fn func(session.Session))
}

func NewClient(cfg ws.Config) (Client, error) {
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

	c := &defaultClient{config: cfg}
	if cfg.Mode == ws.ModeEpoll {
		c.elg = newEventLoopGroupForClient(cfg)
	}
	return c, nil
}

type defaultClient struct {
	config    ws.Config
	sess      session.Session
	onConnect func(session.Session)
	mu        sync.Mutex
	closed    bool
	elg       eventloop.EventLoopGroup
}

func (c *defaultClient) Config() ws.Config        { return c.config }
func (c *defaultClient) Session() session.Session { return c.sess }
func (c *defaultClient) OnConnect(fn func(session.Session)) {
	c.onConnect = fn
}

func (c *defaultClient) Connect() error {
	c.mu.Lock()
	c.closed = false
	c.mu.Unlock()

	if c.elg != nil {
		if err := c.elg.Start(); err != nil {
			return err
		}
	}

	return c.doConnect()
}

func (c *defaultClient) doConnect() error {
	switch c.config.Mode {
	case ws.ModeEpoll:
		return c.doConnectEpoll()
	default:
		return c.doConnectNet()
	}
}

func (c *defaultClient) doConnectNet() error {
	nc, err := conn.ClientHandshake(c.config.Addr, c.config.Headers)
	if err != nil {
		return err
	}
	if err := conn.ApplyTCPOptions(nc, c.config.TCPNoDelay, c.config.TCPQuickAck); err != nil {
		nc.Close()
		return err
	}

	wc := conn.NewNetConn(nc, true, 1)
	sess := c.initSession(wc)

	go c.serveConnNet(nc, sess)

	c.sess = sess
	if c.onConnect != nil {
		c.onConnect(sess)
	}
	return nil
}

func (c *defaultClient) initSession(cn conn.Conn) session.Session {
	sess := session.NewSession(cn, session.Config{
		PingInterval:      c.config.PingInterval,
		PongTimeout:       c.config.PongTimeout,
		ReconnectInterval: c.config.ReconnectInterval,
		MaxReconnect:      c.config.MaxReconnect,
	})

	hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		cn.Close()
	})
	sess.SetState(session.StateConnected)
	hb.Start(sess)

	cn.Pipeline().AddFirst("headWriter", &conn.ConnWriter{Conn: cn})
	cn.Pipeline().AddLast("codec", &conn.FrameCodec{IsClient: true})

	if edc, ok := cn.(conn.EventDrivenConn); ok {
		c.setupEpollFrameCallback(edc, sess)
	}

	return sess
}

func (c *defaultClient) setupEpollFrameCallback(edc conn.EventDrivenConn, sess session.Session) {
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
			sess.Conn().Close()
		}
	})
}

func (c *defaultClient) serveConnNet(nc net.Conn, sess session.Session) {
	defer func() {
		sess.Close()
		c.maybeReconnect()
	}()

	readTimeout := c.config.PongTimeout * 2
	if readTimeout == 0 {
		readTimeout = 120 * time.Second
	}

	br := bufio.NewReaderSize(nc, 65536)

	for {
		nc.SetReadDeadline(time.Now().Add(readTimeout))
		f, err := frame.ReadFrameLimit(br, c.config.MaxFrameSize)
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

func (c *defaultClient) serveConnEpoll(sess session.Session) {
	defer func() {
		sess.Close()
		c.maybeReconnect()
	}()
	for st := range sess.StateChan() {
		if st == session.StateClosed || st == session.StateDisconnected {
			return
		}
	}
}

func (c *defaultClient) maybeReconnect() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	go func() {
		backoff := c.config.ReconnectInterval
		for i := 0; i < c.config.MaxReconnect; i++ {
			time.Sleep(backoff)

			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return
			}
			c.mu.Unlock()

			if err := c.doConnect(); err == nil {
				return
			}

			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
		if c.sess != nil {
			c.sess.SetState(session.StateClosed)
		}
	}()
}

func (c *defaultClient) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if c.elg != nil {
		c.elg.Stop()
	}
	if c.sess != nil {
		return c.sess.Close()
	}
	return nil
}

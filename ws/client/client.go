package client

import (
	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/session"
)

// Client is the WebSocket client.
type Client interface {
	Config() ws.Config
	Connect() error
	Close() error
	Session() session.Session
}

// NewClient creates a Client with the given config.
func NewClient(cfg ws.Config) Client {
	if cfg.ReadBufferSize == 0 {
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
	}
	return &defaultClient{config: cfg}
}

type defaultClient struct {
	config ws.Config
	sess   session.Session
}

func (c *defaultClient) Config() ws.Config      { return c.config }
func (c *defaultClient) Session() session.Session { return c.sess }

func (c *defaultClient) Connect() error {
	nc, err := conn.ClientHandshake(c.config.Addr, c.config.Headers)
	if err != nil {
		return err
	}

	wc := conn.NewNetConn(nc, true, 1)
	sess := session.NewSession(wc, session.Config{
		PingInterval:      c.config.PingInterval,
		PongTimeout:       c.config.PongTimeout,
		ReconnectInterval: c.config.ReconnectInterval,
		MaxReconnect:      c.config.MaxReconnect,
	})

	hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
	hb.SetOnTimeout(func() {
		sess.SetState(session.StateDisconnected)
		wc.Close()
		// TODO: auto-reconnect (V2.1)
	})
	sess.SetState(session.StateConnected)
	hb.Start(sess)

	c.sess = sess
	return nil
}

func (c *defaultClient) Close() error {
	if c.sess != nil {
		return c.sess.Close()
	}
	return nil
}

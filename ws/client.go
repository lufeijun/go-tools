package ws

import (
	"net/http"
	"time"

	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type ClientConfig struct {
	URL               string
	Headers           http.Header
	PingInterval      time.Duration
	PongTimeout       time.Duration
	ReconnectInterval time.Duration
	MaxReconnect      int
}

func defaultClientConfig(cfg ClientConfig) ClientConfig {
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 30 * time.Second
	}
	if cfg.PongTimeout == 0 {
		cfg.PongTimeout = 60 * time.Second
	}
	if cfg.ReconnectInterval == 0 {
		cfg.ReconnectInterval = 5 * time.Second
	}
	if cfg.MaxReconnect == 0 {
		cfg.MaxReconnect = 5
	}
	return cfg
}

type Client struct {
	config  ClientConfig
	session *session.Session
	headers http.Header
}

func NewClient(cfg ClientConfig) *Client {
	cfg = defaultClientConfig(cfg)
	return &Client{
		config:  cfg,
		headers: cfg.Headers,
	}
}

func (c *Client) Connect() error {
	wc, err := conn.ClientHandshake(c.config.URL, c.headers)
	if err != nil {
		return err
	}

	sess := session.NewSession(wc, session.SessionConfig{
		PingInterval:      c.config.PingInterval,
		PongTimeout:       c.config.PongTimeout,
		ReconnectInterval: c.config.ReconnectInterval,
		MaxReconnect:      c.config.MaxReconnect,
	})

	hb := session.NewPerConnHeartbeater(c.config.PingInterval, c.config.PongTimeout)
	sess.SetHeartbeater(hb)
	sess.SetState(session.StateConnected)
	hb.Start(wc)

	c.session = sess
	return nil
}

func (c *Client) ReadChan() <-chan Message {
	if c.session == nil {
		return nil
	}
	return c.session.ReadChan()
}

func (c *Client) WriteChan() chan<- Message {
	if c.session == nil {
		return nil
	}
	return c.session.WriteChan()
}

func (c *Client) StateChan() <-chan State {
	if c.session == nil {
		return nil
	}
	return c.session.StateChan()
}

func (c *Client) Send(msg Message) {
	if c.session != nil {
		c.session.WriteChan() <- msg
	}
}

func (c *Client) Close() error {
	if c.session != nil {
		return c.session.Close()
	}
	return nil
}

//go:build !linux

package client

import (
	"errors"

	"github.com/lufeijun/goTools/ws"
	"github.com/lufeijun/goTools/ws/eventloop"
)

func newEventLoopGroupForClient(cfg ws.Config) eventloop.EventLoopGroup {
	return nil
}

func (c *defaultClient) doConnectEpoll() error {
	return errors.New("epoll mode is only supported on Linux")
}

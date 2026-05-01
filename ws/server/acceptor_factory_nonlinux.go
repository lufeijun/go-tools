//go:build !linux

package server

import "github.com/lufeijun/goTools/ws"

func newAcceptorForMode(cfg ws.Config) Acceptor {
	return newNetAcceptor(cfg)
}

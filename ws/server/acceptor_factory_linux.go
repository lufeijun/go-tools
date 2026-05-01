//go:build linux

package server

import "github.com/lufeijun/goTools/ws"

func newAcceptorForMode(cfg ws.Config) Acceptor {
    switch cfg.Mode {
    case ws.ModeEpoll:
        return newEpollAcceptor(cfg)
    default:
        return newNetAcceptor(cfg)
    }
}

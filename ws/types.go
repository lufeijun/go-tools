package ws

import (
	"github.com/lufeijun/goTools/ws/frame"
	"github.com/lufeijun/goTools/ws/internal/conn"
	"github.com/lufeijun/goTools/ws/internal/session"
)

type Opcode = frame.Opcode
type Message = conn.Message
type State = session.State
type Conn = conn.Conn

const (
	OpcodeText   = frame.OpcodeText
	OpcodeBinary = frame.OpcodeBinary
	OpcodeClose  = frame.OpcodeClose
	OpcodePing   = frame.OpcodePing
	OpcodePong   = frame.OpcodePong
)

const (
	StateDisconnected = session.StateDisconnected
	StateConnecting   = session.StateConnecting
	StateConnected    = session.StateConnected
	StateReconnecting = session.StateReconnecting
	StateClosed       = session.StateClosed
)

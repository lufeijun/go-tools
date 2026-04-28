package frame

import (
	"encoding/binary"
	"io"
)

type Opcode byte

const (
	OpcodeText   Opcode = 0x1
	OpcodeBinary Opcode = 0x2
	OpcodeClose  Opcode = 0x8
	OpcodePing   Opcode = 0x9
	OpcodePong   Opcode = 0xA
)

type Frame struct {
	FIN     bool
	RSV1    bool
	RSV2    bool
	RSV3    bool
	Opcode  Opcode
	Masked  bool
	MaskKey [4]byte
	Payload []byte
}

func NewTextFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodeText, Payload: data}
}

func NewBinaryFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodeBinary, Payload: data}
}

func NewPingFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodePing, Payload: data}
}

func NewPongFrame(data []byte) Frame {
	return Frame{FIN: true, Opcode: OpcodePong, Payload: data}
}

func NewCloseFrame(code uint16, reason string) Frame {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload[:2], code)
	copy(payload[2:], reason)
	return Frame{FIN: true, Opcode: OpcodeClose, Payload: payload}
}

func WriteFrame(w io.Writer, f Frame) error {
	header := make([]byte, 0, 14)

	b1 := byte(f.Opcode)
	if f.FIN {
		b1 |= 0x80
	}
	if f.RSV1 {
		b1 |= 0x40
	}
	if f.RSV2 {
		b1 |= 0x20
	}
	if f.RSV3 {
		b1 |= 0x10
	}
	header = append(header, b1)

	b2 := byte(0)
	if f.Masked {
		b2 |= 0x80
	}

	payloadLen := len(f.Payload)
	switch {
	case payloadLen <= 125:
		b2 |= byte(payloadLen)
		header = append(header, b2)
	case payloadLen <= 65535:
		b2 |= 126
		header = append(header, b2)
		lenBytes := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBytes, uint16(payloadLen))
		header = append(header, lenBytes...)
	default:
		b2 |= 127
		header = append(header, b2)
		lenBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(lenBytes, uint64(payloadLen))
		header = append(header, lenBytes...)
	}

	if f.Masked {
		header = append(header, f.MaskKey[:]...)
	}

	if _, err := w.Write(header); err != nil {
		return err
	}

	if payloadLen == 0 {
		return nil
	}

	if f.Masked {
		masked := applyMask(f.Payload, f.MaskKey)
		_, err := w.Write(masked)
		return err
	}

	_, err := w.Write(f.Payload)
	return err
}

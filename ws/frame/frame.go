package frame

import (
	"encoding/binary"
	"fmt"
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

// DefaultMaxFrameSize is the default maximum allowed frame payload (64 MB).
const DefaultMaxFrameSize = 64 * 1024 * 1024

// MaxFrameSizeError is returned when a frame payload exceeds the configured limit.
type MaxFrameSizeError struct {
	Limit   int
	Payload int
}

func (e *MaxFrameSizeError) Error() string {
	return fmt.Sprintf("frame payload %d exceeds limit %d", e.Payload, e.Limit)
}

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
	buf := make([]byte, 0, 14+len(f.Payload))

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
	buf = append(buf, b1)

	b2 := byte(0)
	if f.Masked {
		b2 |= 0x80
	}

	payloadLen := len(f.Payload)
	switch {
	case payloadLen <= 125:
		b2 |= byte(payloadLen)
		buf = append(buf, b2)
	case payloadLen <= 65535:
		b2 |= 126
		buf = append(buf, b2)
		lenBytes := make([]byte, 2)
		binary.BigEndian.PutUint16(lenBytes, uint16(payloadLen))
		buf = append(buf, lenBytes...)
	default:
		b2 |= 127
		buf = append(buf, b2)
		lenBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(lenBytes, uint64(payloadLen))
		buf = append(buf, lenBytes...)
	}

	if f.Masked {
		buf = append(buf, f.MaskKey[:]...)
		buf = append(buf, applyMask(f.Payload, f.MaskKey)...)
	} else if payloadLen > 0 {
		buf = append(buf, f.Payload...)
	}

	_, err := w.Write(buf)
	return err
}

func ReadFrame(r io.Reader) (Frame, error) {
	return ReadFrameLimit(r, DefaultMaxFrameSize)
}

// ReadFrameLimit reads a single WebSocket frame with a maximum payload size limit.
// It also enforces the cumulative limit for fragmented messages.
func ReadFrameLimit(r io.Reader, maxPayload int) (Frame, error) {
	return readFrameWithAccumulated(r, maxPayload, 0)
}

func readFrameWithAccumulated(r io.Reader, maxPayload, accumulatedLen int) (Frame, error) {
	var result Frame
	var header [2]byte

	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}

	result.FIN = header[0]&0x80 != 0
	result.RSV1 = header[0]&0x40 != 0
	result.RSV2 = header[0]&0x20 != 0
	result.RSV3 = header[0]&0x10 != 0
	result.Opcode = Opcode(header[0] & 0x0F)

	result.Masked = header[1]&0x80 != 0
	payloadLen := int(header[1] & 0x7F)

	switch payloadLen {
	case 126:
		var lenBytes [2]byte
		if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
			return Frame{}, err
		}
		payloadLen = int(binary.BigEndian.Uint16(lenBytes[:]))
	case 127:
		var lenBytes [8]byte
		if _, err := io.ReadFull(r, lenBytes[:]); err != nil {
			return Frame{}, err
		}
		payloadLen = int(binary.BigEndian.Uint64(lenBytes[:]))
	}

	if maxPayload > 0 && payloadLen > maxPayload {
		return Frame{}, &MaxFrameSizeError{Limit: maxPayload, Payload: payloadLen}
	}

	if result.Masked {
		if _, err := io.ReadFull(r, result.MaskKey[:]); err != nil {
			return Frame{}, err
		}
	}

	if payloadLen > 0 {
		result.Payload = make([]byte, payloadLen)
		if _, err := io.ReadFull(r, result.Payload); err != nil {
			return Frame{}, err
		}
		if result.Masked {
			result.Payload = applyMask(result.Payload, result.MaskKey)
			result.Masked = false
		}
	}

	// Handle fragmentation: non-FIN frames continue reading continuation frames
	if !result.FIN {
		firstOpcode := result.Opcode
		accumulated := result.Payload
		totalLen := accumulatedLen + len(accumulated)
		if maxPayload > 0 && totalLen > maxPayload {
			return Frame{}, &MaxFrameSizeError{Limit: maxPayload, Payload: totalLen}
		}

		for {
			next, err := readFrameWithAccumulated(r, maxPayload, totalLen)
			if err != nil {
				return Frame{}, err
			}
			accumulated = append(accumulated, next.Payload...)
			totalLen += len(next.Payload)
			if maxPayload > 0 && totalLen > maxPayload {
				return Frame{}, &MaxFrameSizeError{Limit: maxPayload, Payload: totalLen}
			}
			if next.FIN {
				result.FIN = true
				result.Opcode = firstOpcode
				result.Payload = accumulated
				break
			}
		}
	}

	return result, nil
}

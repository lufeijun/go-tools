package frame

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lufeijun/goTools/ws/buf"
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
			if next.Opcode != 0x0 {
				return Frame{}, &ProtocolError{Code: 1002, Message: "expected continuation frame"}
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

// WriteFrameTo serializes a WebSocket frame into dst without allocating.
// It is suitable for use with pooled ByteBufs.
func WriteFrameTo(dst buf.ByteBuf, f Frame) error {
	headerSize := 2
	payloadLen := len(f.Payload)
	switch {
	case payloadLen <= 125:
		// headerSize stays 2
	case payloadLen <= 65535:
		headerSize += 2
	default:
		headerSize += 8
	}
	if f.Masked {
		headerSize += 4
	}

	dst.EnsureWritable(headerSize + payloadLen)

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
	dst.WriteByte(b1)

	b2 := byte(0)
	if f.Masked {
		b2 |= 0x80
	}

	switch {
	case payloadLen <= 125:
		b2 |= byte(payloadLen)
		dst.WriteByte(b2)
	case payloadLen <= 65535:
		b2 |= 126
		dst.WriteByte(b2)
		var lenBytes [2]byte
		binary.BigEndian.PutUint16(lenBytes[:], uint16(payloadLen))
		dst.Write(lenBytes[:])
	default:
		b2 |= 127
		dst.WriteByte(b2)
		var lenBytes [8]byte
		binary.BigEndian.PutUint64(lenBytes[:], uint64(payloadLen))
		dst.Write(lenBytes[:])
	}

	if f.Masked {
		dst.Write(f.MaskKey[:])
		dst.Write(applyMask(f.Payload, f.MaskKey))
	} else if payloadLen > 0 {
		dst.Write(f.Payload)
	}

	return nil
}

// ReadFrameBuf reads a single WebSocket frame using a pooled ByteBuf.
// The returned Frame.Payload is backed by the returned ByteBuf; the caller
// MUST Release the ByteBuf after the Frame is no longer needed.
//
// This is a zero-copy read path intended for callers that manage ByteBuf
// lifetimes (e.g. Pipeline handlers).  For callers that do not use ByteBuf,
// ReadFrameLimit is simpler and safer.
func ReadFrameBuf(r io.Reader, pool buf.Pool, maxPayload int) (Frame, buf.ByteBuf, error) {
	return readFrameBufWithAccumulated(r, pool, maxPayload, 0)
}

func readFrameBufWithAccumulated(r io.Reader, pool buf.Pool, maxPayload, accumulatedLen int) (Frame, buf.ByteBuf, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, nil, err
	}

	var result Frame
	result.FIN = header[0]&0x80 != 0
	result.RSV1 = header[0]&0x40 != 0
	result.RSV2 = header[0]&0x20 != 0
	result.RSV3 = header[0]&0x10 != 0
	result.Opcode = Opcode(header[0] & 0x0F)
	result.Masked = header[1]&0x80 != 0
	payloadLen := int(header[1] & 0x7F)

	var extendedLen int
	switch payloadLen {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Frame{}, nil, err
		}
		payloadLen = int(binary.BigEndian.Uint16(b[:]))
		extendedLen = 2
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return Frame{}, nil, err
		}
		payloadLen = int(binary.BigEndian.Uint64(b[:]))
		extendedLen = 8
	}

	if maxPayload > 0 && payloadLen > maxPayload {
		return Frame{}, nil, &MaxFrameSizeError{Limit: maxPayload, Payload: payloadLen}
	}

	if result.Masked {
		if _, err := io.ReadFull(r, result.MaskKey[:]); err != nil {
			return Frame{}, nil, err
		}
	}

	headerSize := 2 + extendedLen
	if result.Masked {
		headerSize += 4
	}

	bb := pool.Get(headerSize + payloadLen)
	bb.Write(header[:])
	if extendedLen == 2 {
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(payloadLen))
		bb.Write(b[:])
	} else if extendedLen == 8 {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(payloadLen))
		bb.Write(b[:])
	}
	if result.Masked {
		bb.Write(result.MaskKey[:])
	}

	if payloadLen > 0 {
		bb.EnsureWritable(payloadLen)
		data := bb.Bytes()[bb.WriterIndex() : bb.WriterIndex()+payloadLen]
		if _, err := io.ReadFull(r, data); err != nil {
			bb.Release()
			return Frame{}, nil, err
		}
		if result.Masked {
			applyMaskInPlace(data, result.MaskKey)
			result.Masked = false
		}
		bb.SetWriterIndex(bb.WriterIndex() + payloadLen)
	}

	// Payload is the trailing bytes after the header.
	result.Payload = bb.Bytes()[headerSize : headerSize+payloadLen]

	if !result.FIN {
		// Fragmented frame: read continuation frames and accumulate into
		// a single ByteBuf.
		firstOpcode := result.Opcode
		totalLen := accumulatedLen + len(result.Payload)
		if maxPayload > 0 && totalLen > maxPayload {
			bb.Release()
			return Frame{}, nil, &MaxFrameSizeError{Limit: maxPayload, Payload: totalLen}
		}

		for {
			next, nextBB, err := readFrameBufWithAccumulated(r, pool, maxPayload, totalLen)
			if err != nil {
				bb.Release()
				return Frame{}, nil, err
			}
			if next.Opcode != 0x0 {
				nextBB.Release()
				bb.Release()
				return Frame{}, nil, &ProtocolError{Code: 1002, Message: "expected continuation frame"}
			}
			// Append next payload to bb.
			bb.Write(next.Payload)
			nextBB.Release()
			totalLen += len(next.Payload)
			if maxPayload > 0 && totalLen > maxPayload {
				bb.Release()
				return Frame{}, nil, &MaxFrameSizeError{Limit: maxPayload, Payload: totalLen}
			}
			if next.FIN {
				result.FIN = true
				result.Opcode = firstOpcode
				result.Payload = bb.Bytes()[headerSize:]
				break
			}
		}
	}

	return result, bb, nil
}

// ReadFrameFromBuf parses a single WebSocket frame from a ByteBuf using
// Peek+Skip for zero-copy header inspection.
//
// The returned Frame.Payload references the ByteBuf's backing array; the caller
// MUST ensure the ByteBuf outlives the Frame or copy the payload if needed.
// If the frame is masked, the payload is unmasked in-place inside the ByteBuf.
//
// If bb does not contain a complete frame, io.ErrShortBuffer is returned and
// bb's reader index is left unchanged (the function rewinds on failure).
//
// NOTE: This function does not handle fragmented messages.  For fragmentation
// support, use ReadFrame (blocking I/O) or IncrementalParser (non-blocking).
func ReadFrameFromBuf(bb buf.ByteBuf, maxPayload int) (Frame, error) {
	start := bb.ReaderIndex()
	if bb.ReadableBytes() < 2 {
		return Frame{}, io.ErrShortBuffer
	}
	header := bb.Peek(2)
	bb.Skip(2)

	var result Frame
	result.FIN = header[0]&0x80 != 0
	result.RSV1 = header[0]&0x40 != 0
	result.RSV2 = header[0]&0x20 != 0
	result.RSV3 = header[0]&0x10 != 0
	result.Opcode = Opcode(header[0] & 0x0F)
	result.Masked = header[1]&0x80 != 0
	payloadLen := int(header[1] & 0x7F)

	switch payloadLen {
	case 126:
		if bb.ReadableBytes() < 2 {
			bb.SetReaderIndex(start)
			return Frame{}, io.ErrShortBuffer
		}
		payloadLen = int(binary.BigEndian.Uint16(bb.Peek(2)))
		bb.Skip(2)
	case 127:
		if bb.ReadableBytes() < 8 {
			bb.SetReaderIndex(start)
			return Frame{}, io.ErrShortBuffer
		}
		payloadLen = int(binary.BigEndian.Uint64(bb.Peek(8)))
		bb.Skip(8)
	}

	if maxPayload > 0 && payloadLen > maxPayload {
		bb.SetReaderIndex(start)
		return Frame{}, &MaxFrameSizeError{Limit: maxPayload, Payload: payloadLen}
	}

	if result.Masked {
		if bb.ReadableBytes() < 4 {
			bb.SetReaderIndex(start)
			return Frame{}, io.ErrShortBuffer
		}
		copy(result.MaskKey[:], bb.Peek(4))
		bb.Skip(4)
	}

	if payloadLen > 0 {
		if bb.ReadableBytes() < payloadLen {
			bb.SetReaderIndex(start)
			return Frame{}, io.ErrShortBuffer
		}
		result.Payload = bb.Peek(payloadLen)
		if result.Masked {
			applyMaskInPlace(result.Payload, result.MaskKey)
			result.Masked = false
		}
		bb.Skip(payloadLen)
	}

	return result, nil
}

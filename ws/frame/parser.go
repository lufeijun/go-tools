package frame

import (
	"encoding/binary"
	"io"
)

// IncrementalParser parses WebSocket frames incrementally from byte streams.
// It handles fragmentation by accumulating non-FIN frames and emitting a single
// complete frame when the FIN bit is set.
type IncrementalParser struct {
	maxPayload int            // Maximum allowed payload size per frame/message
	buf        []byte         // Buffer for accumulating partial data
	err        error          // Last error encountered
	fragState  *fragmentState // State for handling fragmented messages
}

// fragmentState tracks the state of a fragmented message.
type fragmentState struct {
	opcode  Opcode // Opcode of the first frame (text/binary)
	payload []byte // Accumulated payload
}

// NewIncrementalParser creates a new incremental parser with the specified
// maximum payload size limit. Use DefaultMaxFrameSize for sensible defaults.
func NewIncrementalParser(maxPayload int) *IncrementalParser {
	return &IncrementalParser{
		maxPayload: maxPayload,
		buf:        make([]byte, 0, 4096), // Pre-allocate buffer to reduce allocations
	}
}

// Feed appends data to the internal buffer and tries to parse complete frames.
// Returns a slice of complete frames that were successfully parsed.
// If an error occurs, subsequent calls to Feed will return nil until Reset() is called.
func (p *IncrementalParser) Feed(data []byte) []Frame {
	if p.err != nil {
		return nil
	}

	// Append new data to buffer
	p.buf = append(p.buf, data...)

	var frames []Frame

	for {
		frame, err := p.tryParseFrame()
		if err == io.ErrShortBuffer {
			// Incomplete frame, wait for more data
			break
		}
		if err != nil {
			// Parsing error, stop processing and set error
			p.err = err
			break
		}

		frames = append(frames, frame)
	}

	return frames
}

// Err returns the last error encountered during parsing.
func (p *IncrementalParser) Err() error {
	return p.err
}

// Reset clears all internal state and errors, allowing the parser to be reused.
func (p *IncrementalParser) Reset() {
	p.buf = p.buf[:0] // Reuse buffer to avoid allocation
	p.err = nil
	p.fragState = nil
}

// tryParseFrame attempts to parse a single complete frame from the buffer.
// Returns io.ErrShortBuffer if there isn't enough data to complete a frame.
func (p *IncrementalParser) tryParseFrame() (Frame, error) {
	var result Frame

	// Check if we have enough data for at least the 2-byte header
	if len(p.buf) < 2 {
		return Frame{}, io.ErrShortBuffer
	}

	// Parse fixed header (2 bytes)
	b0 := p.buf[0]
	b1 := p.buf[1]

	result.FIN = b0&0x80 != 0
	result.RSV1 = b0&0x40 != 0
	result.RSV2 = b0&0x20 != 0
	result.RSV3 = b0&0x10 != 0
	result.Opcode = Opcode(b0 & 0x0F)
	result.Masked = b1&0x80 != 0
	payloadLen := int(b1 & 0x7F)

	// Parse extended payload length
	var headerOffset int = 2
	switch payloadLen {
	case 126:
		if len(p.buf) < headerOffset+2 {
			return Frame{}, io.ErrShortBuffer
		}
		payloadLen = int(binary.BigEndian.Uint16(p.buf[headerOffset : headerOffset+2]))
		headerOffset += 2
	case 127:
		if len(p.buf) < headerOffset+8 {
			return Frame{}, io.ErrShortBuffer
		}
		payloadLen = int(binary.BigEndian.Uint64(p.buf[headerOffset : headerOffset+8]))
		headerOffset += 8
	}

	// Check payload size limit before proceeding
	if p.maxPayload > 0 && payloadLen > p.maxPayload {
		return Frame{}, &MaxFrameSizeError{Limit: p.maxPayload, Payload: payloadLen}
	}

	// Parse mask key if present
	if result.Masked {
		if len(p.buf) < headerOffset+4 {
			return Frame{}, io.ErrShortBuffer
		}
		copy(result.MaskKey[:], p.buf[headerOffset:headerOffset+4])
		headerOffset += 4
	}

	// Check if we have the complete payload
	if len(p.buf) < headerOffset+payloadLen {
		return Frame{}, io.ErrShortBuffer
	}

	// Extract payload
	result.Payload = make([]byte, payloadLen)
	copy(result.Payload, p.buf[headerOffset:headerOffset+payloadLen])

	// Remove parsed bytes from buffer
	p.buf = p.buf[headerOffset+payloadLen:]

	// Unmask payload if necessary
	if result.Masked {
		applyMaskInPlace(result.Payload, result.MaskKey)
		result.Masked = false // Once unmasked, frame is no longer masked
	}

	// Handle fragmentation
	if !result.FIN {
		// Start or continue fragmentation
		if p.fragState == nil {
			// First frame in fragmented message
			p.fragState = &fragmentState{
				opcode:  result.Opcode,
				payload: result.Payload,
			}
		} else {
			// Subsequent continuation frame
			// Check that opcode is continuation (0x0)
			if result.Opcode != 0x0 {
				return Frame{}, &ProtocolError{Code: 1002, Message: "invalid opcode for continuation frame"}
			}
			p.fragState.payload = append(p.fragState.payload, result.Payload...)

			// Check total accumulated payload size
			if p.maxPayload > 0 && len(p.fragState.payload) > p.maxPayload {
				return Frame{}, &MaxFrameSizeError{Limit: p.maxPayload, Payload: len(p.fragState.payload)}
			}
		}
		// Continue parsing, don't return anything yet
		return p.tryParseFrame()
	}

	// FIN is set - check if we were in a fragmented state
	if p.fragState != nil {
		// Complete a fragmented message
		// Check that opcode is continuation (0x0) for final frame
		if result.Opcode != 0x0 {
			return Frame{}, &ProtocolError{Code: 1002, Message: "invalid opcode for continuation frame"}
		}

		// Combine accumulated payload
		result.Payload = append(p.fragState.payload, result.Payload...)
		result.Opcode = p.fragState.opcode

		// Check total message size
		if p.maxPayload > 0 && len(result.Payload) > p.maxPayload {
			return Frame{}, &MaxFrameSizeError{Limit: p.maxPayload, Payload: len(result.Payload)}
		}

		// Clear fragmentation state
		p.fragState = nil
	} else {
		// Single frame message - valid opcodes are non-continuation (0x1-0xA)
		if result.Opcode == 0x0 {
			return Frame{}, &ProtocolError{Code: 1002, Message: "invalid opcode: 0x0 for non-fragmented frame"}
		}
	}

	return result, nil
}

// ProtocolError represents a WebSocket protocol violation.
type ProtocolError struct {
	Code    int    // RFC 6455 close code
	Message string // Human-readable error message
}

func (e *ProtocolError) Error() string {
	return e.Message
}

package frame

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteFrame_TextUnmasked(t *testing.T) {
	f := NewTextFrame([]byte("Hello"))
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("WriteFrame = %x, want %x", buf.Bytes(), expected)
	}
}

func TestWriteFrame_BinaryMasked(t *testing.T) {
	f := NewBinaryFrame([]byte{0x01, 0x02})
	f.Masked = true
	f.MaskKey = [4]byte{0x37, 0xfa, 0x21, 0x3d}

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[0] != 0x82 {
		t.Errorf("first byte = %x, want 0x82", buf.Bytes()[0])
	}
	if buf.Bytes()[1] != 0x82 {
		t.Errorf("second byte = %x, want 0x82", buf.Bytes()[1])
	}
}

func TestWriteFrame_Ping(t *testing.T) {
	f := NewPingFrame(nil)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte{0x89, 0x00}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("WriteFrame ping = %x, want %x", buf.Bytes(), expected)
	}
}

func TestWriteFrame_Pong(t *testing.T) {
	f := NewPongFrame(nil)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	expected := []byte{0x8A, 0x00}
	if !bytes.Equal(buf.Bytes(), expected) {
		t.Errorf("WriteFrame pong = %x, want %x", buf.Bytes(), expected)
	}
}

func TestWriteFrame_Close(t *testing.T) {
	f := NewCloseFrame(1000, "bye")
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[0] != 0x88 {
		t.Errorf("first byte = %x, want 0x88", buf.Bytes()[0])
	}
	if buf.Bytes()[1] != 0x05 {
		t.Errorf("second byte = %x, want 0x05", buf.Bytes()[1])
	}
}

func TestWriteFrame_MediumPayload(t *testing.T) {
	payload := make([]byte, 126)
	for i := range payload {
		payload[i] = byte(i)
	}
	f := NewTextFrame(payload)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[1] != 0x7E {
		t.Errorf("length indicator = %x, want 0x7E", buf.Bytes()[1])
	}
	length := uint16(buf.Bytes()[2])<<8 | uint16(buf.Bytes()[3])
	if length != 126 {
		t.Errorf("decoded length = %d, want 126", length)
	}
}

func TestWriteFrame_LargePayload(t *testing.T) {
	payload := make([]byte, 65536)
	f := NewBinaryFrame(payload)
	f.Masked = false

	var buf bytes.Buffer
	err := WriteFrame(&buf, f)
	if err != nil {
		t.Fatal(err)
	}

	if buf.Bytes()[1] != 0x7F {
		t.Errorf("length indicator = %x, want 0x7F", buf.Bytes()[1])
	}
}

func TestReadFrame_TextUnmasked(t *testing.T) {
	raw := []byte{0x81, 0x05, 'H', 'e', 'l', 'l', 'o'}
	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !f.FIN {
		t.Error("FIN = false, want true")
	}
	if f.Opcode != OpcodeText {
		t.Errorf("Opcode = %d, want %d", f.Opcode, OpcodeText)
	}
	if string(f.Payload) != "Hello" {
		t.Errorf("Payload = %q, want %q", string(f.Payload), "Hello")
	}
}

func TestReadFrame_Masked(t *testing.T) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	original := []byte("Hello")
	masked := applyMask(original, key)

	raw := []byte{0x81, 0x85}
	raw = append(raw, key[:]...)
	raw = append(raw, masked...)

	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Payload) != "Hello" {
		t.Errorf("Payload = %q, want %q", string(f.Payload), "Hello")
	}
}

func TestReadFrame_Ping(t *testing.T) {
	raw := []byte{0x89, 0x00}
	f, err := ReadFrame(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != OpcodePing {
		t.Errorf("Opcode = %d, want Ping", f.Opcode)
	}
}

func TestReadFrame_16bitLength(t *testing.T) {
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	var buf bytes.Buffer
	WriteFrame(&buf, Frame{FIN: true, Opcode: OpcodeBinary, Payload: payload})

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Payload) != 200 {
		t.Errorf("Payload len = %d, want 200", len(f.Payload))
	}
}

func TestReadFrame_CloseWithStatus(t *testing.T) {
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, 1000)
	payload = append(payload, "normal"...)

	var buf bytes.Buffer
	WriteFrame(&buf, Frame{FIN: true, Opcode: OpcodeClose, Payload: payload})

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if f.Opcode != OpcodeClose {
		t.Errorf("Opcode = %d, want Close", f.Opcode)
	}
	code := binary.BigEndian.Uint16(f.Payload[:2])
	if code != 1000 {
		t.Errorf("Close code = %d, want 1000", code)
	}
}

func TestReadFrame_Fragmentation(t *testing.T) {
	frag1 := Frame{FIN: false, Opcode: OpcodeText, Payload: []byte("Hel")}
	frag2 := Frame{FIN: false, Opcode: 0, Payload: []byte("lo ")}
	frag3 := Frame{FIN: true, Opcode: 0, Payload: []byte("World")}

	var buf bytes.Buffer
	WriteFrame(&buf, frag1)
	WriteFrame(&buf, frag2)
	WriteFrame(&buf, frag3)

	f, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !f.FIN {
		t.Error("FIN = false, want true (reassembled)")
	}
	if f.Opcode != OpcodeText {
		t.Errorf("Opcode = %d, want Text", f.Opcode)
	}
	if string(f.Payload) != "Hello World" {
		t.Errorf("Payload = %q, want %q", string(f.Payload), "Hello World")
	}
}

package frame

import (
	"bytes"
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

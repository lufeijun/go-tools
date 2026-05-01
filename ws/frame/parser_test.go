package frame

import (
	"bytes"
	"testing"
)

func TestEmptyFeed(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)
	frames := parser.Feed(nil)
	if len(frames) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}

	frames = parser.Feed([]byte{})
	if len(frames) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestCompleteFrameInOneFeed(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create a text frame with payload "Hello, World!"
	f := NewTextFrame([]byte("Hello, World!"))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != f.Opcode {
		t.Errorf("Expected opcode %d, got %d", f.Opcode, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, f.Payload) {
		t.Errorf("Expected payload %q, got %q", string(f.Payload), string(frames[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestFrameSplitAcrossFeeds(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create a text frame with payload "Hello, World!"
	f := NewTextFrame([]byte("Hello, World!"))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()

	// Split into two parts
	part1 := data[:len(data)/2]
	part2 := data[len(data)/2:]

	// First feed - incomplete frame
	frames1 := parser.Feed(part1)
	if len(frames1) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames1))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}

	// Second feed - completes the frame
	frames2 := parser.Feed(part2)
	if len(frames2) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames2))
	}
	if frames2[0].Opcode != f.Opcode {
		t.Errorf("Expected opcode %d, got %d", f.Opcode, frames2[0].Opcode)
	}
	if !frames2[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames2[0].Payload, f.Payload) {
		t.Errorf("Expected payload %q, got %q", string(f.Payload), string(frames2[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestMultipleFramesInOneFeed(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create two text frames
	f1 := NewTextFrame([]byte("Frame 1"))
	f2 := NewTextFrame([]byte("Frame 2"))

	var buf bytes.Buffer
	if err := WriteFrame(&buf, f1); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, f2); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 2 {
		t.Fatalf("Expected 2 frames, got %d", len(frames))
	}

	if frames[0].Opcode != f1.Opcode {
		t.Errorf("Expected opcode %d, got %d", f1.Opcode, frames[0].Opcode)
	}
	if !bytes.Equal(frames[0].Payload, f1.Payload) {
		t.Errorf("Expected payload %q, got %q", string(f1.Payload), string(frames[0].Payload))
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}

	if frames[1].Opcode != f2.Opcode {
		t.Errorf("Expected opcode %d, got %d", f2.Opcode, frames[1].Opcode)
	}
	if !bytes.Equal(frames[1].Payload, f2.Payload) {
		t.Errorf("Expected payload %q, got %q", string(f2.Payload), string(frames[1].Payload))
	}
	if !frames[1].FIN {
		t.Errorf("Expected FIN true, got false")
	}

	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestMaxFrameSizeExceeded(t *testing.T) {
	maxSize := 10
	parser := NewIncrementalParser(maxSize)

	// Create a frame with payload larger than maxSize
	f := NewTextFrame(make([]byte, maxSize+1))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames))
	}
	if parser.Err() == nil {
		t.Fatal("Expected error, got nil")
	}

	mfsErr, ok := parser.Err().(*MaxFrameSizeError)
	if !ok {
		t.Fatalf("Expected MaxFrameSizeError, got %T", parser.Err())
	}
	if mfsErr.Limit != maxSize {
		t.Errorf("Expected limit %d, got %d", maxSize, mfsErr.Limit)
	}
	if mfsErr.Payload != maxSize+1 {
		t.Errorf("Expected payload %d, got %d", maxSize+1, mfsErr.Payload)
	}
}

func TestMaskedFrame(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create a masked frame
	f := NewTextFrame([]byte("Masked frame"))
	f.Masked = true
	f.MaskKey = [4]byte{0x12, 0x34, 0x56, 0x78}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != OpcodeText {
		t.Errorf("Expected opcode %d, got %d", OpcodeText, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, []byte("Masked frame")) {
		t.Errorf("Expected payload %q, got %q", "Masked frame", string(frames[0].Payload))
	}
	if frames[0].Masked {
		t.Errorf("Expected masked false, got true")
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestResetAfterError(t *testing.T) {
	maxSize := 20
	parser := NewIncrementalParser(maxSize)

	// First, exceed the max frame size
	f := NewTextFrame(make([]byte, maxSize+1))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames))
	}
	if parser.Err() == nil {
		t.Fatal("Expected error, got nil")
	}

	// Reset and try again with valid frame
	parser.Reset()
	if parser.Err() != nil {
		t.Errorf("Expected no error after reset, got %v", parser.Err())
	}

	f2 := NewTextFrame([]byte("Valid frame")) // 11 bytes, well under 20
	var buf2 bytes.Buffer
	if err := WriteFrame(&buf2, f2); err != nil {
		t.Fatal(err)
	}

	frames2 := parser.Feed(buf2.Bytes())
	if len(frames2) != 1 {
		t.Fatalf("Expected 1 frame, got %d, err: %v", len(frames2), parser.Err())
	}
	if !bytes.Equal(frames2[0].Payload, f2.Payload) {
		t.Errorf("Expected payload %q, got %q", string(f2.Payload), string(frames2[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestPingFrame(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create a ping frame with payload "Ping!"
	f := NewPingFrame([]byte("Ping!"))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != OpcodePing {
		t.Errorf("Expected opcode %d, got %d", OpcodePing, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, []byte("Ping!")) {
		t.Errorf("Expected payload %q, got %q", "Ping!", string(frames[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestFragmentationInOneFeed(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create fragmented message: non-FIN + continuation + FIN
	// Frame 1: non-FIN text frame with "Hel"
	f1 := NewTextFrame([]byte("Hel"))
	f1.FIN = false

	// Frame 2: continuation frame with "lo, "
	f2 := Frame{
		FIN:     false,
		Opcode:  0x0, // Continuation
		Payload: []byte("lo, "),
	}

	// Frame 3: continuation frame with "World!" (FIN)
	f3 := Frame{
		FIN:     true,
		Opcode:  0x0, // Continuation
		Payload: []byte("World!"),
	}

	var buf bytes.Buffer
	if err := WriteFrame(&buf, f1); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, f2); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf, f3); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != OpcodeText {
		t.Errorf("Expected opcode %d, got %d", OpcodeText, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, []byte("Hello, World!")) {
		t.Errorf("Expected payload %q, got %q", "Hello, World!", string(frames[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestFragmentationAcrossFeeds(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	// Create fragmented message parts
	f1 := NewTextFrame([]byte("Hel"))
	f1.FIN = false

	f2 := Frame{
		FIN:     false,
		Opcode:  0x0,
		Payload: []byte("lo, "),
	}

	f3 := Frame{
		FIN:     true,
		Opcode:  0x0,
		Payload: []byte("World!"),
	}

	var buf1, buf2, buf3 bytes.Buffer
	if err := WriteFrame(&buf1, f1); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf2, f2); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buf3, f3); err != nil {
		t.Fatal(err)
	}

	// Feed first frame (incomplete message)
	frames1 := parser.Feed(buf1.Bytes())
	if len(frames1) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames1))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}

	// Feed second frame (still incomplete)
	frames2 := parser.Feed(buf2.Bytes())
	if len(frames2) != 0 {
		t.Errorf("Expected empty frames, got %d", len(frames2))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}

	// Feed third frame (completes the message)
	frames3 := parser.Feed(buf3.Bytes())
	if len(frames3) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames3))
	}
	if frames3[0].Opcode != OpcodeText {
		t.Errorf("Expected opcode %d, got %d", OpcodeText, frames3[0].Opcode)
	}
	if !frames3[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames3[0].Payload, []byte("Hello, World!")) {
		t.Errorf("Expected payload %q, got %q", "Hello, World!", string(frames3[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestPongFrame(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	f := NewPongFrame([]byte("Pong!"))
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != OpcodePong {
		t.Errorf("Expected opcode %d, got %d", OpcodePong, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, []byte("Pong!")) {
		t.Errorf("Expected payload %q, got %q", "Pong!", string(frames[0].Payload))
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestCloseFrame(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	f := NewCloseFrame(1000, "Normal closure")
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != OpcodeClose {
		t.Errorf("Expected opcode %d, got %d", OpcodeClose, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, f.Payload) {
		t.Errorf("Expected payload %x, got %x", f.Payload, frames[0].Payload)
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestBinaryFrame(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	payload := []byte{0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE, 0xFD}
	f := NewBinaryFrame(payload)
	var buf bytes.Buffer
	if err := WriteFrame(&buf, f); err != nil {
		t.Fatal(err)
	}

	frames := parser.Feed(buf.Bytes())
	if len(frames) != 1 {
		t.Fatalf("Expected 1 frame, got %d", len(frames))
	}
	if frames[0].Opcode != OpcodeBinary {
		t.Errorf("Expected opcode %d, got %d", OpcodeBinary, frames[0].Opcode)
	}
	if !frames[0].FIN {
		t.Errorf("Expected FIN true, got false")
	}
	if !bytes.Equal(frames[0].Payload, payload) {
		t.Errorf("Expected payload %x, got %x", payload, frames[0].Payload)
	}
	if parser.Err() != nil {
		t.Errorf("Expected no error, got %v", parser.Err())
	}
}

func TestBufferReuse(t *testing.T) {
	parser := NewIncrementalParser(DefaultMaxFrameSize)

	for i := 0; i < 10; i++ {
		payload := []byte("Test payload " + string(rune('0'+i)))
		f := NewTextFrame(payload)
		var buf bytes.Buffer
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatal(err)
		}

		frames := parser.Feed(buf.Bytes())
		if len(frames) != 1 {
			t.Fatalf("Expected 1 frame, got %d", len(frames))
		}
		if !bytes.Equal(frames[0].Payload, payload) {
			t.Errorf("Expected payload %q, got %q", string(payload), string(frames[0].Payload))
		}
		if parser.Err() != nil {
			t.Errorf("Expected no error, got %v", parser.Err())
		}
	}
}

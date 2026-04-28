package frame

import (
	"bytes"
	"testing"
)

func TestApplyMask(t *testing.T) {
	key := [4]byte{0x37, 0xfa, 0x21, 0x3d}
	payload := []byte("Hello")
	masked := applyMask(payload, key)
	expected := []byte{0x7f, 0x9f, 0x4d, 0x51, 0x58}
	if !bytes.Equal(masked, expected) {
		t.Errorf("applyMask = %x, want %x", masked, expected)
	}
}

func TestApplyMask_Roundtrip(t *testing.T) {
	key := [4]byte{0x12, 0x34, 0x56, 0x78}
	original := []byte("test payload data")
	masked := applyMask(original, key)
	unmasked := applyMask(masked, key)
	if !bytes.Equal(unmasked, original) {
		t.Errorf("roundtrip failed: got %x, want %x", unmasked, original)
	}
}

func TestApplyMask_Empty(t *testing.T) {
	key := [4]byte{1, 2, 3, 4}
	result := applyMask([]byte{}, key)
	if len(result) != 0 {
		t.Errorf("empty payload should return empty")
	}
}

func TestGenerateMaskKey(t *testing.T) {
	key1 := GenerateMaskKey()
	key2 := GenerateMaskKey()
	if key1 == key2 {
		t.Error("two generated mask keys should differ")
	}
}

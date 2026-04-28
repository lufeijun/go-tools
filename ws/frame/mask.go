package frame

import (
	"crypto/rand"
)

// applyMask XORs the payload with the mask key per RFC 6455.
// It returns a new slice; the input is not modified.
func applyMask(payload []byte, maskKey [4]byte) []byte {
	if len(payload) == 0 {
		return payload
	}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	return masked
}

// GenerateMaskKey generates a random 4-byte mask key using crypto/rand.
func GenerateMaskKey() [4]byte {
	var key [4]byte
	_, _ = rand.Read(key[:])
	return key
}

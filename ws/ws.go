package ws

import (
	"fmt"
	"net/http"
	"runtime"
	"time"
)

// WSError is the unified error type for v2.
type WSError struct {
	Code    int
	Message string
	Cause   error
	ConnID  uint64
}

func (e *WSError) Error() string {
	if e.ConnID != 0 {
		return fmt.Sprintf("ws error code=%d: %s (conn=%d)", e.Code, e.Message, e.ConnID)
	}
	return fmt.Sprintf("ws error code=%d: %s", e.Code, e.Message)
}

func (e *WSError) Unwrap() error {
	return e.Cause
}

func (e *WSError) WithConnID(id uint64) *WSError {
	return &WSError{
		Code:    e.Code,
		Message: e.Message,
		Cause:   e.Cause,
		ConnID:  id,
	}
}

// Predefined error codes.
const (
	ErrCodeProtocolError   = 1002
	ErrCodeUnsupportedData = 1003
	ErrCodeInvalidFrame    = 1007
	ErrCodePolicyViolation = 1008
	ErrCodeMessageTooBig   = 1009
	ErrCodeInternalError   = 1011
	ErrCodeReadTimeout     = 2001
	ErrCodeWriteTimeout    = 2002
	ErrCodeConnReset       = 2003
	ErrCodeHubFull         = 3001
)

// Config is the global configuration for Server and Client.
// Zero values mean "use default".
type Config struct {
	Addr              string
	ReadBufferSize    int
	WriteBufferSize   int
	MaxConnections    int
	TCPNoDelay        bool
	TCPQuickAck       bool
	SOReusePort       bool
	EventLoopWorkers  int
	EventLoopStrategy string
	BufferPoolSmall   int
	BufferPoolDefault int
	BufferPoolLarge   int
	PingInterval      time.Duration
	PongTimeout       time.Duration
	MaxFrameSize      int
	EnableCompression bool
	Headers           http.Header
	ReconnectInterval time.Duration
	MaxReconnect      int
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		ReadBufferSize:    4096,
		WriteBufferSize:   4096,
		TCPNoDelay:        true,
		TCPQuickAck:       false,
		SOReusePort:       false,
		EventLoopWorkers:  0, // 0 means runtime.NumCPU()
		EventLoopStrategy: "roundrobin",
		BufferPoolSmall:   4096,
		BufferPoolDefault: 1024,
		BufferPoolLarge:   256,
		PingInterval:      30 * time.Second,
		PongTimeout:       60 * time.Second,
		MaxFrameSize:      64 * 1024 * 1024,
		EnableCompression: false,
		ReconnectInterval: 5 * time.Second,
		MaxReconnect:      5,
	}
}

// EventLoopWorkerCount returns the effective number of event loop workers.
func (c Config) EventLoopWorkerCount() int {
	if c.EventLoopWorkers > 0 {
		return c.EventLoopWorkers
	}
	return runtime.NumCPU()
}

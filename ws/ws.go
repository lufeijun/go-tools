package ws

import "fmt"

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

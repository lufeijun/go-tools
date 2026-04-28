package ws

import "fmt"

type CloseError struct {
	Code   uint16
	Reason string
	Cause  error
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket close code %d: %s", e.Code, e.Reason)
}

func (e *CloseError) WithCause(cause error) *CloseError {
	return &CloseError{
		Code:   e.Code,
		Reason: e.Reason,
		Cause:  cause,
	}
}

func (e *CloseError) Unwrap() error {
	return e.Cause
}

var (
	ErrProtocolError   = &CloseError{Code: 1002, Reason: "protocol error"}
	ErrUnsupportedData = &CloseError{Code: 1003, Reason: "unsupported data"}
	ErrInvalidFrame    = &CloseError{Code: 1007, Reason: "invalid frame payload data"}
	ErrPolicyViolation = &CloseError{Code: 1008, Reason: "policy violation"}
	ErrMessageTooBig   = &CloseError{Code: 1009, Reason: "message too big"}
	ErrInternalError   = &CloseError{Code: 1011, Reason: "internal error"}
)

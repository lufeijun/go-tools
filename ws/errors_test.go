package ws

import (
	"errors"
	"testing"
)

func TestCloseError_Error(t *testing.T) {
	e := &CloseError{Code: 1002, Reason: "protocol error"}
	if got := e.Error(); got != "websocket close code 1002: protocol error" {
		t.Errorf("Error() = %q, want %q", got, "websocket close code 1002: protocol error")
	}
}

func TestCloseError_WithCause(t *testing.T) {
	cause := errors.New("underlying")
	e := ErrProtocolError.WithCause(cause)
	if e.Code != 1002 {
		t.Errorf("Code = %d, want 1002", e.Code)
	}
	if e.Cause != cause {
		t.Errorf("Cause = %v, want %v", e.Cause, cause)
	}
}

func TestPredefinedErrors(t *testing.T) {
	cases := []struct {
		err  *CloseError
		code uint16
	}{
		{ErrProtocolError, 1002},
		{ErrUnsupportedData, 1003},
		{ErrInvalidFrame, 1007},
		{ErrPolicyViolation, 1008},
		{ErrMessageTooBig, 1009},
		{ErrInternalError, 1011},
	}
	for _, tc := range cases {
		if tc.err.Code != tc.code {
			t.Errorf("Code = %d, want %d", tc.err.Code, tc.code)
		}
	}
}

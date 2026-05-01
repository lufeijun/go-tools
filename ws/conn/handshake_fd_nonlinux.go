//go:build !linux

package conn

import (
	"errors"
	"net/http"
)

func ServerHandshakeFD(fd int) (int, error) {
	return fd, errors.New("ServerHandshakeFD is only supported on Linux")
}

func ClientHandshakeFD(fd int, rawURL string, headers http.Header) error {
	return errors.New("ClientHandshakeFD is only supported on Linux")
}

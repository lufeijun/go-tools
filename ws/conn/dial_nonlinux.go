//go:build !linux

package conn

import "errors"

func DialNonBlock(addr string) (int, error) {
    return -1, errors.New("DialNonBlock is only supported on Linux")
}

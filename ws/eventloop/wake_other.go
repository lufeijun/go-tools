//go:build !linux && !darwin && !freebsd && !openbsd

package eventloop

func createWakeFd() (int, int, error) { return -1, -1, nil }
func doWake(fd int)                   {}
func drainWake(fd int)                {}
func closeWakeFd(readFd, writeFd int) {}

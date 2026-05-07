//go:build linux
// +build linux

package eventloop

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

type epollPoller struct {
	epfd int
}

func newEpollPoller() Poller {
	return &epollPoller{}
}

// NewEpollPoller creates a new epoll-based Poller.
func NewEpollPoller() Poller {
	return newEpollPoller()
}

func (p *epollPoller) Open() error {
	fd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	p.epfd = fd
	return nil
}

func (p *epollPoller) Close() error {
	if p.epfd != 0 {
		return unix.Close(p.epfd)
	}
	return nil
}

func (p *epollPoller) Add(fd int, events uint32) error {
	var ev unix.EpollEvent
	ev.Fd = int32(fd)
	ev.Events = epollEvents(events)
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_ADD, fd, &ev)
}

func (p *epollPoller) Mod(fd int, events uint32) error {
	var ev unix.EpollEvent
	ev.Fd = int32(fd)
	ev.Events = epollEvents(events)
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_MOD, fd, &ev)
}

func (p *epollPoller) Del(fd int) error {
	return unix.EpollCtl(p.epfd, unix.EPOLL_CTL_DEL, fd, nil)
}

func (p *epollPoller) Wait(timeoutMs int) ([]Event, error) {
	const maxEvents = 1024
	var epEvents [maxEvents]unix.EpollEvent

	n, err := unix.EpollWait(p.epfd, epEvents[:], timeoutMs)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return nil, nil
		}
		return nil, err
	}

	events := make([]Event, 0, n)
	for i := 0; i < n; i++ {
		ev := epEvents[i]
		events = append(events, Event{
			FD:     int(ev.Fd),
			Events: fromEpollEvents(ev.Events),
		})
	}
	return events, nil
}

func epollEvents(events uint32) uint32 {
	var e uint32
	if events&EventRead != 0 {
		e |= unix.EPOLLIN
	}
	if events&EventWrite != 0 {
		e |= unix.EPOLLOUT
	}
	if events&EventError != 0 {
		e |= unix.EPOLLERR
	}
	if events&EventHup != 0 {
		e |= unix.EPOLLHUP | unix.EPOLLRDHUP
	}
	return e | unix.EPOLLET // edge-triggered
}

func fromEpollEvents(e uint32) uint32 {
	var events uint32
	if e&unix.EPOLLIN != 0 {
		events |= EventRead
	}
	if e&unix.EPOLLOUT != 0 {
		events |= EventWrite
	}
	if e&unix.EPOLLERR != 0 {
		events |= EventError
	}
	if e&(unix.EPOLLHUP|unix.EPOLLRDHUP) != 0 {
		events |= EventHup
	}
	return events
}

// --- wake mechanism (Linux: eventfd) ---

func createWakeFd() (int, int, error) {
	fd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		return -1, -1, err
	}
	return fd, fd, nil // readFd == writeFd for eventfd
}

func doWake(fd int) {
	if fd < 0 {
		return
	}
	var val uint64 = 1
	unix.Write(fd, uint64ToBytes(val))
}

func drainWake(fd int) {
	if fd < 0 {
		return
	}
	var buf [8]byte
	unix.Read(fd, buf[:])
}

func closeWakeFd(readFd, writeFd int) {
	if readFd >= 0 {
		unix.Close(readFd)
	}
}

func uint64ToBytes(v uint64) []byte {
	var b [8]byte
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
	b[4] = byte(v >> 32)
	b[5] = byte(v >> 40)
	b[6] = byte(v >> 48)
	b[7] = byte(v >> 56)
	return b[:]
}

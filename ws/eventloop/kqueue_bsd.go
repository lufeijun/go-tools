//go:build darwin || freebsd || openbsd
// +build darwin freebsd openbsd

package eventloop

import (
	"errors"
	"syscall"

	"golang.org/x/sys/unix"
)

type kqueuePoller struct {
	kqfd int
}

func newKqueuePoller() Poller {
	return &kqueuePoller{}
}

// NewKqueuePoller creates a new kqueue-based Poller.
func NewKqueuePoller() Poller {
	return newKqueuePoller()
}

func (p *kqueuePoller) Open() error {
	fd, err := unix.Kqueue()
	if err != nil {
		return err
	}
	p.kqfd = fd
	return nil
}

func (p *kqueuePoller) Close() error {
	if p.kqfd != 0 {
		return unix.Close(p.kqfd)
	}
	return nil
}

func (p *kqueuePoller) Add(fd int, events uint32) error {
	kevents := kqueueEvents(fd, events, unix.EV_ADD)
	_, err := unix.Kevent(p.kqfd, kevents, nil, nil)
	return err
}

func (p *kqueuePoller) Mod(fd int, events uint32) error {
	return p.Add(fd, events)
}

func (p *kqueuePoller) Del(fd int) error {
	kevents := []unix.Kevent_t{
		{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_DELETE},
		{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_DELETE},
	}
	_, err := unix.Kevent(p.kqfd, kevents, nil, nil)
	if err != nil && !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return nil
}

func (p *kqueuePoller) Wait(timeoutMs int) ([]Event, error) {
	const maxEvents = 1024
	var kevents [maxEvents]unix.Kevent_t

	var ts *unix.Timespec
	if timeoutMs >= 0 {
		t := unix.NsecToTimespec(int64(timeoutMs) * 1e6)
		ts = &t
	}

	n, err := unix.Kevent(p.kqfd, nil, kevents[:], ts)
	if err != nil {
		if errors.Is(err, syscall.EINTR) {
			return nil, nil
		}
		return nil, err
	}

	fdEvents := make(map[int]uint32)
	for i := 0; i < n; i++ {
		ev := kevents[i]
		fd := int(ev.Ident)
		switch ev.Filter {
		case unix.EVFILT_READ:
			fdEvents[fd] |= EventRead
		case unix.EVFILT_WRITE:
			fdEvents[fd] |= EventWrite
		}
		if ev.Flags&unix.EV_ERROR != 0 {
			fdEvents[fd] |= EventError
		}
		if ev.Flags&unix.EV_EOF != 0 {
			fdEvents[fd] |= EventHup
		}
	}

	events := make([]Event, 0, len(fdEvents))
	for fd, e := range fdEvents {
		events = append(events, Event{FD: fd, Events: e})
	}
	return events, nil
}

func kqueueEvents(fd int, events uint32, flags uint16) []unix.Kevent_t {
	var kev []unix.Kevent_t
	if events&EventRead != 0 {
		kev = append(kev, unix.Kevent_t{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_READ,
			Flags:  flags,
		})
	}
	if events&EventWrite != 0 {
		kev = append(kev, unix.Kevent_t{
			Ident:  uint64(fd),
			Filter: unix.EVFILT_WRITE,
			Flags:  flags,
		})
	}
	return kev
}

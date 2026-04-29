//go:build linux
// +build linux

package eventloop

import (
	"os"
	"testing"
)

func TestEpollPoller_Lifecycle(t *testing.T) {
	p := newEpollPoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEpollPoller_AddDel(t *testing.T) {
	p := newEpollPoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Use a pipe to get a real fd
	r, _, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	fd := int(r.Fd())
	if err := p.Add(fd, EventRead); err != nil {
		t.Fatal(err)
	}
	if err := p.Del(fd); err != nil {
		t.Fatal(err)
	}
}

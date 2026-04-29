//go:build darwin || freebsd || openbsd
// +build darwin freebsd openbsd

package eventloop

import (
	"os"
	"testing"
)

func TestKqueuePoller_Lifecycle(t *testing.T) {
	p := newKqueuePoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestKqueuePoller_AddDel(t *testing.T) {
	p := newKqueuePoller()
	if err := p.Open(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

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

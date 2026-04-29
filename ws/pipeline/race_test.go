package pipeline

import (
	"testing"
)

func TestRace(t *testing.T) {
	p := NewPipeline()
	h := &recorderInbound{testHandler: testHandler{name: "h"}}
	p.AddLast("h", h)

	go func() {
		for i := 0; i < 1000; i++ {
			p.FireChannelRead(i)
		}
	}()

	go func() {
		for i := 0; i < 1000; i++ {
			p.Remove("h")
			p.AddLast("h", h)
		}
	}()
}

package ws

import (
	"github.com/lufeijun/goTools/ws/internal/conn"
)

type Hub struct {
	conns      map[uint64]*Session
	register   chan *Session
	unregister chan uint64
	broadcast  chan conn.Message
	getReq     chan uint64
	getResp    chan *Session
	countReq   chan struct{}
	countResp  chan int
	stopChan   chan struct{}
}

func NewHub() *Hub {
	return &Hub{
		conns:      make(map[uint64]*Session),
		register:   make(chan *Session, 64),
		unregister: make(chan uint64, 64),
		broadcast:  make(chan conn.Message, 64),
		getReq:     make(chan uint64),
		getResp:    make(chan *Session, 1),
		countReq:   make(chan struct{}),
		countResp:  make(chan int, 1),
		stopChan:   make(chan struct{}),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case s := <-h.register:
			h.conns[s.Conn().ID()] = s

		case id := <-h.unregister:
			delete(h.conns, id)

		case msg := <-h.broadcast:
			for _, s := range h.conns {
				select {
				case s.WriteChan() <- msg:
				default:
				}
			}

		case id := <-h.getReq:
			s, ok := h.conns[id]
			if ok {
				h.getResp <- s
			} else {
				h.getResp <- nil
			}

		case <-h.countReq:
			h.countResp <- len(h.conns)

		case <-h.stopChan:
			return
		}
	}
}

func (h *Hub) Register(s *Session)  { h.register <- s }
func (h *Hub) Unregister(id uint64)         { h.unregister <- id }
func (h *Hub) Broadcast(msg conn.Message)   { h.broadcast <- msg }

func (h *Hub) Send(id uint64, msg conn.Message) {
	h.getReq <- id
	s := <-h.getResp
	if s != nil {
		s.WriteChan() <- msg
	}
}

func (h *Hub) Get(id uint64) *Session {
	h.getReq <- id
	return <-h.getResp
}

func (h *Hub) Count() int {
	h.countReq <- struct{}{}
	return <-h.countResp
}

func (h *Hub) Stop() {
	close(h.stopChan)
}

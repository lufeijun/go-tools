package hub

import (
	"sync"

	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/session"
)

// Hub is the connection management center.
type Hub interface {
	Register(s session.Session)
	Unregister(id uint64)
	Broadcast(msg conn.Message)
	Send(id uint64, msg conn.Message)
	Count() int
	Get(id uint64) session.Session
}

// NewHub creates a sharded Hub with the given shard count.
func NewHub(shardCount int) Hub {
	if shardCount <= 0 {
		shardCount = 32
	}
	h := &shardedHub{
		shardCount: shardCount,
		shards:     make([]*shard, shardCount),
	}
	for i := range h.shards {
		h.shards[i] = &shard{conns: make(map[uint64]session.Session)}
	}
	return h
}

type shardedHub struct {
	shardCount int
	shards     []*shard
}

type shard struct {
	mu    sync.RWMutex
	conns map[uint64]session.Session
}

func (h *shardedHub) shardIndex(id uint64) int {
	return int(id % uint64(h.shardCount))
}

func (h *shardedHub) Register(s session.Session) {
	id := s.Conn().ID()
	sh := h.shards[h.shardIndex(id)]
	sh.mu.Lock()
	sh.conns[id] = s
	sh.mu.Unlock()
}

func (h *shardedHub) Unregister(id uint64) {
	sh := h.shards[h.shardIndex(id)]
	sh.mu.Lock()
	delete(sh.conns, id)
	sh.mu.Unlock()
}

func (h *shardedHub) Get(id uint64) session.Session {
	sh := h.shards[h.shardIndex(id)]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.conns[id]
}

func (h *shardedHub) Count() int {
	var total int
	for _, sh := range h.shards {
		sh.mu.RLock()
		total += len(sh.conns)
		sh.mu.RUnlock()
	}
	return total
}

func (h *shardedHub) Broadcast(msg conn.Message) {
	var wg sync.WaitGroup
	for _, sh := range h.shards {
		wg.Add(1)
		go func(s *shard) {
			defer wg.Done()
			s.mu.RLock()
			sessions := make([]session.Session, 0, len(s.conns))
			for _, sess := range s.conns {
				sessions = append(sessions, sess)
			}
			s.mu.RUnlock()

			for _, sess := range sessions {
				sess.Conn().Pipeline().FireChannelWrite(&msg)
			}
		}(sh)
	}
	wg.Wait()
}

func (h *shardedHub) Send(id uint64, msg conn.Message) {
	s := h.Get(id)
	if s == nil {
		return
	}
	s.Conn().Pipeline().FireChannelWrite(&msg)
}

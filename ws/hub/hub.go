package hub

import (
	"sync"
	"unsafe"

	"github.com/lufeijun/goTools/ws/conn"
	"github.com/lufeijun/goTools/ws/session"
)

const cacheLineSize = 64

// Hub is the connection management center.
type Hub interface {
	Register(s session.Session)
	Unregister(id uint64)
	Broadcast(msg conn.Message)
	Send(id uint64, msg conn.Message)
	Count() int
	Get(id uint64) session.Session
	CloseAll()
	Close() // shuts down background workers
}

const broadcastQueueSize = 256

// NewHub creates a sharded Hub with the given shard count.
func NewHub(shardCount int) Hub {
	if shardCount <= 0 {
		shardCount = 32
	}
	h := &shardedHub{
		shardCount: shardCount,
		shards:     make([]*shard, shardCount),
		workers:    make([]*broadcastWorker, shardCount),
	}
	for i := range h.shards {
		h.shards[i] = &shard{conns: make(map[uint64]session.Session)}
		h.workers[i] = &broadcastWorker{shard: h.shards[i], ch: make(chan conn.Message, broadcastQueueSize)}
		go h.workers[i].run()
	}
	return h
}

type shardedHub struct {
	shardCount int
	shards     []*shard
	workers    []*broadcastWorker
}

type broadcastWorker struct {
	shard *shard
	ch    chan conn.Message
}

func (w *broadcastWorker) run() {
	for msg := range w.ch {
		w.shard.mu.RLock()
		sessions := make([]session.Session, 0, len(w.shard.conns))
		for _, sess := range w.shard.conns {
			sessions = append(sessions, sess)
		}
		w.shard.mu.RUnlock()

		for _, sess := range sessions {
			sess.Conn().Pipeline().FireChannelWrite(&msg)
		}
	}
}

type shard struct {
	mu    sync.RWMutex
	conns map[uint64]session.Session
	// Pad to a full cache line to prevent false sharing between shards.
	_ [cacheLineSize - int(unsafe.Sizeof(sync.RWMutex{})) - int(unsafe.Sizeof(map[uint64]session.Session{}))]byte
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
	for _, w := range h.workers {
		select {
		case w.ch <- msg:
		default:
			// Queue full: drop message for this shard to avoid blocking.
		}
	}
}

func (h *shardedHub) Send(id uint64, msg conn.Message) {
	s := h.Get(id)
	if s == nil {
		return
	}
	s.Conn().Pipeline().FireChannelWrite(&msg)
}

func (h *shardedHub) CloseAll() {
	for _, sh := range h.shards {
		sh.mu.RLock()
		sessions := make([]session.Session, 0, len(sh.conns))
		for _, sess := range sh.conns {
			sessions = append(sessions, sess)
		}
		sh.mu.RUnlock()

		for _, sess := range sessions {
			sess.Close()
		}
	}
}

// Close shuts down all background broadcast workers and releases resources.
func (h *shardedHub) Close() {
	for _, w := range h.workers {
		close(w.ch)
	}
}

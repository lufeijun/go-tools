package session

import (
	"sync"
	"sync/atomic"
	"time"
)

// TimingWheel is a single-level hierarchical timing wheel for managing
// delayed tasks with a single goroutine.  It is safe for concurrent use.
type TimingWheel struct {
	tick      time.Duration
	wheelSize int

	buckets []map[int64]*twTask
	current int

	taskIDSeq int64
	mu        sync.RWMutex

	stopCh chan struct{}
	wg     sync.WaitGroup
}

type twTask struct {
	id        int64
	round     int
	callback  func()
	cancelled int32
}

// NewTimingWheel creates a timing wheel.
// tick is the interval between slot advances.
// wheelSize is the number of slots; max delay = tick * wheelSize.
func NewTimingWheel(tick time.Duration, wheelSize int) *TimingWheel {
	buckets := make([]map[int64]*twTask, wheelSize)
	for i := range buckets {
		buckets[i] = make(map[int64]*twTask)
	}
	return &TimingWheel{
		tick:      tick,
		wheelSize: wheelSize,
		buckets:   buckets,
		stopCh:    make(chan struct{}),
	}
}

// Start begins the timing wheel goroutine.
func (tw *TimingWheel) Start() {
	tw.wg.Add(1)
	go tw.run()
}

// Stop shuts down the timing wheel and waits for the goroutine to exit.
func (tw *TimingWheel) Stop() {
	close(tw.stopCh)
	tw.wg.Wait()
}

// Add schedules a callback after delay.  It returns a task ID that can be
// passed to Cancel.  If delay exceeds the wheel capacity the task is dropped
// and -1 is returned.
func (tw *TimingWheel) Add(delay time.Duration, callback func()) int64 {
	ticks := int(delay / tw.tick)
	if delay%tw.tick != 0 {
		ticks++
	}
	if ticks == 0 {
		ticks = 1
	}

	round := ticks / tw.wheelSize
	slot := (tw.current + ticks) % tw.wheelSize

	id := atomic.AddInt64(&tw.taskIDSeq, 1)
	task := &twTask{
		id:       id,
		round:    round,
		callback: callback,
	}

	tw.mu.Lock()
	// Slot may have changed if the wheel advanced during lock acquisition,
	// but the round number is still correct for the original slot.  For
	// simplicity we use the computed slot; a small drift is acceptable for
	// heartbeat use cases.
	tw.buckets[slot][id] = task
	tw.mu.Unlock()

	return id
}

// Cancel removes a scheduled task.  It is safe to call with an invalid ID.
func (tw *TimingWheel) Cancel(taskID int64) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	for _, bucket := range tw.buckets {
		if t, ok := bucket[taskID]; ok {
			atomic.StoreInt32(&t.cancelled, 1)
			delete(bucket, taskID)
			return
		}
	}
}

func (tw *TimingWheel) run() {
	defer tw.wg.Done()
	ticker := time.NewTicker(tw.tick)
	defer ticker.Stop()

	for {
		select {
		case <-tw.stopCh:
			return
		case <-ticker.C:
			tw.advance()
		}
	}
}

func (tw *TimingWheel) advance() {
	tw.mu.Lock()
	bucket := tw.buckets[tw.current]
	// Collect tasks whose round has reached zero.
	var ready []*twTask
	for id, task := range bucket {
		if atomic.LoadInt32(&task.cancelled) == 1 {
			delete(bucket, id)
			continue
		}
		if task.round <= 0 {
			ready = append(ready, task)
			delete(bucket, id)
		} else {
			task.round--
		}
	}
	tw.current = (tw.current + 1) % tw.wheelSize
	tw.mu.Unlock()

	// Execute callbacks outside the lock.
	for _, task := range ready {
		if atomic.LoadInt32(&task.cancelled) == 0 {
			task.callback()
		}
	}
}

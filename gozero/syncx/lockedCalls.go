package syncx

import "sync"

type (
	LockedCalls interface {
		Do(key string, fn func() (any, error)) (any, error)
	}

	lockedGroup struct {
		mu sync.Mutex
		m  map[string]*sync.WaitGroup
	}
)

func NewLockedCalls() LockedCalls {
	return &lockedGroup{
		m: make(map[string]*sync.WaitGroup),
	}
}

func (lg *lockedGroup) Do(key string, fn func() (any, error)) (any, error) {
begin:
	lg.mu.Lock()
	if wg, ok := lg.m[key]; ok {
		lg.mu.Unlock()
		wg.Wait()
		goto begin // 返回头重新开始执行
	}

	return lg.makeCall(key, fn)
}


func (lg *lockedGroup) makeCall(key string, fn func() (any, error)) (any, error) {
	var wg sync.WaitGroup
	wg.Add(1)
	lg.m[key] = &wg
	lg.mu.Unlock()

	defer func() {
		// delete key first, done later. can't reverse the order, because if reverse,
		// another Do call might wg.Wait() without get notified with wg.Done()
		lg.mu.Lock()
		delete(lg.m, key)
		lg.mu.Unlock()
		wg.Done()
	}()

	return fn()
}

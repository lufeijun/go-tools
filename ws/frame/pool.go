package frame

import "sync"

const (
	smallBufSize   = 512
	defaultBufSize = 4096
)

var smallBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, smallBufSize) },
}

var defaultBufPool = sync.Pool{
	New: func() interface{} { return make([]byte, defaultBufSize) },
}

func GetBuf(size int) []byte {
	if size <= smallBufSize {
		return smallBufPool.Get().([]byte)[:0]
	}
	if size <= defaultBufSize {
		return defaultBufPool.Get().([]byte)[:0]
	}
	return make([]byte, size)
}

func PutBuf(buf []byte) {
	c := cap(buf)
	if c <= smallBufSize {
		smallBufPool.Put(buf)
	} else if c <= defaultBufSize {
		defaultBufPool.Put(buf)
	}
	// larger than defaultBufSize: let GC handle it
}

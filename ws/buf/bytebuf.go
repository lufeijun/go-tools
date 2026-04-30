// Package buf provides reference-counted ByteBuf with separate read/write
// indexes and tiered object pooling.
//
// Thread safety: ByteBuf is NOT thread-safe.  A single ByteBuf must be
// accessed by only one goroutine at a time.  To transfer ownership across
// goroutines use Retain() on the sender side and Release() on the receiver
// side.  Peek/ReadBytes/Slice create views that share the underlying array
// and must also obey the single-writer rule.
package buf

import (
	"fmt"
	"sync/atomic"
)

// Pool is a tiered ByteBuf pool.
type Pool interface {
	Get(capacity int) ByteBuf
	Put(ByteBuf)
}

// ByteBuf is a reference-counted byte buffer with separate read/write indexes.
type ByteBuf interface {
	ReadableBytes() int
	ReadBytes(n int) []byte
	ReadAll() []byte
	Skip(n int)
	Peek(n int) []byte
	WritableBytes() int
	Write(p []byte) (int, error)
	WriteByte(b byte) error
	EnsureWritable(min int)
	Slice(start, length int) ByteBuf
	Retain() ByteBuf
	Release()
	RefCount() int
	Bytes() []byte
	ReaderIndex() int
	WriterIndex() int
	SetReaderIndex(int)
	SetWriterIndex(int)
}

// byteBuf is the default ByteBuf implementation.
type byteBuf struct {
	data         []byte
	readerIndex  int
	writerIndex  int
	refCount     int32
	pool         Pool
}

// NewByteBuf creates a new ByteBuf with the given capacity.
func NewByteBuf(capacity int) ByteBuf {
	return &byteBuf{
		data:     make([]byte, 0, capacity),
		refCount: 1,
	}
}

func (b *byteBuf) ReadableBytes() int { return b.writerIndex - b.readerIndex }
func (b *byteBuf) WritableBytes() int { return cap(b.data) - b.writerIndex }

func (b *byteBuf) ReadBytes(n int) []byte {
	if n > b.ReadableBytes() {
		n = b.ReadableBytes()
	}
	result := make([]byte, n)
	copy(result, b.data[b.readerIndex:b.readerIndex+n])
	b.readerIndex += n
	return result
}

func (b *byteBuf) ReadAll() []byte {
	return b.ReadBytes(b.ReadableBytes())
}

func (b *byteBuf) Skip(n int) {
	if n > b.ReadableBytes() {
		n = b.ReadableBytes()
	}
	b.readerIndex += n
}

func (b *byteBuf) Peek(n int) []byte {
	if n > b.ReadableBytes() {
		n = b.ReadableBytes()
	}
	return b.data[b.readerIndex : b.readerIndex+n]
}

func (b *byteBuf) Write(p []byte) (int, error) {
	b.EnsureWritable(len(p))
	b.data = b.data[:b.writerIndex+len(p)]
	copy(b.data[b.writerIndex:], p)
	b.writerIndex += len(p)
	return len(p), nil
}

func (b *byteBuf) WriteByte(v byte) error {
	b.EnsureWritable(1)
	b.data = b.data[:b.writerIndex+1]
	b.data[b.writerIndex] = v
	b.writerIndex++
	return nil
}

func (b *byteBuf) EnsureWritable(min int) {
	if b.WritableBytes() >= min {
		return
	}
	needed := b.writerIndex + min
	if needed <= cap(b.data) {
		return
	}
	// Align to power-of-two tiers for better pool reuse.
	newCap := roundUpPowerOf2(needed)
	newData := make([]byte, b.writerIndex, newCap)
	copy(newData, b.data)
	b.data = newData
}

func roundUpPowerOf2(n int) int {
	if n <= 512 {
		return 512
	}
	if n <= 4096 {
		return 4096
	}
	if n <= 65536 {
		return 65536
	}
	// Next power of two for larger values.
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return n
}

func (b *byteBuf) Slice(start, length int) ByteBuf {
	if start < 0 || start+length > b.writerIndex {
		panic(fmt.Sprintf("slice out of range: start=%d length=%d writerIndex=%d", start, length, b.writerIndex))
	}
	s := &byteBuf{
		data:         b.data,
		readerIndex:  start,
		writerIndex:  start + length,
		refCount:     1,
	}
	b.Retain()
	return s
}

func (b *byteBuf) Retain() ByteBuf {
	rc := atomic.AddInt32(&b.refCount, 1)
	if rc <= 1 {
		panic(fmt.Sprintf("ByteBuf(%p) Retain on released buffer, refCount=%d", b, rc))
	}
	return b
}

func (b *byteBuf) Release() {
	rc := atomic.AddInt32(&b.refCount, -1)
	if rc == 0 {
		b.readerIndex = 0
		b.writerIndex = 0
		if b.pool != nil {
			b.pool.Put(b)
		}
	} else if rc < 0 {
		panic(fmt.Sprintf("ByteBuf(%p) double free detected, refCount=%d", b, rc))
	}
}

func (b *byteBuf) RefCount() int       { return int(atomic.LoadInt32(&b.refCount)) }
func (b *byteBuf) Bytes() []byte       { return b.data }
func (b *byteBuf) ReaderIndex() int    { return b.readerIndex }
func (b *byteBuf) WriterIndex() int    { return b.writerIndex }
func (b *byteBuf) SetReaderIndex(v int) { b.readerIndex = v }
func (b *byteBuf) SetWriterIndex(v int) { b.writerIndex = v }

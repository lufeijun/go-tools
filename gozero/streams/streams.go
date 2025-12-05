package streams

import "sort"

const (
	defaultWorkers = 16
	minWorkers     = 1
)

type (
	// 流结构体
	Stream struct {
		source <-chan any
	}

	rxOptions struct {
		unlimitedWorkers bool
		workers          int
	}

	// FilterFunc defines the method to filter a Stream.
	FilterFunc func(item any) bool
	// ForAllFunc defines the method to handle all elements in a Stream.
	ForAllFunc func(pipe <-chan any)
	// ForEachFunc defines the method to handle each element in a Stream.
	ForEachFunc func(item any)
	// GenerateFunc defines the method to send elements into a Stream.
	GenerateFunc func(source chan<- any)
	// KeyFunc defines the method to generate keys for the elements in a Stream.
	KeyFunc func(item any) any
	// LessFunc defines the method to compare the elements in a Stream.
	LessFunc func(a, b any) bool
	// MapFunc defines the method to map each element to another object in a Stream.
	MapFunc func(item any) any
	// Option defines the method to customize a Stream.
	Option func(opts *rxOptions)
	// ParallelFunc defines the method to handle elements parallelly.
	ParallelFunc func(item any)
	// ReduceFunc defines the method to reduce all the elements in a Stream.
	ReduceFunc func(pipe <-chan any) (any, error)
	// WalkFunc defines the method to walk through all the elements in a Stream.
	WalkFunc func(item any, pipe chan<- any)
)

// ============= 创建流结构部分  ==========================

// 通过可变参数模式创建 stream
// Just converts the given arbitrary items to a Stream.
func Just(items ...any) Stream {
	source := make(chan any, len(items))
	for _, item := range items {
		source <- item
	}
	close(source)
	return Range(source)
}

// Range converts the given channel to a Stream.
func Range(source <-chan any) Stream {
	return Stream{
		source: source,
	}
}

// ============= 操作部分  ==========================

// ForAll handles the streaming elements from the source and no later streams.
func (s Stream) ForAll(fn ForAllFunc) {
	fn(s.source)
	// avoid goroutine leak on fn not consuming all items.
	go drain(s.source)
}

// drain drains the given channel.
func drain(channel <-chan any) {
	for range channel {
	}
}

// ForEach seals the Stream with the ForEachFunc on each item, no successive operations.
func (s Stream) ForEach(fn ForEachFunc) {
	for item := range s.source {
		fn(item)
	}
}

// Filter filters the items by the given FilterFunc.
func (s Stream) Filter(fn FilterFunc, opts ...Option) Stream {
	return s.Walk(func(item any, pipe chan<- any) {
		if fn(item) {
			pipe <- item
		}
	}, opts...)
}

// Walk lets the callers handle each item, the caller may write zero, one or more items based on the given item.
func (s Stream) Walk(fn WalkFunc, opts ...Option) Stream {
	option := buildOptions(opts...)

	if option.unlimitedWorkers {
		return s.walkUnlimited(fn, option)
	}

	return s.walkLimited(fn, option)
}

// buildOptions returns a rxOptions with given customizations.
func buildOptions(opts ...Option) *rxOptions {
	options := newOptions()
	for _, opt := range opts {
		opt(options)
	}

	return options
}

// newOptions returns a default rxOptions.
func newOptions() *rxOptions {
	return &rxOptions{
		workers: defaultWorkers,
	}
}

// func (s Stream) walkLimited(fn WalkFunc, option *rxOptions) Stream {
// 	pipe := make(chan any, option.workers)

// 	go func() {
// 		var wg sync.WaitGroup
// 		pool := make(chan lang.PlaceholderType, option.workers)

// 		for item := range s.source {
// 			// important, used in another goroutine
// 			val := item
// 			pool <- lang.Placeholder
// 			wg.Add(1)

// 			// better to safely run caller defined method
// 			threading.GoSafe(func() {
// 				defer func() {
// 					wg.Done()
// 					<-pool
// 				}()

// 				fn(val, pipe)
// 			})
// 		}

// 		wg.Wait()
// 		close(pipe)
// 	}()

// 	return Range(pipe)
// }

// func (s Stream) walkUnlimited_old(fn WalkFunc, option *rxOptions) Stream {
// 	pipe := make(chan any, option.workers)

// 	go func() {
// 		var wg sync.WaitGroup

// 		for item := range s.source {
// 			// important, used in another goroutine
// 			val := item
// 			wg.Add(1)
// 			// better to safely run caller defined method
// 			threading.GoSafe(func() {
// 				defer wg.Done()
// 				fn(val, pipe)
// 			})
// 		}

// 		wg.Wait()
// 		close(pipe)
// 	}()

// 	return Range(pipe)
// }

func (s Stream) walkUnlimited(fn WalkFunc, option *rxOptions) Stream {
	pipe := make(chan any, option.workers)

	for item := range s.source {
		// important, used in another goroutine
		fn(item, pipe)
	}

	close(pipe)

	return Range(pipe)
}

func (s Stream) walkLimited(fn WalkFunc, option *rxOptions) Stream {
	pipe := make(chan any, option.workers)

	// pool := make(chan struct{}, option.workers)

	for item := range s.source {
		// important, used in another goroutine
		// pool <- struct{}{}

		// better to safely run caller defined method
		fn(item, pipe)
	}

	close(pipe)

	return Range(pipe)
}

// Distinct removes the duplicated items based on the given KeyFunc.
func (s Stream) Distinct(fn KeyFunc) Stream {

	source := make(chan any)

	go func() {
		defer close(source)

		keys := make(map[any]any)
		for item := range s.source {
			key := fn(item)
			if _, ok := keys[key]; !ok {
				source <- item
				keys[key] = item
			}
		}
	}()

	return Range(source)
}

// Last returns the last item, or nil if no items.
func (s Stream) Last() (item any) {
	for item = range s.source {
	}
	return
}

// Head returns the first n elements in p.
func (s Stream) Head(n int64) Stream {
	if n < 1 {
		panic("n must be greater than 0")
	}

	source := make(chan any)

	go func() {
		for item := range s.source {
			n--
			if n >= 0 {
				source <- item
			}
			if n == 0 {
				// let successive method go ASAP even we have more items to skip
				close(source)
				// why we don't just break the loop, and drain to consume all items.
				// because if breaks, this former goroutine will block forever,
				// which will cause goroutine leak.
				drain(s.source)
			}
		}
		// not enough items in s.source, but we need to let successive method to go ASAP.
		if n > 0 {
			close(source)
		}
	}()

	return Range(source)
}

func (s Stream) Map(fn MapFunc, opts ...Option) Stream {
	return s.Walk(func(item any, pipe chan<- any) {
		pipe <- fn(item)
	}, opts...)
}

// Sort sorts the items from the underlying source.
func (s Stream) Sort(less LessFunc) Stream {
	var items []any
	for item := range s.source {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		return less(items[i], items[j])
	})

	return Just(items...)
}

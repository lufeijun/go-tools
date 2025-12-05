package streams

import (
	"fmt"
	"testing"
)

func TestAdd(t *testing.T) {

	items := []any{1, 2, 3, 4, 5}

	str := Just(items...)

	// Just(items...).ForEach(func(item any) {
	// 	fmt.Println(item.(int))
	// })

	// result := 0
	// Just(items...).ForAll(func(pipe <-chan any) {
	// 	for item := range pipe {
	// 		result += item.(int)
	// 	}
	// })
	// fmt.Println("Sum:", result)

	// Just(items...).Filter(func(item any) bool {
	// 	return item.(int)%2 == 0
	// }, func(opts *rxOptions) {
	// 	opts.workers = 4
	// 	opts.unlimitedWorkers = true
	// }).ForEach(func(item any) {
	// 	fmt.Println("Even:", item.(int))
	// })

	// Just(items...).Distinct(func(item any) any {
	// 	return item.(int) % 3
	// }).ForEach(func(item any) {
	// 	fmt.Println("Even:", item.(int))
	// })

	// num := str.Last()
	// fmt.Println("Last:", num)

	// str.Head(2).
	// 	ForEach(func(item any) {
	// 		fmt.Println("Head:", item.(int))
	// 	})

	str.Map(func(item any) any {
		return item.(int) * 10
	}).
		ForEach(func(item any) {
			fmt.Println("map:", item.(int))
		})

}

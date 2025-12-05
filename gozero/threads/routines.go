package threads

import (
	"bytes"
	"context"
	"runtime"
	"strconv"

	"github.com/lufeijun/go-tools/gozero/rescue"
)

// 功能介绍
// - 封装了安全启动 goroutine 的工具：在新 goroutine 中执行用户函数并在发生 panic 时进行恢复（recover），避免 panic 传播导致程序崩溃。
// - 提供一个用于调试的 goroutine id 获取函数（仅调试用途，不建议生产使用）。

// GoSafe runs the given fn using another goroutine, recovers if fn panics.
func GoSafe(fn func()) {
	go RunSafe(fn)
}

func RunSafe(fn func()) {
	defer rescue.Recover()

	fn()
}

// GoSafeCtx runs the given fn using another goroutine, recovers if fn panics with ctx.
func GoSafeCtx(ctx context.Context, fn func()) {
	go RunSafeCtx(ctx, fn)
}

func RunSafeCtx(ctx context.Context, fn func()) {
	defer rescue.RecoverCtx(ctx)

	fn()
}

// RoutineId is only for debug, never use it in production.
func RoutineId() uint64 {
	b := make([]byte, 64)
	b = b[:runtime.Stack(b, false)]
	b = bytes.TrimPrefix(b, []byte("goroutine "))
	b = b[:bytes.IndexByte(b, ' ')]
	// if error, just return 0
	n, _ := strconv.ParseUint(string(b), 10, 64)

	return n
}

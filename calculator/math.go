package calculator

// 定义支持的数字类型
type Number interface {
	int | int8 | int16 | int32 | int64 | uint | uint8 | uint16 | uint32 | uint64 | float32 | float64
}

// 泛型加法
func Add[T Number](a, b T) T {
	return a + b
}

// 泛型减法
func Subtract[T Number](a, b T) T {
	return a - b
}

// 泛型乘法
func Multiply[T Number](a, b T) T {
	return a * b
}

// 泛型除法
func Divide[T Number](a, b T) T {
	if b == 0 {
		panic("division by zero")
	}
	return a / b
}

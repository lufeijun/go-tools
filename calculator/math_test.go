package calculator

import "testing"

// 测试表格：每组数据定义了输入和期望输出
func TestAdd(t *testing.T) {
	tests := []struct {
		name string
		a, b int
		want int
	}{
		{"positive", 2, 3, 5},
		{"negative", -1, -1, -2},
		{"zero", 0, 5, 5},
	}
	for _, tt := range tests {
		// t.Run为每个用例创建子测试，报告更清晰[citation:2]
		t.Run(tt.name, func(t *testing.T) {
			if got := Add(tt.a, tt.b); got != tt.want {
				t.Errorf("Add() = %v, want %v", got, tt.want)
			}
		})
	}
}

// 测试除法，特别是除零的情况
func TestDivide(t *testing.T) {
	tests := []struct {
		name        string
		a, b        float64
		want        float64
		shouldPanic bool
	}{
		{"normal", 10.0, 2.0, 5.0, false},
		{"zero_dividend", 0.0, 5.0, 0.0, false},
		{"by_zero", 10.0, 0.0, 0, true}, // 期望触发panic
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 处理期望发生panic的用例
			if tt.shouldPanic {
				defer func() {
					if r := recover(); r == nil {
						t.Errorf("Divide() did not panic as expected")
					}
				}()
			}
			got := Divide(tt.a, tt.b)
			if !tt.shouldPanic && got != tt.want {
				t.Errorf("Divide() = %v, want %v", got, tt.want)
			}
		})
	}
}

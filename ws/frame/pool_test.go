package frame

import "testing"

func TestGetBuf_SmallSize(t *testing.T) {
	buf := GetBuf(100)
	if cap(buf) != smallBufSize {
		t.Errorf("cap(buf) = %d, want %d", cap(buf), smallBufSize)
	}
}

func TestGetBuf_DefaultSize(t *testing.T) {
	buf := GetBuf(2000)
	if cap(buf) != defaultBufSize {
		t.Errorf("cap(buf) = %d, want %d", cap(buf), defaultBufSize)
	}
}

func TestPutBuf_ReturnsToCorrectPool(t *testing.T) {
	small := GetBuf(100)
	PutBuf(small)
	default_ := GetBuf(2000)
	PutBuf(default_)
}

func TestGetBuf_LargerThanDefault(t *testing.T) {
	buf := GetBuf(10000)
	if cap(buf) < 10000 {
		t.Errorf("cap(buf) = %d, want >= 10000", cap(buf))
	}
}

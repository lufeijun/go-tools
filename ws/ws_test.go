package ws

import (
	"runtime"
	"testing"
)

func TestConfig_ModeDefault(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Mode != ModeNet {
		t.Errorf("DefaultConfig Mode = %d, want %d (ModeNet)", cfg.Mode, ModeNet)
	}
}

func TestConfig_ModeZeroValueIsNet(t *testing.T) {
	var cfg Config
	if cfg.Mode != ModeNet {
		t.Errorf("zero-value Config Mode = %d, want %d (ModeNet)", cfg.Mode, ModeNet)
	}
}

func TestValidateMode_EpollOnLinux(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEpoll
	if runtime.GOOS == "linux" {
		if err := cfg.ValidateMode(); err != nil {
			t.Errorf("ValidateMode on linux: %v", err)
		}
	}
}

func TestValidateMode_EpollOnNonLinux(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Mode = ModeEpoll
	if runtime.GOOS != "linux" {
		if err := cfg.ValidateMode(); err == nil {
			t.Error("expected error for ModeEpoll on non-linux")
		}
	}
}
package hardware

import (
	"context"
	"runtime"
	"testing"
)

func TestDetect(t *testing.T) {
	p, err := Detect(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p.Os != runtime.GOOS || p.Arch != runtime.GOARCH {
		t.Errorf("os/arch = %s/%s", p.Os, p.Arch)
	}
	if p.CpuCores == 0 {
		t.Error("cpu cores = 0")
	}
	if len(p.Gpus) == 0 {
		t.Error("expected at least the cpu fallback gpu entry")
	}
	if BestAccel(p) == "" {
		t.Error("BestAccel empty")
	}
}

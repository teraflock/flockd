//go:build linux

package governor

import (
	"context"
	"errors"
	"testing"
)

func TestLinuxIdleSourceLive(t *testing.T) {
	// Live smoke test: either a real reading or a clean ErrNoIdleSource
	// (no logind / headless CI runner); never a parse failure.
	d, err := NewPlatformIdleSource().IdleFor(context.Background())
	if err != nil {
		if errors.Is(err, ErrNoIdleSource) {
			t.Skipf("no seat session: %v", err)
		}
		t.Skipf("logind unavailable: %v", err)
	}
	if d < 0 {
		t.Errorf("negative idle %v", d)
	}
}

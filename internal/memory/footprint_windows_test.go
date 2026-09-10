//go:build windows

package memory

import (
	"os"
	"runtime"
	"testing"
)

// Live probe: runs for real on the windows-latest CI lane (no Windows box
// in the mesh yet). The assertions are about the call working and moving
// with the process's own memory, not about any particular figure.
func TestWindowsFootprintTracksAllocation(t *testing.T) {
	before, err := processFootprintBytes(os.Getpid())
	if err != nil {
		t.Fatalf("GetProcessMemoryInfo(self): %v", err)
	}
	if before < 1*MiB {
		t.Fatalf("footprint of the test binary = %d bytes, implausibly small", before)
	}
	// Commit and touch 64 MiB: PrivateUsage grows on commit, WorkingSetSize
	// on touch, so the max grows either way.
	const grow = 64 * MiB
	buf := make([]byte, grow)
	for i := 0; i < len(buf); i += 4096 {
		buf[i] = 1
	}
	after, err := processFootprintBytes(os.Getpid())
	runtime.KeepAlive(buf)
	if err != nil {
		t.Fatal(err)
	}
	if after < before+grow/2 {
		t.Errorf("footprint %d -> %d bytes after allocating %d: did not track the allocation", before, after, grow)
	}
	if _, err := processFootprintBytes(0); err == nil {
		t.Error("pid 0 measured without error")
	}
}

//go:build !darwin && !linux

package memory

// TODO(windows): GetProcessMemoryInfo PrivateUsage via x/sys/windows.
func processFootprintBytes(int) (uint64, error) { return 0, ErrUnsupported }

//go:build !darwin && !linux && !windows

package memory

func processFootprintBytes(int) (uint64, error) { return 0, ErrUnsupported }

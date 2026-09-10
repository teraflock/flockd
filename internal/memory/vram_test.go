package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

func TestParseNvidiaSMIMemoryUsed(t *testing.T) {
	got, err := parseNvidiaSMIMemoryUsed("3210\n1024\n\n")
	if err != nil || got != 4234 {
		t.Fatalf("two GPUs: %d, %v", got, err)
	}
	if got, err := parseNvidiaSMIMemoryUsed(" 512 MiB\n"); err != nil || got != 512 {
		t.Fatalf("units suffix: %d, %v", got, err)
	}
	if _, err := parseNvidiaSMIMemoryUsed(""); err == nil {
		t.Fatal("empty output accepted")
	}
	if _, err := parseNvidiaSMIMemoryUsed("N/A\n"); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestVRAMSamplerVendorGating(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	calls := 0
	nvidia := &typesv1.CapabilityProfile{RamTotalMb: 32768, Gpus: []*typesv1.GpuInfo{{Vendor: "nvidia", VramMb: 24576}}}
	s := NewVRAMSampler(nvidia, quiet)
	s.Query = func(context.Context) (int64, error) { calls++; return 9000, nil }
	if mb, ok := s.Sample(context.Background()); !ok || mb != 9000 {
		t.Fatalf("nvidia sample = %d, %v", mb, ok)
	}
	// A failing query keeps the estimate but does not disable sampling.
	s.Query = func(context.Context) (int64, error) { calls++; return 0, errors.New("timeout") }
	if _, ok := s.Sample(context.Background()); ok {
		t.Fatal("failed query reported a measurement")
	}
	s.Query = func(context.Context) (int64, error) { calls++; return 100, nil }
	if _, ok := s.Sample(context.Background()); !ok {
		t.Fatal("sampler disabled after a transient failure")
	}
	// A missing tool disables it for good: no further exec attempts.
	s.Query = func(context.Context) (int64, error) { calls++; return 0, ErrNoGPUTool }
	s.Sample(context.Background())
	before := calls
	s.Sample(context.Background())
	if calls != before {
		t.Fatal("sampler kept querying after ErrNoGPUTool")
	}

	// AMD is measured through its own source (sysfs / rocm-smi).
	amd := NewVRAMSampler(&typesv1.CapabilityProfile{RamTotalMb: 16384, Gpus: []*typesv1.GpuInfo{{Vendor: "amd", VramMb: 16384}}}, quiet)
	amd.Query = func(context.Context) (int64, error) { return 321, nil }
	if mb, ok := amd.Sample(context.Background()); !ok || mb != 321 {
		t.Fatalf("amd sample = %d, %v", mb, ok)
	}

	// Unified memory, CPU-only and unmeasured-vendor nodes never exec
	// anything.
	for _, hw := range []*typesv1.CapabilityProfile{
		{RamTotalMb: 65536, Gpus: []*typesv1.GpuInfo{{Vendor: "apple", VramMb: 65536, UnifiedMemory: true}}},
		{RamTotalMb: 16384, Gpus: []*typesv1.GpuInfo{{Vendor: "none"}}},
		{RamTotalMb: 16384, Gpus: []*typesv1.GpuInfo{{Vendor: "intel", VramMb: 8192}}},
	} {
		s := NewVRAMSampler(hw, quiet)
		s.Query = func(context.Context) (int64, error) { t.Fatal("query called"); return 0, nil }
		if _, ok := s.Sample(context.Background()); ok {
			t.Fatalf("%v: reported a measurement", hw.GetGpus())
		}
	}
}

func TestNvidiaVRAMUsedMBWithoutTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := NvidiaVRAMUsedMB(context.Background()); !errors.Is(err, ErrNoGPUTool) {
		t.Fatalf("err = %v, want ErrNoGPUTool", err)
	}
}

func TestVRAMSamplerDefaultQueryFollowsVendor(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	for vendor, want := range map[string]bool{"nvidia": true, "amd": true, "intel": false, "none": false} {
		s := NewVRAMSampler(&typesv1.CapabilityProfile{RamTotalMb: 16384, Gpus: []*typesv1.GpuInfo{{Vendor: vendor, VramMb: 8192}}}, quiet)
		if got := s.Query != nil; got != want {
			t.Errorf("%s: default query set = %v, want %v", vendor, got, want)
		}
	}
}

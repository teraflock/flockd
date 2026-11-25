//go:build linux || windows

package hardware

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A candidate that does not exist is skipped, not an error, and a bare
// name that is not on PATH yields nil — the CPU-only fallback path.
func TestDetectNvidiaMissingTool(t *testing.T) {
	got := detectNvidia(context.Background(), "definitely-not-nvidia-smi-xyz", `/nonexistent/dir/nvidia-smi`)
	if got != nil {
		t.Errorf("detectNvidia with no tool = %v, want nil", got)
	}
}

// fakeNvidiaSMI writes a shell script standing in for nvidia-smi. With
// capSupported it answers the compute_cap query with capRows; otherwise it
// rejects that query the way a driver that predates the field does (error
// text, exit 2) and only answers the three-column one.
func fakeNvidiaSMI(t *testing.T, capSupported bool, capRows, baseRows string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script nvidia-smi stand-in; the parsers are covered on every OS in nvidia_test.go")
	}
	script := "#!/bin/sh\ncase \"$1\" in\n*compute_cap*)\n"
	if capSupported {
		script += "cat <<'EOF'\n" + capRows + "EOF\nexit 0\n"
	} else {
		script += "echo 'Field \"compute_cap\" is not a valid field to query.'\nexit 2\n"
	}
	script += ";;\nesac\ncat <<'EOF'\n" + baseRows + "EOF\n"
	path := filepath.Join(t.TempDir(), "nvidia-smi")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureDefaultLog redirects slog.Default for the test and returns the
// buffer it writes to.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestDetectNvidiaComputeCap(t *testing.T) {
	const requirement = "the CUDA runtime needs 7.5 or newer (RTX 20-series / GTX 16-series or newer)"
	cases := []struct {
		name         string
		capSupported bool
		capRows      string
		baseRows     string
		wantAccel    []string
		wantLog      string // "" = nothing logged
	}{
		{
			name:         "ada 8.9: cuda12, silent",
			capSupported: true,
			capRows:      "NVIDIA GeForce RTX 4090, 24564, 560.94, 8.9\n",
			baseRows:     "NVIDIA GeForce RTX 4090, 24564, 560.94\n",
			wantAccel:    []string{"cuda12"},
		},
		{
			name:         "pascal 6.1: off the cuda lane, operator told why",
			capSupported: true,
			capRows:      "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94, 6.1\n",
			baseRows:     "NVIDIA GeForce GTX 1080 Ti, 11264, 560.94\n",
			wantAccel:    []string{""},
			wantLog:      "NVIDIA GeForce GTX 1080 Ti has compute capability 6.1",
		},
		{
			name:         "old driver without compute_cap: fallback query, cuda12 as before",
			capSupported: false,
			baseRows:     "NVIDIA GeForce GTX 1080 Ti, 11264, 390.157\n",
			wantAccel:    []string{"cuda12"},
		},
		{
			name:         "mixed box: both stay on cuda12, old card warned about",
			capSupported: true,
			capRows:      "NVIDIA GeForce GTX 1080, 8192, 560.94, 6.1\nNVIDIA GeForce RTX 4070, 12282, 560.94, 8.9\n",
			baseRows:     "NVIDIA GeForce GTX 1080, 8192, 560.94\nNVIDIA GeForce RTX 4070, 12282, 560.94\n",
			wantAccel:    []string{"cuda12", "cuda12"},
			wantLog:      "NVIDIA GeForce GTX 1080 has compute capability 6.1",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			smi := fakeNvidiaSMI(t, c.capSupported, c.capRows, c.baseRows)
			logs := captureDefaultLog(t)
			// The missing first candidate is skipped, as on a box where
			// nvidia-smi is only at its absolute path.
			gpus := detectNvidia(context.Background(), "definitely-not-nvidia-smi-xyz", smi)
			if len(gpus) != len(c.wantAccel) {
				t.Fatalf("gpus = %+v, want %d", gpus, len(c.wantAccel))
			}
			for i, g := range gpus {
				if g.Accel != c.wantAccel[i] || g.Vendor != "nvidia" || g.VramMb == 0 || g.Model == "" || g.DriverVersion == "" {
					t.Errorf("gpu[%d] = %+v, want Accel %q", i, g, c.wantAccel[i])
				}
			}
			out := logs.String()
			switch {
			case c.wantLog == "" && out != "":
				t.Errorf("unexpected log output: %s", out)
			case c.wantLog != "" && (!strings.Contains(out, c.wantLog) || !strings.Contains(out, requirement) ||
				!strings.Contains(out, "level=WARN") || strings.Count(out, "\n") != 1):
				t.Errorf("log = %q, want one WARN line containing %q and %q", out, c.wantLog, requirement)
			}
		})
	}
}

// A tool that fails both queries (driver not loaded) means no NVIDIA GPUs,
// and later candidates are not consulted — unchanged behaviour.
func TestDetectNvidiaToolFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script nvidia-smi stand-in")
	}
	dir := t.TempDir()
	failing := filepath.Join(dir, "nvidia-smi-failing")
	script := "#!/bin/sh\necho \"NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\"\nexit 9\n"
	if err := os.WriteFile(failing, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	working := fakeNvidiaSMI(t, true, "NVIDIA GeForce RTX 4090, 24564, 560.94, 8.9\n", "NVIDIA GeForce RTX 4090, 24564, 560.94\n")
	logs := captureDefaultLog(t)
	if got := detectNvidia(context.Background(), failing, working); got != nil {
		t.Errorf("detectNvidia = %+v, want nil", got)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected log output: %s", logs)
	}
}

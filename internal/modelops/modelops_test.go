package modelops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// harness stands up an artifact server, a one-model catalog file, a manager
// and an engine, all wired into a Service backed by the mock runtime.
func harness(t *testing.T, id string, blob []byte) (*Service, *engine.Engine) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(blob)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	catalog := fmt.Sprintf(
		"models:\n  - id: %s\n    sha256: %s\n    artifact_url: %s/%s\n    size_bytes: %d\n",
		id, shaOf(blob), srv.URL, id, len(blob),
	)
	catPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}

	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(nil, nil, nil)
	return &Service{
		Mgr:          mgr,
		Eng:          eng,
		Loader:       rt.NewMockRuntime(0),
		Budget:       rt.ResourceBudget{MaxConcurrent: 2},
		Log:          quietLog(),
		ManifestPath: catPath,
	}, eng
}

func TestStartDownloadThenReady(t *testing.T) {
	blob := []byte("gguf bytes")
	svc, _ := harness(t, "tiny-model", blob)

	started, err := svc.StartDownload(context.Background(), "tiny-model")
	if err != nil || !started {
		t.Fatalf("StartDownload = %v, %v", started, err)
	}
	deadline := time.After(5 * time.Second)
	for {
		rows := svc.Mgr.List()
		if len(rows) == 1 && rows[0].State == "ready" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("download never finished: %+v", rows)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Second call: already cached.
	started, err = svc.StartDownload(context.Background(), "tiny-model")
	if err != nil || started {
		t.Fatalf("second StartDownload = %v, %v (want false, nil)", started, err)
	}
}

func TestStartDownloadUnknownModel(t *testing.T) {
	svc, _ := harness(t, "tiny-model", []byte("x"))
	if _, err := svc.StartDownload(context.Background(), "no-such-model"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel", err)
	}
}

func TestLoadRegistersAndSwitchesDefault(t *testing.T) {
	svc, eng := harness(t, "tiny-model", []byte("gguf bytes"))
	if err := svc.Load(context.Background(), "tiny-model"); err != nil {
		t.Fatal(err)
	}
	if eng.DefaultModel() != "tiny-model" {
		t.Fatalf("default = %q", eng.DefaultModel())
	}
	// Loading again is a no-op.
	if err := svc.Load(context.Background(), "tiny-model"); err != nil {
		t.Fatal(err)
	}
	if got := len(eng.Models()); got != 1 {
		t.Fatalf("models loaded = %d, want 1", got)
	}
	if err := svc.SetDefault("nope"); err == nil {
		t.Fatal("SetDefault on unloaded model should fail")
	}
}

func TestUnload(t *testing.T) {
	svc, eng := harness(t, "tiny-model", []byte("gguf bytes"))
	if err := svc.Load(context.Background(), "tiny-model"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unload(context.Background(), "tiny-model"); err != nil {
		t.Fatal(err)
	}
	if got := len(eng.Models()); got != 0 {
		t.Fatalf("models loaded = %d, want 0", got)
	}
	if err := svc.Unload(context.Background(), "tiny-model"); err == nil {
		t.Fatal("second unload should fail")
	}
}

func TestCancelDownload(t *testing.T) {
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(hang)

	dir := t.TempDir()
	catalog := fmt.Sprintf(
		"models:\n  - id: big-model\n    sha256: %s\n    artifact_url: %s/big\n    size_bytes: 1000000\n",
		shaOf([]byte("whatever")), srv.URL,
	)
	catPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catPath, []byte(catalog), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		Mgr: mgr, Eng: engine.New(nil, nil, nil), Loader: rt.NewMockRuntime(0),
		Log: quietLog(), ManifestPath: catPath,
	}

	if _, err := svc.StartDownload(context.Background(), "big-model"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for !svc.Downloading("big-model") {
		select {
		case <-deadline:
			t.Fatal("download never started")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !svc.CancelDownload("big-model") {
		t.Fatal("cancel reported no download")
	}
	for svc.Downloading("big-model") {
		select {
		case <-deadline:
			t.Fatal("download never stopped after cancel")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// Cancelled download leaves no cache entry; a later attempt restarts.
	if rows := mgr.List(); len(rows) != 0 {
		t.Fatalf("cancelled download left entries: %+v", rows)
	}
}

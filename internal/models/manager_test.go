package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// artifactServer serves named blobs with Range support.
func artifactServer(t *testing.T, blobs map[string][]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blob, ok := blobs[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var off int
			if _, err := parseRange(rng, &off); err == nil && off < len(blob) {
				w.Header().Set("Content-Range", "bytes "+strconv.Itoa(off)+"-")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(blob[off:])
				return
			}
		}
		_, _ = w.Write(blob)
	}))
}

func parseRange(h string, off *int) (int, error) {
	h = strings.TrimPrefix(h, "bytes=")
	h = strings.TrimSuffix(h, "-")
	n, err := strconv.Atoi(h)
	*off = n
	return n, err
}

func spec(id string, blob []byte, url string) *typesv1.ModelSpec {
	return &typesv1.ModelSpec{
		Id:          id,
		Sha256:      shaOf(blob),
		ArtifactUrl: url + "/" + id,
		SizeBytes:   uint64(len(blob)),
	}
}

func TestEnsureDownloadVerifyAndCache(t *testing.T) {
	blob := []byte("gguf-bytes-model-a")
	srv := artifactServer(t, map[string][]byte{"model-a": blob})
	defer srv.Close()

	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure(context.Background(), spec("model-a", blob, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(blob) {
		t.Fatal("content mismatch")
	}
	if err := m.Verify("model-a"); err != nil {
		t.Fatal(err)
	}
	// Cache hit: works even with the server down.
	srv.Close()
	if _, err := m.Ensure(context.Background(), spec("model-a", blob, srv.URL)); err != nil {
		t.Fatalf("cache hit failed: %v", err)
	}
}

func TestValidateID(t *testing.T) {
	valid := []string{
		"llama-3.2-3b-instruct-q4_k_m",
		"nomic-embed-text-v1.5-q8_0",
		"a",
	}
	for _, id := range valid {
		if err := ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q) = %v, want nil", id, err)
		}
	}
	invalid := []string{
		"",
		"../../etc/cron.d/evil",
		"..",
		"a/../../b",
		"sub/dir",
		`sub\dir`,
		".hidden",
		"-leading-dash",
		"has space",
		"nul\x00byte",
		strings.Repeat("x", maxIDLen+1),
	}
	for _, id := range invalid {
		if err := ValidateID(id); !errors.Is(err, ErrInvalidID) {
			t.Errorf("ValidateID(%q) = %v, want ErrInvalidID", id, err)
		}
	}
}

// A catalog is fetched over the network, so a hostile entry must not be able
// to turn its id into a path outside the models dir.
func TestEnsureRejectsTraversalID(t *testing.T) {
	blob := []byte("payload")
	srv := artifactServer(t, map[string][]byte{"evil": blob})
	defer srv.Close()

	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	sp := spec("../../evil", blob, srv.URL)
	if _, err := m.Ensure(context.Background(), sp); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID", err)
	}
	// Nothing may have been written outside the cache dir.
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "evil.gguf")); !errors.Is(err, os.ErrNotExist) {
		t.Error("traversal wrote outside the models dir")
	}
}

func TestRemoveRejectsTraversalID(t *testing.T) {
	m, _ := NewManager(t.TempDir(), 0, quietLog())
	if err := m.Remove("../../etc/passwd"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("err = %v, want ErrInvalidID", err)
	}
}

func TestEnsureRefusesSHAMismatch(t *testing.T) {
	blob := []byte("real bytes")
	srv := artifactServer(t, map[string][]byte{"model-x": []byte("tampered bytes")})
	defer srv.Close()

	m, _ := NewManager(t.TempDir(), 0, quietLog())
	sp := spec("model-x", blob, srv.URL) // sha of real, serves tampered
	_, err := m.Ensure(context.Background(), sp)
	if !errors.Is(err, ErrSHAMismatch) {
		t.Fatalf("err = %v, want ErrSHAMismatch", err)
	}
	if _, err := os.Stat(m.Path("model-x") + ".partial"); !errors.Is(err, os.ErrNotExist) {
		t.Error("poisoned partial must be deleted")
	}
}

func TestResumeFromPartial(t *testing.T) {
	blob := []byte("0123456789abcdefghij-full-model-content")
	srv := artifactServer(t, map[string][]byte{"model-r": blob})
	defer srv.Close()

	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	// Simulate an interrupted download: first 10 bytes already on disk.
	if err := os.WriteFile(m.Path("model-r")+".partial", blob[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := m.Ensure(context.Background(), spec("model-r", blob, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(blob) {
		t.Fatalf("resumed content mismatch: %q", got)
	}
}

func TestLRUEvictionUnderBudget(t *testing.T) {
	// Budget 3MB; three 1MB models + a fourth forces eviction of the LRU.
	mb := 1024 * 1024
	blobs := map[string][]byte{}
	for _, id := range []string{"m1", "m2", "m3", "m4"} {
		blobs[id] = []byte(strings.Repeat(id[:1]+id[1:], mb/2))
	}
	srv := artifactServer(t, blobs)
	defer srv.Close()

	m, _ := NewManager(t.TempDir(), 3, quietLog())
	ctx := context.Background()
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := m.Ensure(ctx, spec(id, blobs[id], srv.URL)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // distinct LastUsed
	}
	m.Touch("m1") // m2 becomes the LRU

	if _, err := m.Ensure(ctx, spec("m4", blobs["m4"], srv.URL)); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, i := range m.List() {
		ids[i.ID] = true
	}
	if ids["m2"] {
		t.Error("m2 (LRU) should have been evicted")
	}
	if !ids["m1"] || !ids["m3"] || !ids["m4"] {
		t.Errorf("cache = %v", ids)
	}
	if _, err := os.Stat(m.Path("m2")); !errors.Is(err, os.ErrNotExist) {
		t.Error("m2 file should be gone")
	}
}

func TestPinnedModelsSurviveEviction(t *testing.T) {
	mb := 1024 * 1024
	blobs := map[string][]byte{
		"p1": []byte(strings.Repeat("p", 2*mb)),
		"p2": []byte(strings.Repeat("q", 2*mb)),
	}
	srv := artifactServer(t, blobs)
	defer srv.Close()

	m, _ := NewManager(t.TempDir(), 3, quietLog())
	ctx := context.Background()
	if _, err := m.Ensure(ctx, spec("p1", blobs["p1"], srv.URL)); err != nil {
		t.Fatal(err)
	}
	if err := m.Pin("p1", true); err != nil {
		t.Fatal(err)
	}
	// p2 (2MB) cannot fit: p1 is pinned and budget is 3MB total.
	if _, err := m.Ensure(ctx, spec("p2", blobs["p2"], srv.URL)); err == nil {
		t.Fatal("expected budget error with pinned occupant")
	}
	if _, err := os.Stat(m.Path("p1")); err != nil {
		t.Error("pinned model must survive")
	}
}

func TestRemoveAndList(t *testing.T) {
	blob := []byte("bytes")
	srv := artifactServer(t, map[string][]byte{"m": blob})
	defer srv.Close()
	m, _ := NewManager(t.TempDir(), 0, quietLog())
	if _, err := m.Ensure(context.Background(), spec("m", blob, srv.URL)); err != nil {
		t.Fatal(err)
	}
	if got := m.List(); len(got) != 1 || got[0].State != "ready" {
		t.Fatalf("list = %+v", got)
	}
	if err := m.Remove("m"); err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 {
		t.Fatal("list not empty after remove")
	}
	if err := m.Remove("m"); err == nil {
		t.Fatal("removing unknown model should error")
	}
}

func TestStatePersistsAcrossRestarts(t *testing.T) {
	blob := []byte("persist me")
	srv := artifactServer(t, map[string][]byte{"m": blob})
	defer srv.Close()
	dir := t.TempDir()
	m1, _ := NewManager(dir, 0, quietLog())
	if _, err := m1.Ensure(context.Background(), spec("m", blob, srv.URL)); err != nil {
		t.Fatal(err)
	}
	_ = m1.Pin("m", true)

	m2, err := NewManager(dir, 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	got := m2.List()
	if len(got) != 1 || !got[0].Pinned {
		t.Fatalf("restarted state = %+v", got)
	}
}

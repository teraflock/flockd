package models

import (
	"bytes"
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
	"sync/atomic"
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

// An upstream re-upload moves the manifest's hash: the cached file is
// now the wrong artifact and must be replaced on the next Ensure, not
// served because it happens to exist.
func TestEnsureRefetchesWhenManifestHashMoves(t *testing.T) {
	old := []byte("gguf-bytes-model-a-v1")
	blobs := map[string][]byte{"model-a": old}
	srv := artifactServer(t, blobs)
	defer srv.Close()

	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(context.Background(), spec("model-a", old, srv.URL)); err != nil {
		t.Fatal(err)
	}

	// Upstream replaced the file; the catalog now pins the new hash.
	fresh := []byte("gguf-bytes-model-a-v2-reuploaded")
	blobs["model-a"] = fresh
	path, err := m.Ensure(context.Background(), spec("model-a", fresh, srv.URL))
	if err != nil {
		t.Fatalf("ensure after hash move: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(fresh) {
		t.Fatalf("served the superseded artifact: %q", got)
	}
	if err := m.Verify("model-a"); err != nil {
		t.Fatal(err)
	}
	// A placeholder hash on the manifest is not a move: the cached file
	// stays (drafts are allowed to carry TODO-verify).
	draft := spec("model-a", fresh, srv.URL)
	draft.Sha256 = "TODO-verify"
	srv.Close()
	if _, err := m.Ensure(context.Background(), draft); err != nil {
		t.Fatalf("placeholder hash evicted a good file: %v", err)
	}
}

// gatedServer serves one blob whose second half waits on gate, so a test
// can act while the download is in flight. The request context ending
// (a cancel) releases the handler too.
func gatedServer(t *testing.T, blob []byte, gate chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(blob[:len(blob)/2])
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write(blob[len(blob)/2:])
	}))
}

// Any download can be cancelled by id, not only the ones the operator
// started: a mesh placement's fetch here. The entry is gone afterwards
// and a second cancel finds nothing in flight.
func TestCancelDownloadAnyOrigin(t *testing.T) {
	blob := bytes.Repeat([]byte("m"), 64*1024)
	gate := make(chan struct{})
	srv := gatedServer(t, blob, gate)
	defer srv.Close()
	defer close(gate)

	m, _ := NewManager(t.TempDir(), 0, quietLog())
	done := make(chan error, 1)
	go func() {
		_, err := m.EnsureArtifact(context.Background(), spec("mesh-a", blob, srv.URL), OriginMesh)
		done <- err
	}()
	waitFor(t, func() bool { _, ok := m.DownloadProgress("mesh-a"); return ok })

	if !m.CancelDownload("mesh-a") {
		t.Fatal("cancel found no download in flight")
	}
	if err := <-done; err == nil {
		t.Fatal("cancelled download reported success")
	}
	for _, i := range m.List() {
		if i.ID == "mesh-a" {
			t.Fatalf("entry survived the cancel: %+v", i)
		}
	}
	if m.CancelDownload("mesh-a") {
		t.Fatal("second cancel claimed a download in flight")
	}
}

// Remove during a download stops it, and leaves neither the entry nor the
// .partial behind: the finishing goroutine used to re-add the entry as
// ready after the operator had removed it.
func TestRemoveDuringDownloadCancelsAndDropsPartial(t *testing.T) {
	blob := bytes.Repeat([]byte("r"), 64*1024)
	gate := make(chan struct{})
	srv := gatedServer(t, blob, gate)
	defer srv.Close()
	defer close(gate)

	m, _ := NewManager(t.TempDir(), 0, quietLog())
	done := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), spec("rm-a", blob, srv.URL))
		done <- err
	}()
	waitFor(t, func() bool { _, ok := m.DownloadProgress("rm-a"); return ok })

	if err := m.Remove("rm-a"); err != nil {
		t.Fatalf("remove during download: %v", err)
	}
	<-done
	for _, i := range m.List() {
		if i.ID == "rm-a" {
			t.Fatalf("entry came back after remove: %+v", i)
		}
	}
	if _, err := os.Stat(m.Path("rm-a") + ".partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remove left the .partial behind")
	}
}

// A "downloading" entry written by a previous process is forgotten at
// start: nothing owns it, so it would show 0% forever, with no cancel
// and no remove.
func TestStaleDownloadingEntryForgottenOnStart(t *testing.T) {
	dir := t.TempDir()
	state := `{"entries":{"stale":{"id":"stale","sha256":"abc","state":"downloading"},"good":{"id":"good","sha256":"def","state":"ready"}}}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good.gguf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(dir, 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, i := range m.List() {
		ids[i.ID] = true
	}
	if ids["stale"] || !ids["good"] {
		t.Fatalf("cache after start = %v", ids)
	}
}

// Too big to ever fit evicts nothing: before, the pre-flight wiped every
// evictable model and then refused anyway.
func TestOverBudgetEvictsNothingWhenItCannotFit(t *testing.T) {
	mb := 1024 * 1024
	blobs := map[string][]byte{
		"s1":  []byte(strings.Repeat("a", mb)),
		"s2":  []byte(strings.Repeat("b", mb)),
		"big": []byte(strings.Repeat("c", 4*mb)),
	}
	srv := artifactServer(t, blobs)
	defer srv.Close()

	m, _ := NewManager(t.TempDir(), 3, quietLog())
	ctx := context.Background()
	for _, id := range []string{"s1", "s2"} {
		if _, err := m.Ensure(ctx, spec(id, blobs[id], srv.URL)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Ensure(ctx, spec("big", blobs["big"], srv.URL)); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("err = %v, want ErrOverBudget", err)
	}
	for _, id := range []string{"s1", "s2"} {
		if _, err := os.Stat(m.Path(id)); err != nil {
			t.Fatalf("%s was evicted for a model that could never fit", id)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
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

func TestDownloadProgressAndDownloadingState(t *testing.T) {
	blob := bytes.Repeat([]byte("x"), 64*1024)
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// First half, then hold until released — the test inspects state
		// mid-download.
		_, _ = w.Write(blob[:32*1024])
		w.(http.Flusher).Flush()
		<-gate
		_, _ = w.Write(blob[32*1024:])
	}))
	defer srv.Close()
	defer close(gate)

	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	sp := &typesv1.ModelSpec{
		Id: "prog-model", Sha256: shaOf(blob),
		ArtifactUrl: srv.URL + "/blob", SizeBytes: uint64(len(blob)),
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), sp)
		done <- err
	}()

	// Mid-download: entry is "downloading" with live byte progress.
	deadline := time.After(5 * time.Second)
	for {
		rows := m.List()
		if len(rows) == 1 && rows[0].State == "downloading" && rows[0].ReceivedBytes > 0 {
			if rows[0].SizeBytes != int64(len(blob)) {
				t.Fatalf("SizeBytes = %d, want %d", rows[0].SizeBytes, len(blob))
			}
			if p, ok := m.DownloadProgress("prog-model"); !ok || p.TotalBytes != int64(len(blob)) {
				t.Fatalf("DownloadProgress = %+v ok=%v", p, ok)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never observed downloading state: %+v", rows)
		case <-time.After(10 * time.Millisecond):
		}
	}

	gate <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rows := m.List()
	if len(rows) != 1 || rows[0].State != "ready" || rows[0].ReceivedBytes != 0 {
		t.Fatalf("after download: %+v", rows)
	}
	if _, ok := m.DownloadProgress("prog-model"); ok {
		t.Fatal("progress should be cleared after completion")
	}
}

func TestDownloadRetriesTransientFailureThenSucceeds(t *testing.T) {
	blob := []byte("hello world")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) <= 2 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(blob)
	}))
	defer srv.Close()

	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	m.DownloadBackoffMin = time.Millisecond
	m.DownloadBackoffMax = time.Millisecond
	sp := &typesv1.ModelSpec{Id: "retry-model", Sha256: shaOf(blob), ArtifactUrl: srv.URL + "/blob", SizeBytes: uint64(len(blob))}

	if _, err := m.Ensure(context.Background(), sp); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("hits = %d, want 3 (2 rate-limited attempts then success)", n)
	}
	if !m.Has("retry-model") {
		t.Fatal("not ready after a retried download")
	}
}

func TestDownloadGivesUpAfterMaxAttempts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "always busy", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	m.DownloadMaxAttempts = 3
	m.DownloadBackoffMin = time.Millisecond
	m.DownloadBackoffMax = time.Millisecond
	sp := &typesv1.ModelSpec{Id: "always-429", Sha256: shaOf([]byte("y")), ArtifactUrl: srv.URL + "/x", SizeBytes: 1}

	if _, err := m.Ensure(context.Background(), sp); err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("hits = %d, want 3 (DownloadMaxAttempts)", n)
	}
}

func TestFailedDownloadLeavesNoEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	m.DownloadMaxAttempts = 1 // a 500 is retried in production; this test wants the fast, final failure
	sp := &typesv1.ModelSpec{Id: "failing", Sha256: shaOf([]byte("y")), ArtifactUrl: srv.URL + "/x", SizeBytes: 1}
	if _, err := m.Ensure(context.Background(), sp); err == nil {
		t.Fatal("expected error")
	}
	if rows := m.List(); len(rows) != 0 {
		t.Fatalf("failed download left entries: %+v", rows)
	}
}

package models

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// shardedFixture is a small sharded model: n parts named the way upstream
// names them, plus an mmproj sidecar when asked, served from srvURL.
// blobs is filled in for the caller's artifactServer.
func shardedFixture(id, srvURL string, n int, mmproj bool, blobs map[string][]byte) *typesv1.ModelSpec {
	spec := &typesv1.ModelSpec{Id: id}
	var hashes []string
	for i := 1; i <= n; i++ {
		name := id + "-0000" + string(rune('0'+i)) + "-of-0000" + string(rune('0'+n)) + ".gguf"
		blob := bytes.Repeat([]byte{byte('a' + i)}, 3000+i)
		blobs[name] = blob
		spec.Parts = append(spec.Parts, &typesv1.ArtifactPart{Url: srvURL + "/" + name, Sha256: shaOf(blob), SizeBytes: uint64(len(blob))})
		spec.SizeBytes += uint64(len(blob))
		hashes = append(hashes, shaOf(blob))
	}
	spec.Sha256 = CompositeSHA256(hashes)
	if mmproj {
		blob := []byte("projector weights")
		blobs["mmproj-F16.gguf"] = blob
		spec.Mmproj = &typesv1.ArtifactPart{Url: srvURL + "/mmproj-F16.gguf", Sha256: shaOf(blob), SizeBytes: uint64(len(blob))}
	}
	return spec
}

func totalOf(blobs map[string][]byte) int64 {
	var n int64
	for _, b := range blobs {
		n += int64(len(b))
	}
	return n
}

func TestMultiPartDownloadLaysOutSiblingsAndVerifies(t *testing.T) {
	blobs := map[string][]byte{}
	srv := artifactServer(t, blobs)
	defer srv.Close()
	spec := shardedFixture("big", srv.URL, 3, true, blobs)

	dir := t.TempDir()
	m, err := NewManager(dir, 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	a, err := m.EnsureArtifact(context.Background(), spec, OriginOperator)
	if err != nil {
		t.Fatal(err)
	}
	// llama-server gets part 1 and finds the rest next to it under the
	// upstream names.
	if want := filepath.Join(dir, "big", "big-00001-of-00003.gguf"); a.Path != want {
		t.Fatalf("Path = %s, want %s", a.Path, want)
	}
	if want := filepath.Join(dir, "big", "mmproj-F16.gguf"); a.MmprojPath != want {
		t.Fatalf("MmprojPath = %s, want %s", a.MmprojPath, want)
	}
	if a.SizeBytes != totalOf(blobs) {
		t.Fatalf("SizeBytes = %d, want %d", a.SizeBytes, totalOf(blobs))
	}
	for name, blob := range blobs {
		got, err := os.ReadFile(filepath.Join(dir, "big", name))
		if err != nil || !bytes.Equal(got, blob) {
			t.Fatalf("%s: %v / content mismatch", name, err)
		}
	}
	if m.Path("big") != a.Path {
		t.Fatalf("Path(id) = %s", m.Path("big"))
	}
	if err := m.Verify("big"); err != nil {
		t.Fatal(err)
	}
	rows := m.List()
	if len(rows) != 1 || rows[0].State != StateReady || rows[0].PartsTotal != 4 || rows[0].PartsDone != 4 ||
		rows[0].SizeBytes != totalOf(blobs) || rows[0].Path != a.Path {
		t.Fatalf("List = %+v", rows)
	}
	if st := m.Stats(); st.ModelsBytes != totalOf(blobs) || st.PartialBytes != 0 {
		t.Fatalf("Stats = %+v", st)
	}
	// The composite id is what the entry reports; it is never a file hash.
	m.mu.Lock()
	sha := m.state.Entries["big"].SHA256
	m.mu.Unlock()
	if sha != spec.Sha256 {
		t.Fatalf("entry sha = %s, want composite %s", sha, spec.Sha256)
	}

	// Cache hit survives a restart and the server going away.
	srv.Close()
	m2, err := NewManager(dir, 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Has("big") {
		t.Fatal("multi-part entry not reloaded from models.json")
	}
	a2, err := m2.EnsureArtifact(context.Background(), spec, OriginMesh)
	if err != nil || a2 != a {
		t.Fatalf("cache hit: %+v %v", a2, err)
	}
	if m2.Origin("big") != OriginOperator {
		t.Fatal("a mesh re-send relabelled the operator's model")
	}
}

func TestMultiPartProgressSumsOverParts(t *testing.T) {
	blobs := map[string][]byte{}
	gate := make(chan struct{})
	var released atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		blob := blobs[name]
		if strings.Contains(name, "00002-of") && !released.Load() {
			_, _ = w.Write(blob[:1000])
			w.(http.Flusher).Flush()
			<-gate
			_, _ = w.Write(blob[1000:])
			return
		}
		_, _ = w.Write(blob)
	}))
	defer srv.Close()
	spec := shardedFixture("prog", srv.URL, 3, false, blobs)

	m, _ := NewManager(t.TempDir(), 0, quietLog())
	done := make(chan error, 1)
	go func() {
		_, err := m.Ensure(context.Background(), spec)
		done <- err
	}()
	part1 := int64(len(blobs["prog-00001-of-00003.gguf"]))
	deadline := time.After(5 * time.Second)
	for {
		rows := m.List()
		if len(rows) == 1 && rows[0].State == StateDownloading && rows[0].ReceivedBytes >= part1+1000 {
			if rows[0].PartsTotal != 3 || rows[0].PartsDone != 1 {
				t.Fatalf("mid-download row = %+v", rows[0])
			}
			if rows[0].SizeBytes != int64(spec.SizeBytes) {
				t.Fatalf("SizeBytes = %d, want %d", rows[0].SizeBytes, spec.SizeBytes)
			}
			if p, _ := m.DownloadProgress("prog"); p.TotalBytes != int64(spec.SizeBytes) {
				t.Fatalf("progress = %+v", p)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("never saw summed progress: %+v", rows)
		case <-time.After(5 * time.Millisecond):
		}
	}
	released.Store(true)
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rows := m.List(); rows[0].State != StateReady || rows[0].PartsDone != 3 || rows[0].ReceivedBytes != 0 {
		t.Fatalf("after: %+v", rows)
	}
}

func TestMultiPartResumeNeverRefetchesAVerifiedPart(t *testing.T) {
	blobs := map[string][]byte{}
	var hits = map[string]*atomic.Int32{}
	var failPart3 atomic.Bool
	failPart3.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		hits[name].Add(1)
		if strings.Contains(name, "00003-of") && failPart3.Load() {
			http.Error(w, "flaky mirror", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(blobs[name])
	}))
	defer srv.Close()
	spec := shardedFixture("res", srv.URL, 3, false, blobs)
	for name := range blobs {
		hits[name] = &atomic.Int32{}
	}

	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	if _, err := m.Ensure(context.Background(), spec); err == nil {
		t.Fatal("expected part 3 to fail")
	}
	if rows := m.List(); len(rows) != 0 {
		t.Fatalf("failed download left an entry: %+v", rows)
	}
	for _, name := range []string{"res-00001-of-00003.gguf", "res-00002-of-00003.gguf"} {
		if _, err := os.Stat(filepath.Join(dir, "res", name)); err != nil {
			t.Fatalf("finished part gone after a later failure: %v", err)
		}
	}

	failPart3.Store(false)
	if _, err := m.Ensure(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if n := hits["res-00001-of-00003.gguf"].Load(); n != 1 {
		t.Fatalf("part 1 fetched %d times, want 1", n)
	}
	if n := hits["res-00002-of-00003.gguf"].Load(); n != 1 {
		t.Fatalf("part 2 fetched %d times, want 1", n)
	}
	if !m.Has("res") {
		t.Fatal("not ready after resume")
	}

	// After a restart nothing is remembered in memory: finished parts are
	// re-hashed, not re-fetched — a hand-placed wrong file is caught.
	if err := os.WriteFile(filepath.Join(dir, "res", "res-00002-of-00003.gguf"), bytes.Repeat([]byte{'Z'}, 3002), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "models.json")); err != nil {
		t.Fatal(err)
	}
	m2, _ := NewManager(dir, 0, quietLog())
	if _, err := m2.Ensure(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if n := hits["res-00002-of-00003.gguf"].Load(); n != 2 {
		t.Fatalf("tampered part 2 fetched %d times in all, want 2", n)
	}
	if n := hits["res-00001-of-00003.gguf"].Load(); n != 1 {
		t.Fatalf("intact part 1 fetched %d times in all, want 1", n)
	}
	if err := m2.Verify("res"); err != nil {
		t.Fatal(err)
	}
}

func TestMultiPartRejectsCorruptedPart(t *testing.T) {
	blobs := map[string][]byte{}
	srv := artifactServer(t, blobs)
	defer srv.Close()
	spec := shardedFixture("bad", srv.URL, 3, false, blobs)
	// Part 3 is pinned to a hash the served bytes do not have; the
	// composite stays consistent with the (wrong) pin.
	spec.Parts[2].Sha256 = shaOf([]byte("something else"))
	spec.Sha256 = CompositeSHA256([]string{spec.Parts[0].Sha256, spec.Parts[1].Sha256, spec.Parts[2].Sha256})

	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	_, err := m.Ensure(context.Background(), spec)
	if !errors.Is(err, ErrSHAMismatch) {
		t.Fatalf("err = %v, want ErrSHAMismatch", err)
	}
	if rows := m.List(); len(rows) != 0 {
		t.Fatalf("rejected download left an entry: %+v", rows)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad", "bad-00003-of-00003.gguf.partial")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("poisoned partial kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "bad", "bad-00001-of-00003.gguf")); err != nil {
		t.Fatal("a good part was thrown away with the bad one")
	}
}

func TestMultiPartRefusesInconsistentComposite(t *testing.T) {
	blobs := map[string][]byte{}
	srv := artifactServer(t, blobs)
	defer srv.Close()
	spec := shardedFixture("comp", srv.URL, 2, false, blobs)
	spec.Sha256 = strings.Repeat("0", 64)
	m, _ := NewManager(t.TempDir(), 0, quietLog())
	_, err := m.Ensure(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "composite") {
		t.Fatalf("err = %v", err)
	}
	if rows := m.List(); len(rows) != 0 {
		t.Fatalf("entry created: %+v", rows)
	}
}

func TestEnsureRejectsSpecWithoutArtifact(t *testing.T) {
	m, _ := NewManager(t.TempDir(), 0, quietLog())
	_, err := m.Ensure(context.Background(), &typesv1.ModelSpec{Id: "old-daemon-view", Sha256: strings.Repeat("a", 64), SizeBytes: 5})
	if !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("err = %v, want ErrNoArtifact", err)
	}
	if rows := m.List(); len(rows) != 0 {
		t.Fatalf("entry created: %+v", rows)
	}
}

func TestURLBasename(t *testing.T) {
	good := map[string]string{
		"https://hf.co/x/resolve/main/m-00001-of-00003.gguf":           "m-00001-of-00003.gguf",
		"https://hf.co/x/resolve/main/mmproj%20F16.gguf?download=true": "mmproj F16.gguf",
	}
	for in, want := range good {
		if got, err := urlBasename(in); err != nil || got != want {
			t.Errorf("urlBasename(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// url.Parse decodes %2F into a real separator; the last segment is
	// still what gets used, and "b" is a fine name.
	if got, err := urlBasename("https://hf.co/a%2F..%2Fb"); err != nil || got != "b" {
		t.Errorf("urlBasename(encoded slashes) = %q, %v; want b", got, err)
	}
	for _, in := range []string{"https://hf.co/", "https://hf.co/x/..", "https://hf.co/x/.hidden", "https://hf.co/x/ctl%01"} {
		if got, err := urlBasename(in); err == nil {
			t.Errorf("urlBasename(%q) = %q, want error", in, got)
		}
	}
}

func TestCompositeSHA256(t *testing.T) {
	// hex(sha256("aa...bb...")) with the two part hashes concatenated raw:
	// the same definition as models/tools/validate and the registry.
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if got, want := CompositeSHA256([]string{a, b}), shaOf([]byte(a+b)); got != want {
		t.Fatalf("composite = %s, want %s", got, want)
	}
}

func TestReconcileAdoptsCompleteMultiPartSet(t *testing.T) {
	blobs := map[string][]byte{}
	spec := shardedFixture("set", "https://example.invalid", 2, true, blobs)
	cm := CatalogModel{ID: "set", SHA256: spec.Sha256, SizeBytes: spec.SizeBytes,
		Mmproj: &CatalogPart{URL: spec.Mmproj.Url, SHA256: spec.Mmproj.Sha256, SizeBytes: spec.Mmproj.SizeBytes}}
	for _, p := range spec.Parts {
		cm.Parts = append(cm.Parts, CatalogPart{URL: p.Url, SHA256: p.Sha256, SizeBytes: p.SizeBytes})
	}
	cat := &Catalog{Models: []CatalogModel{cm}}

	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	setDir := filepath.Join(dir, "set")
	if err := os.MkdirAll(setDir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, content []byte) {
		if err := os.WriteFile(filepath.Join(setDir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("set-00001-of-00002.gguf", blobs["set-00001-of-00002.gguf"])
	write("set-00002-of-00002.gguf", blobs["set-00002-of-00002.gguf"])
	// Incomplete (no mmproj yet): left alone.
	if got := m.Reconcile(cat); len(got) != 0 {
		t.Fatalf("incomplete set adopted: %v", got)
	}
	// Complete but a shard is wrong: rejected, and remembered.
	write("mmproj-F16.gguf", blobs["mmproj-F16.gguf"])
	write("set-00002-of-00002.gguf", bytes.Repeat([]byte{'Z'}, len(blobs["set-00002-of-00002.gguf"])))
	if got := m.Reconcile(cat); len(got) != 0 {
		t.Fatalf("tampered set adopted: %v", got)
	}
	m.mu.Lock()
	rejected := len(m.reconcileRejected)
	m.mu.Unlock()
	if rejected != 1 {
		t.Fatalf("rejected = %d, want 1", rejected)
	}
	// Fixed: adopted, and the row looks like a downloaded one.
	write("set-00002-of-00002.gguf", blobs["set-00002-of-00002.gguf"])
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(filepath.Join(setDir, "set-00002-of-00002.gguf"), future, future)
	if got := m.Reconcile(cat); len(got) != 1 || got[0] != "set" {
		t.Fatalf("Reconcile = %v", got)
	}
	rows := m.List()
	if len(rows) != 1 || rows[0].State != StateReady || rows[0].PartsTotal != 3 || rows[0].PartsDone != 3 ||
		rows[0].SizeBytes != totalOf(blobs) || rows[0].Path != filepath.Join(setDir, "set-00001-of-00002.gguf") {
		t.Fatalf("adopted row = %+v", rows)
	}
	if err := m.Verify("set"); err != nil {
		t.Fatal(err)
	}
	// A flat <id>.gguf cannot stand in for a sharded catalog entry.
	if err := os.WriteFile(filepath.Join(dir, "flat.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cat.Models = append(cat.Models, CatalogModel{ID: "flat", SHA256: spec.Sha256, SizeBytes: 1, Parts: cm.Parts})
	if got := m.Reconcile(cat); len(got) != 0 {
		t.Fatalf("flat file adopted for a sharded entry: %v", got)
	}
}

func TestRemoveAndEvictMultiPartDeleteTheDirectory(t *testing.T) {
	blobs := map[string][]byte{}
	srv := artifactServer(t, blobs)
	defer srv.Close()
	rmSpec := shardedFixture("rm", srv.URL, 2, true, blobs)

	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	if _, err := m.Ensure(context.Background(), rmSpec); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove("rm"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "rm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("directory survived Remove")
	}
	if len(m.List()) != 0 {
		t.Fatal("entry survived Remove")
	}

	// Under a budget, the set is the LRU victim as a whole.
	mb := 1024 * 1024
	big := map[string][]byte{}
	bigSrv := artifactServer(t, big)
	defer bigSrv.Close()
	bigSpec := shardedFixture("lru", bigSrv.URL, 2, false, big)
	for i, p := range bigSpec.Parts {
		blob := bytes.Repeat([]byte{'p'}, mb)
		big[filepath.Base(p.Url)] = blob
		p.Sha256, p.SizeBytes = shaOf(blob), uint64(len(blob))
		bigSpec.Parts[i] = p
	}
	bigSpec.SizeBytes = uint64(2 * mb)
	bigSpec.Sha256 = CompositeSHA256([]string{bigSpec.Parts[0].Sha256, bigSpec.Parts[1].Sha256})
	single := bytes.Repeat([]byte{'s'}, 2*mb)
	singleSrv := artifactServer(t, map[string][]byte{"single": single})
	defer singleSrv.Close()

	m2, _ := NewManager(t.TempDir(), 3, quietLog())
	if _, err := m2.Ensure(context.Background(), bigSpec); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := m2.Ensure(context.Background(), spec("single", single, singleSrv.URL)); err != nil {
		t.Fatal(err)
	}
	if m2.Has("lru") {
		t.Fatal("sharded LRU entry not evicted")
	}
	if _, err := os.Stat(filepath.Join(m2.Dir, "lru")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("evicted set's directory still there")
	}
}

func TestPartialsInsideASetAreCountedAndCollected(t *testing.T) {
	dir := t.TempDir()
	m, _ := NewManager(dir, 0, quietLog())
	sub := filepath.Join(dir, "abandoned")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(sub, "abandoned-00002-of-00003.gguf.partial")
	if err := os.WriteFile(partial, []byte("half a shard"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := m.Stats(); st.PartialBytes != 12 {
		t.Fatalf("PartialBytes = %d, want 12", st.PartialBytes)
	}
	if got := m.GCPartials(PartialMaxAge); len(got) != 0 {
		t.Fatalf("fresh partial collected: %v", got)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(partial, old, old)
	if got := m.GCPartials(PartialMaxAge); len(got) != 1 || got[0] != "abandoned" {
		t.Fatalf("GCPartials = %v", got)
	}
	if _, err := os.Stat(partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale partial kept")
	}
}

func TestLocalShardedArtifactServedInPlace(t *testing.T) {
	blobs := map[string][]byte{}
	spec := shardedFixture("local", "", 2, true, blobs)
	src := t.TempDir()
	for name, blob := range blobs {
		if err := os.WriteFile(filepath.Join(src, name), blob, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range spec.Parts {
		p.Url = LocalArtifactURL(filepath.Join(src, filepath.Base(p.Url)))
	}
	spec.Mmproj.Url = LocalArtifactURL(filepath.Join(src, "mmproj-F16.gguf"))

	m, _ := NewManager(t.TempDir(), 0, quietLog())
	a, err := m.EnsureArtifact(context.Background(), spec, OriginOperator)
	if err != nil {
		t.Fatal(err)
	}
	if a.Path != filepath.Join(src, "local-00001-of-00002.gguf") || a.MmprojPath != filepath.Join(src, "mmproj-F16.gguf") || a.SizeBytes != totalOf(blobs) {
		t.Fatalf("Artifact = %+v", a)
	}
	if len(m.List()) != 0 {
		t.Fatal("a local set must not be indexed (the LRU could delete it)")
	}
	// A wrong shard is caught even though it is the operator's own file.
	spec.Parts[1].Sha256 = shaOf([]byte("nope"))
	if _, err := m.EnsureArtifact(context.Background(), spec, OriginOperator); !errors.Is(err, ErrSHAMismatch) {
		t.Fatalf("err = %v, want ErrSHAMismatch", err)
	}
	// Mixing a local part 1 with a remote part 2 is refused, not fetched.
	spec.Parts[1].Sha256 = shaOf(blobs["local-00002-of-00002.gguf"])
	spec.Parts[1].Url = "https://example.invalid/local-00002-of-00002.gguf"
	if _, err := m.EnsureArtifact(context.Background(), spec, OriginOperator); err == nil || !strings.Contains(err.Error(), "not a local file") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseCatalogCarriesPartsIntoSpec(t *testing.T) {
	raw := `{"models":[{"id":"sharded-q4","sha256":"` + strings.Repeat("c", 64) + `","artifact_url":"","size_bytes":3,
	  "parts":[{"url":"https://x/s-00001-of-00002.gguf","sha256":"` + strings.Repeat("a", 64) + `","size_bytes":1},
	           {"url":"https://x/s-00002-of-00002.gguf","sha256":"` + strings.Repeat("b", 64) + `","size_bytes":2}],
	  "mmproj":{"url":"https://x/mmproj-F16.gguf","sha256":"` + strings.Repeat("d", 64) + `","size_bytes":7}},
	  {"id":"plain","sha256":"` + strings.Repeat("e", 64) + `","artifact_url":"https://x/plain.gguf","size_bytes":9}]}`
	cat, err := ParseCatalog([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	spec := cat.Models[0].Spec()
	if len(spec.GetParts()) != 2 || spec.GetParts()[1].GetSizeBytes() != 2 || spec.GetParts()[0].GetUrl() != "https://x/s-00001-of-00002.gguf" {
		t.Fatalf("parts = %v", spec.GetParts())
	}
	if spec.GetMmproj() == nil || spec.GetMmproj().GetSizeBytes() != 7 {
		t.Fatalf("mmproj = %v", spec.GetMmproj())
	}
	plain := cat.Models[1].Spec()
	if len(plain.GetParts()) != 0 || plain.GetMmproj() != nil {
		t.Fatalf("plain entry grew parts: %v", plain)
	}
	yaml := "models:\n  - id: y\n    sha256: " + strings.Repeat("f", 64) + "\n    parts:\n      - url: https://x/y-00001-of-00001.gguf\n        sha256: " + strings.Repeat("1", 64) + "\n        size_bytes: 4\n"
	cat, err = ParseCatalog([]byte(yaml))
	if err != nil || len(cat.Models[0].Parts) != 1 || cat.Models[0].Parts[0].SizeBytes != 4 {
		t.Fatalf("yaml parts: %+v %v", cat.Models, err)
	}
}

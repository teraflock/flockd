package models

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/activity"
)

func readyManager(t *testing.T, ids ...string) (*Manager, map[string][]byte) {
	t.Helper()
	blobs := map[string][]byte{}
	for _, id := range ids {
		blobs[id] = []byte("gguf-" + id)
	}
	srv := artifactServer(t, blobs)
	t.Cleanup(srv.Close)
	m, err := NewManager(t.TempDir(), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	m.Activity = activity.New(10)
	for _, id := range ids {
		if _, err := m.Ensure(context.Background(), spec(id, blobs[id], srv.URL)); err != nil {
			t.Fatal(err)
		}
	}
	return m, blobs
}

func kinds(r *activity.Ring, kind string) int {
	n := 0
	for _, e := range r.List() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestListMarksMissingFilesAndDropsThemFromBudget(t *testing.T) {
	m, _ := readyManager(t, "gone", "here")
	rows := m.List()
	for _, r := range rows {
		if r.State != StateReady || r.Path != m.Path(r.ID) {
			t.Fatalf("row %+v", r)
		}
	}
	if st := m.Stats(); st.ModelsBytes != int64(len("gguf-gone")+len("gguf-here")) || st.Dir != m.Dir || st.FreeBytes <= 0 {
		t.Fatalf("stats = %+v", st)
	}
	if err := os.Remove(m.Path("gone")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // twice: the activity row is emitted once
		for _, r := range m.List() {
			switch r.ID {
			case "gone":
				if r.State != StateMissing || r.Path != "" {
					t.Fatalf("deleted file row = %+v", r)
				}
			case "here":
				if r.State != StateReady {
					t.Fatalf("intact row = %+v", r)
				}
			}
		}
	}
	if n := kinds(m.Activity, activity.KindMissing); n != 1 {
		t.Fatalf("missing activity rows = %d, want 1", n)
	}
	if m.Has("gone") || !m.Has("here") {
		t.Fatal("Has() disagrees with disk")
	}
	if st := m.Stats(); st.ModelsBytes != int64(len("gguf-here")) {
		t.Fatalf("missing file counted: %+v", st)
	}
	// Budget: the missing entry occupies nothing, so a 1-byte budget with
	// "here" (9 bytes) is over only by "here" — the missing one is never
	// evicted-by-size, and no error mentions it.
	m.SetMaxDiskMB(1)
	if err := m.evictForLocked(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	if !m.Has("here") {
		t.Fatal("evicted within budget")
	}
	// The download activity rows for the two fetches are there too.
	if n := kinds(m.Activity, activity.KindDownloaded); n != 2 {
		t.Fatalf("downloaded rows = %d", n)
	}
}

func TestGCPartialsRemovesOnlyStaleOrphans(t *testing.T) {
	m, _ := readyManager(t)
	stale := filepath.Join(m.Dir, "old.gguf.partial")
	fresh := filepath.Join(m.Dir, "new.gguf.partial")
	live := filepath.Join(m.Dir, "live.gguf.partial")
	for _, p := range []string{stale, fresh, live} {
		if err := os.WriteFile(p, []byte("0123456789"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, p := range []string{stale, live} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	m.setProgress("live", 10, 100) // in flight: never collected
	if st := m.Stats(); st.PartialBytes != 30 {
		t.Fatalf("partial_bytes = %d, want 30", st.PartialBytes)
	}
	removed := m.GCPartials(PartialMaxAge)
	if len(removed) != 1 || removed[0] != "old" {
		t.Fatalf("removed = %v", removed)
	}
	for p, want := range map[string]bool{stale: false, fresh: true, live: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Fatalf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
	if st := m.Stats(); st.PartialBytes != 20 {
		t.Fatalf("partial_bytes after gc = %d", st.PartialBytes)
	}
}

func TestRetentionEvictsOldUnpinnedUnloaded(t *testing.T) {
	m, _ := readyManager(t, "old", "pinned", "loaded", "recent")
	old := time.Now().Add(-10 * 24 * time.Hour)
	m.mu.Lock()
	for _, id := range []string{"old", "pinned", "loaded"} {
		m.state.Entries[id].LastUsed = old
	}
	m.mu.Unlock()
	if err := m.Pin("pinned", true); err != nil {
		t.Fatal(err)
	}
	m.IsLoaded = func(id string) bool { return id == "loaded" }

	if got := m.Retain(); len(got) != 0 {
		t.Fatalf("retention off evicted %v", got)
	}
	m.SetRetentionDays(7)
	if got := m.Retain(); len(got) != 1 || got[0] != "old" {
		t.Fatalf("Retain = %v", got)
	}
	if m.Has("old") || !m.Has("pinned") || !m.Has("loaded") || !m.Has("recent") {
		t.Fatal("retention touched the wrong models")
	}
	if n := kinds(m.Activity, activity.KindEvicted); n != 1 {
		t.Fatalf("evicted rows = %d", n)
	}
	// Persisted: a reload agrees.
	m2, err := NewManager(m.Dir, 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if m2.Has("old") || len(m2.List()) != 3 {
		t.Fatalf("reload: %+v", m2.List())
	}
}

func TestReconcileAdoptsKnownCatalogFiles(t *testing.T) {
	m, _ := readyManager(t, "indexed")
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(m.Dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("adopt-me.gguf", "twelve bytes")
	write("wrong-size.gguf", "x")
	write("not-in-catalog.gguf", "yy")
	cat := &Catalog{Models: []CatalogModel{
		{ID: "adopt-me", SHA256: "abc", SizeBytes: 12},
		{ID: "wrong-size", SHA256: "def", SizeBytes: 999},
		{ID: "indexed", SHA256: shaOf([]byte("gguf-indexed"))},
	}}
	if got := m.Reconcile(cat); len(got) != 1 || got[0] != "adopt-me" {
		t.Fatalf("Reconcile = %v", got)
	}
	if got := m.Reconcile(cat); len(got) != 0 {
		t.Fatalf("second Reconcile adopted again: %v", got)
	}
	rows := map[string]Info{}
	for _, r := range m.List() {
		rows[r.ID] = r
	}
	if r, ok := rows["adopt-me"]; !ok || r.State != StateReady || r.Origin != OriginOperator || r.SizeBytes != 12 || r.Path == "" {
		t.Fatalf("adopted row = %+v", r)
	}
	if _, ok := rows["wrong-size"]; ok {
		t.Fatal("size mismatch adopted")
	}
	if _, ok := rows["not-in-catalog"]; ok {
		t.Fatal("unknown file adopted")
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
}

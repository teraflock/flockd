package modelops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// memHarness serves a catalog whose entries carry min_ram_mb so the
// footprint estimate is controlled by the test, not by file sizes.
func memHarness(t *testing.T, minRAM map[string]int64, tps float64) (*Service, *engine.Engine) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("gguf " + filepath.Base(r.URL.Path)))
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ids := make([]string, 0, len(minRAM))
	for id := range minRAM {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	cat := "models:\n"
	for _, id := range ids {
		blob := []byte("gguf " + id)
		cat += fmt.Sprintf("  - id: %s\n    sha256: %s\n    artifact_url: %s/%s\n    size_bytes: %d\n    min_ram_mb: %d\n",
			id, shaOf(blob), srv.URL, id, len(blob), minRAM[id])
	}
	catPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catPath, []byte(cat), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.New(nil, nil, nil)
	return &Service{
		Mgr: mgr, Eng: eng, Loader: rt.NewMockRuntime(tps),
		Budget: rt.ResourceBudget{MaxConcurrent: 2}, Log: quietLog(), ManifestPath: catPath,
	}, eng
}

func loadedIDs(eng *engine.Engine) []string {
	var out []string
	for _, m := range eng.Models() {
		out = append(out, m.Spec.ID)
	}
	sort.Strings(out)
	return out
}

func TestAdmissionEvictsIdleMeshFirstNeverDefault(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{
		"def": 1000, "op": 2000, "mesh": 2000, "n": 2000, "p": 3000, "q": 5000,
	}, 0)
	svc.SetMemoryBudgetMB(5000)
	ctx := context.Background()

	if err := svc.Load(ctx, "def"); err != nil { // first loaded = default
		t.Fatal(err)
	}
	if err := svc.Load(ctx, "op"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond) // op is older than mesh in LRU terms
	if _, err := svc.LoadInstanceOrigin(ctx, "mesh", models.OriginMesh); err != nil {
		t.Fatal(err)
	}
	if m := svc.Memory(); m.UsedMB != 5000 || m.BudgetMB != 5000 || m.Models["mesh"] != 2000 {
		t.Fatalf("memory = %+v", m)
	}

	// n needs 2000: the mesh-placed model goes first even though the
	// operator's is least recently used.
	if err := svc.Load(ctx, "n"); err != nil {
		t.Fatal(err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[def n op]" {
		t.Fatalf("after n: loaded = %v", got)
	}
	if !svc.Mgr.Has("mesh") {
		t.Fatal("admission unload deleted the artifact; it must stay cached")
	}

	// p needs 3000: two operator models go (LRU order), the default never.
	if err := svc.Load(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[def p]" {
		t.Fatalf("after p: loaded = %v", got)
	}

	// q needs 5000: even with p gone, def (1000) + q exceeds the budget.
	err := svc.Load(ctx, "q")
	if !errors.Is(err, ErrOverMemory) {
		t.Fatalf("err = %v, want ErrOverMemory", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[def]" {
		t.Fatalf("after q: loaded = %v (default must survive)", got)
	}
	if !svc.Mgr.Has("q") {
		t.Fatal("over-memory load must leave the artifact on disk (cached)")
	}
	if eng.DefaultModel() != "def" {
		t.Fatalf("default = %q", eng.DefaultModel())
	}
}

func TestAdmissionNeverUnloadsInflight(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"def": 1000, "busy": 2000, "n": 2000}, 5)
	svc.SetMemoryBudgetMB(3000)
	ctx := context.Background()
	if err := svc.Load(ctx, "def"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx, "busy"); err != nil {
		t.Fatal(err)
	}
	stream, err := eng.Complete(ctx, rt.CompletionRequest{ID: "r", Model: "busy", Kind: rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "x"}}, Params: rt.GenerationParams{Seed: 1, MaxTokens: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Load(ctx, "n"); !errors.Is(err, ErrOverMemory) {
		t.Fatalf("load with the only candidate in flight: %v, want ErrOverMemory", err)
	}
	if _, ok := svc.IdleSince("busy"); ok {
		t.Fatal("busy model reported idle")
	}
	_ = stream.Close()
	if err := svc.Load(ctx, "n"); err != nil {
		t.Fatalf("after the request ended: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[def n]" {
		t.Fatalf("loaded = %v", got)
	}
}

func TestNoBudgetMeansNoAdmission(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"a": 100000, "b": 100000}, 0)
	for _, id := range []string{"a", "b"} {
		if err := svc.Load(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if len(eng.Models()) != 2 {
		t.Fatal("unbounded budget should load everything")
	}
}

func TestIdleUnloadSparesDefaultAndReportsCached(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"def": 100, "idle": 100}, 0)
	var unloaded []string
	svc.OnUnloaded = func(id string) { unloaded = append(unloaded, id) }
	ctx := context.Background()
	if err := svc.Load(ctx, "def"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoadInstanceOrigin(ctx, "idle", models.OriginMesh); err != nil {
		t.Fatal(err)
	}
	if got := svc.UnloadIdle(ctx); len(got) != 0 {
		t.Fatalf("idle unload off (0) unloaded %v", got)
	}
	svc.SetIdleUnload(30 * time.Millisecond)
	if got := svc.UnloadIdle(ctx); len(got) != 0 {
		t.Fatalf("freshly loaded model unloaded: %v", got)
	}
	since, ok := svc.IdleSince("idle")
	if !ok || since.IsZero() {
		t.Fatal("idle_since missing for an idle model")
	}
	time.Sleep(60 * time.Millisecond)
	if got := svc.UnloadIdle(ctx); fmt.Sprint(got) != "[idle]" {
		t.Fatalf("UnloadIdle = %v", got)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[def]" {
		t.Fatalf("loaded = %v (default is exempt)", got)
	}
	if fmt.Sprint(unloaded) != "[idle]" {
		t.Fatalf("OnUnloaded = %v", unloaded)
	}
	if !svc.Mgr.Has("idle") {
		t.Fatal("idle unload removed the artifact")
	}
	// A request keeps the default fresh; the loop is what the daemon runs.
	old := housekeepInterval
	housekeepInterval = 10 * time.Millisecond
	t.Cleanup(func() { housekeepInterval = old })
	if err := svc.Load(ctx, "idle"); err != nil {
		t.Fatal(err)
	}
	lctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go svc.RunHousekeeping(lctx)
	deadline := time.Now().Add(2 * time.Second)
	for len(eng.Models()) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[def]" {
		t.Fatalf("housekeeping loop: loaded = %v", got)
	}
}

func TestMeasureReplacesEstimate(t *testing.T) {
	svc, _ := memHarness(t, map[string]int64{"a": 4000}, 0)
	if err := svc.Load(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if m := svc.Memory(); m.Models["a"] != 4000 {
		t.Fatalf("before measure: %+v", m)
	}
	svc.Measure(context.Background()) // mock runtime reports 512 MB
	if m := svc.Memory(); m.Models["a"] != 512 || m.UsedMB != 512 {
		t.Fatalf("after measure: %+v", m)
	}
}

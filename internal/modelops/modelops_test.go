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
	"strings"
	"sync"
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

// The load receives the planned slots and total context (flockd#46), not
// the raw ceiling: with no budget known the plan is the ceiling at the
// cap, so --parallel 2 and --ctx-size 2 × 8192 for an unknown window.
func TestLoadPassesThePlannedLayout(t *testing.T) {
	svc, _ := harness(t, "planned-model", []byte("gguf bytes"))
	loader := &captureLoader{mock: rt.NewMockRuntime(0)}
	svc.Loader = loader
	if err := svc.Load(context.Background(), "planned-model"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if loader.lastBudget.Slots != 2 || loader.lastBudget.ContextTokens != 2*8192 {
		t.Fatalf("planned layout = %d slots × %d total, want 2 × 16384", loader.lastBudget.Slots, loader.lastBudget.ContextTokens)
	}
	if loader.lastBudget.MaxConcurrent != 2 {
		t.Fatalf("ceiling not carried through: %+v", loader.lastBudget)
	}
	// The operator's pin narrows every slot and is charged accordingly.
	svc2, _ := harness(t, "pinned-model", []byte("gguf bytes"))
	loader2 := &captureLoader{mock: rt.NewMockRuntime(0)}
	svc2.Loader, svc2.ContextLength = loader2, 4096
	if err := svc2.Load(context.Background(), "pinned-model"); err != nil {
		t.Fatalf("load: %v", err)
	}
	if loader2.lastBudget.Slots != 2 || loader2.lastBudget.ContextTokens != 2*4096 {
		t.Fatalf("pinned layout = %d × %d, want 2 × 8192", loader2.lastBudget.Slots, loader2.lastBudget.ContextTokens)
	}
}

// tooOldLoader refuses decision models the way the llama.cpp adapter does
// on a build that predates /v1/systemone.
type tooOldLoader struct{ *rt.MockRuntime }

func (tooOldLoader) SupportsModel(_ context.Context, m rt.ModelSpec) error {
	if m.Decision {
		return fmt.Errorf("%w: %s needs llama.cpp b11382 or newer", rt.ErrRuntimeTooOld, m.ID)
	}
	return nil
}

func TestLoadRefusesDecisionModelOnOldRuntime(t *testing.T) {
	svc, eng := harness(t, "laya", []byte("gguf"))
	raw, err := os.ReadFile(svc.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svc.ManifestPath, append(raw, []byte("    decision: true\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.Loader = tooOldLoader{rt.NewMockRuntime(0)}
	err = svc.Load(context.Background(), "laya")
	if !errors.Is(err, rt.ErrRuntimeTooOld) {
		t.Fatalf("load err = %v, want ErrRuntimeTooOld", err)
	}
	if svc.Mgr.Has("laya") || len(eng.Models()) != 0 {
		t.Fatal("refused decision model was downloaded or loaded")
	}

	// On a runtime that supports it, the spec handed to the loader says
	// decision.
	svc.Loader = rt.NewMockRuntime(0)
	if err := svc.Load(context.Background(), "laya"); err != nil {
		t.Fatal(err)
	}
	if m := eng.Models(); len(m) != 1 || !m[0].Spec.Decision || m[0].Spec.Embeddings {
		t.Fatalf("loaded = %+v", m)
	}
}

// catalogServer serves a mutable catalog with an ETag and counts requests.
type catalogServer struct {
	mu         sync.Mutex
	body       string
	version    int
	fail       bool
	full, cond int
}

func (c *catalogServer) set(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = body
	c.version++
}

func (c *catalogServer) counts() (full, cond int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.full, c.cond
}

func (c *catalogServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		http.Error(w, "boom", http.StatusInternalServerError)
		return
	}
	etag := fmt.Sprintf(`"v%d"`, c.version)
	if r.Header.Get("If-None-Match") == etag {
		c.cond++
		w.WriteHeader(http.StatusNotModified)
		return
	}
	c.full++
	w.Header().Set("ETag", etag)
	_, _ = w.Write([]byte(c.body))
}

func catalogJSON(ids ...string) string {
	entries := make([]string, len(ids))
	for i, id := range ids {
		entries[i] = fmt.Sprintf(`{"id":%q,"sha256":%q,"artifact_url":"http://127.0.0.1:1/%s","size_bytes":4}`, id, shaOf([]byte(id)), id)
	}
	return `{"models":[` + strings.Join(entries, ",") + `]}`
}

func urlCatalogService(t *testing.T) (*Service, *catalogServer) {
	t.Helper()
	cs := &catalogServer{}
	cs.set(catalogJSON("old-model"))
	srv := httptest.NewServer(cs)
	t.Cleanup(srv.Close)
	mgr, err := models.NewManager(filepath.Join(t.TempDir(), "models"), 0, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return &Service{Mgr: mgr, Eng: engine.New(nil, nil, nil), Loader: rt.NewMockRuntime(0), Log: quietLog(), ManifestURL: srv.URL}, cs
}

// A model promoted into the catalog after the daemon fetched it is found
// without a restart: a lookup miss refetches once, rate-limited.
func TestCatalogMissTriggersOneRefetch(t *testing.T) {
	old := catalogMissRefetch
	t.Cleanup(func() { catalogMissRefetch = old })
	catalogMissRefetch = time.Hour

	svc, cs := urlCatalogService(t)
	ctx := context.Background()
	if _, ok, err := svc.Lookup(ctx, "old-model"); err != nil || !ok {
		t.Fatalf("old-model: %v %v", ok, err)
	}
	// The catalog is promoted. A hit never refetches; within the rate
	// limit a miss does not either.
	cs.set(catalogJSON("old-model", "laya-q8_0"))
	if _, ok, _ := svc.Lookup(ctx, "laya-q8_0"); ok {
		t.Fatal("found a model promoted after the fetch without refetching")
	}
	if full, cond := cs.counts(); full != 1 || cond != 0 {
		t.Fatalf("requests = %d full, %d conditional; the rate limit should have held", full, cond)
	}

	// Past the rate limit: one refetch, and every caller sees the model.
	catalogMissRefetch = 0
	if _, ok, err := svc.Lookup(ctx, "laya-q8_0"); err != nil || !ok {
		t.Fatalf("promoted model not found after a miss refetch: %v %v", ok, err)
	}
	if _, err := svc.StartDownload(ctx, "nope"); !errors.Is(err, ErrUnknownModel) {
		t.Fatalf("unknown id: %v", err)
	}
	catalogMissRefetch = time.Hour
	before, _ := cs.counts()
	for range 5 {
		if _, ok, _ := svc.Lookup(ctx, "still-unknown"); ok {
			t.Fatal("found an unknown model")
		}
	}
	if full, cond := cs.counts(); full != before || cond > 1 {
		t.Fatalf("unknown ids hammered the catalog host: %d full, %d conditional", full-before, cond)
	}
	// The pull path goes through the same lookup: the promoted model's
	// download starts instead of "not in catalog".
	started, err := svc.StartDownload(ctx, "laya-q8_0")
	if err != nil || !started {
		t.Fatalf("pull of the promoted model: started=%v err=%v", started, err)
	}
	svc.CancelDownload("laya-q8_0")
	// The download goroutine writes under the test's temp dir: let it end
	// before the directory is removed.
	deadline := time.Now().Add(5 * time.Second)
	for svc.Downloading("laya-q8_0") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

func TestCatalogPeriodicRefreshAndStaleOnFailure(t *testing.T) {
	old := catalogMissRefetch
	t.Cleanup(func() { catalogMissRefetch = old })
	catalogMissRefetch = time.Hour

	svc, cs := urlCatalogService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := svc.Catalog(ctx, false); err != nil {
		t.Fatal(err)
	}
	go svc.RunCatalogRefresh(ctx, 10*time.Millisecond)

	// Unchanged catalog: the refresh is conditional (304s, no new body).
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, cond := cs.counts(); cond >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no conditional refreshes")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if full, _ := cs.counts(); full != 1 {
		t.Fatalf("an unchanged catalog was downloaded %d times", full)
	}

	// Promoted: the loop picks it up with nobody asking for a refresh.
	cs.set(catalogJSON("old-model", "julia-1-q8_0"))
	for {
		cat, _ := svc.Catalog(ctx, false)
		if _, ok := cat.Find("julia-1-q8_0"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic refresh never picked up the promoted catalog")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The host fails (or serves garbage): the last good catalog stays.
	cs.mu.Lock()
	cs.fail = true
	cs.mu.Unlock()
	time.Sleep(40 * time.Millisecond)
	cat, err := svc.Catalog(ctx, true)
	if err != nil {
		t.Fatalf("refresh failure lost the catalog: %v", err)
	}
	if _, ok := cat.Find("julia-1-q8_0"); !ok || len(cat.Models) != 2 {
		t.Fatalf("stale catalog = %+v", cat.Models)
	}
	cs.mu.Lock()
	cs.fail, cs.body = false, `{"models":[{"id":"broken"}]}` // no sha256: rejected
	cs.version++
	cs.mu.Unlock()
	if cat, err := svc.Catalog(ctx, true); err != nil || len(cat.Models) != 2 {
		t.Fatalf("an invalid catalog replaced the good one: %v %v", cat, err)
	}
}

// Changing the plan limits on a running daemon (PUT /api/v1/limits:
// max_concurrent, max_context) reloads the loaded models that are idle
// with the new layout, never one with a request in flight, and without
// telling the coordinator the model went away.
func TestSetPlanLimitsReloadsIdleModels(t *testing.T) {
	blobs := map[string][]byte{"idle-model": []byte("gguf idle"), "busy-model": []byte("gguf busy")}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(blobs[filepath.Base(r.URL.Path)])
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cat := "models:\n"
	for id, b := range blobs {
		cat += fmt.Sprintf("  - id: %s\n    sha256: %s\n    artifact_url: %s/%s\n    size_bytes: %d\n    context_length: 32768\n", id, shaOf(b), srv.URL, id, len(b))
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
	loader := &captureLoader{mock: rt.NewMockRuntime(5)}
	var cached []string
	// Slots pinned (floor = ceiling): this test is about a settings
	// change, not about demand scaling (scale_test.go).
	svc := &Service{Mgr: mgr, Eng: eng, Loader: loader, Budget: rt.ResourceBudget{MaxConcurrent: 16}, MinConcurrent: 16,
		MaxContext: 16384, MinContext: 8192, Log: quietLog(), ManifestPath: catPath,
		OnUnloaded: func(id string) { cached = append(cached, id) }}
	ctx := context.Background()
	for _, id := range []string{"busy-model", "idle-model"} {
		if err := svc.Load(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.SetDefault("idle-model"); err != nil {
		t.Fatal(err)
	}
	if l := svc.Layouts(); l["idle-model"] != (Layout{Slots: 16, CtxPerSlot: 16384}) || l["busy-model"].Stale {
		t.Fatalf("layouts at load = %+v", l)
	}
	if got := svc.PlanLimits(); got != (PlanLimits{MaxConcurrent: 16, MinConcurrent: 16, MaxContext: 16384, MinContext: 8192}) || svc.SlotCeiling() != 16 {
		t.Fatalf("initial plan limits = %+v", got)
	}

	// One model is mid-request.
	ts, err := eng.Complete(ctx, rt.CompletionRequest{Model: "busy-model", Kind: rt.KindChat, Params: rt.GenerationParams{Seed: 1, MaxTokens: 400}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Recv(); err != nil {
		t.Fatal(err)
	}

	svc.SetPlanLimits(PlanLimits{MaxConcurrent: 4, MinConcurrent: 4, MaxContext: 8192, MinContext: 8192})
	if l := svc.Layouts(); !l["idle-model"].Stale || !l["busy-model"].Stale {
		t.Fatalf("layouts after the change = %+v", l)
	}
	reloaded, busy := svc.ReloadStale(ctx)
	if fmt.Sprint(reloaded) != "[idle-model]" || fmt.Sprint(busy) != "[busy-model]" {
		t.Fatalf("reloaded=%v busy=%v", reloaded, busy)
	}
	l := svc.Layouts()
	if l["idle-model"] != (Layout{Slots: 4, CtxPerSlot: 8192}) {
		t.Fatalf("idle model layout = %+v, want 4 x 8192", l["idle-model"])
	}
	if l["busy-model"] != (Layout{Slots: 16, CtxPerSlot: 16384, Stale: true}) {
		t.Fatalf("busy model layout = %+v, want its old one, stale", l["busy-model"])
	}
	if loader.lastBudget.Slots != 4 || loader.lastBudget.ContextTokens != 4*8192 || loader.lastBudget.MaxConcurrent != 4 {
		t.Fatalf("reload budget = %+v", loader.lastBudget)
	}
	if eng.DefaultModel() != "idle-model" {
		t.Fatalf("default after reload = %q", eng.DefaultModel())
	}
	if len(cached) != 0 {
		t.Fatalf("a reload reported the model as unloaded/cached: %v", cached)
	}
	// The in-flight request was not disturbed.
	if _, err := ts.Recv(); err != nil {
		t.Fatalf("in-flight request broken by the reload: %v", err)
	}
	_ = ts.Close()

	// Once idle, the next pass picks it up; a pass with nothing stale does nothing.
	reloaded, busy = svc.ReloadStale(ctx)
	if fmt.Sprint(reloaded) != "[busy-model]" || len(busy) != 0 {
		t.Fatalf("second pass: reloaded=%v busy=%v", reloaded, busy)
	}
	if reloaded, busy = svc.ReloadStale(ctx); len(reloaded) != 0 || len(busy) != 0 {
		t.Fatalf("third pass: reloaded=%v busy=%v", reloaded, busy)
	}
	// 0 = auto resolves by hardware (no GPU known here: the CPU default).
	svc.SetPlanLimits(PlanLimits{MaxContext: 8192})
	if svc.SlotCeiling() != 2 || svc.SlotFloor() != 2 {
		t.Fatalf("auto ceiling = %d, floor = %d", svc.SlotCeiling(), svc.SlotFloor())
	}
	// The floor never exceeds the ceiling; auto is 2.
	svc.SetPlanLimits(PlanLimits{MaxConcurrent: 16})
	if svc.SlotFloor() != DefaultFloorSlots {
		t.Fatalf("auto floor = %d", svc.SlotFloor())
	}
	svc.SetPlanLimits(PlanLimits{MaxConcurrent: 4, MinConcurrent: 9})
	if svc.SlotFloor() != 4 {
		t.Fatalf("floor above the ceiling = %d, want 4 (pinned)", svc.SlotFloor())
	}
}

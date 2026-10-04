package modelops

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

func TestScaleTargets(t *testing.T) {
	for _, c := range []struct{ cur, peak, ceiling, want int }{
		{2, 2, 16, 4},   // saturated, nobody waiting: double
		{2, 3, 16, 4},   // one waiting
		{2, 7, 16, 8},   // a burst: straight to what holds it
		{2, 40, 16, 16}, // capped by the ceiling
		{8, 9, 16, 16},
		{16, 30, 16, 16}, // already there
		{2, 5, 4, 4},     // the operator's ceiling of 4
		{4, 4, 4, 4},
	} {
		if got := growTarget(c.cur, c.peak, c.ceiling); got != c.want {
			t.Errorf("growTarget(cur=%d, peak=%d, ceiling=%d) = %d, want %d", c.cur, c.peak, c.ceiling, got, c.want)
		}
	}
	for _, c := range []struct{ cur, peak, floor, want int }{
		{16, 0, 2, 2}, // idle: back to the floor
		{16, 1, 2, 2},
		{16, 3, 2, 4},
		{16, 8, 2, 8},
		{16, 9, 2, 16}, // 9 needs 16: stay
		{8, 5, 2, 8},   // hysteresis: 5 of 8 stays at 8
		{8, 4, 2, 4},
		{4, 1, 2, 2},
		{4, 1, 4, 4}, // floor
		{2, 0, 2, 2},
	} {
		if got := shrinkTarget(c.cur, c.peak, c.floor); got != c.want {
			t.Errorf("shrinkTarget(cur=%d, peak=%d, floor=%d) = %d, want %d", c.cur, c.peak, c.floor, got, c.want)
		}
	}
}

// countingLoader counts runtime starts and remembers each layout.
type countingLoader struct {
	mock *rt.MockRuntime
	mu   sync.Mutex
	res  []rt.ResourceBudget
	fail bool
}

func (l *countingLoader) Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error) {
	l.mu.Lock()
	l.res = append(l.res, res)
	fail := l.fail
	l.mu.Unlock()
	if fail {
		return nil, errors.New("runtime would not start")
	}
	return l.mock.Load(ctx, m, res)
}

func (l *countingLoader) starts() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.res)
}

// scaleHarness: a 2 GB-ish chat model (64 KB of KV per token, window
// 32768) on a node with a 32 GB budget, ceiling 16, floor auto (2).
func scaleHarness(t *testing.T, tokensPerSec float64, ids ...string) (*Service, *engine.Engine, *countingLoader) {
	t.Helper()
	blob := []byte("gguf")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(blob) }))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	cat := "models:\n"
	for _, id := range ids {
		cat += fmt.Sprintf("  - id: %s\n    sha256: %s\n    artifact_url: %s/%s\n    size_bytes: %d\n    context_length: 32768\n    min_ram_mb: 2048\n",
			id, shaOf(blob), srv.URL, id, len(blob))
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
	loader := &countingLoader{mock: rt.NewMockRuntime(tokensPerSec)}
	svc := &Service{Mgr: mgr, Eng: eng, Loader: loader, Budget: rt.ResourceBudget{MaxConcurrent: 16},
		MaxContext: 16384, MinContext: 8192, Log: quietLog(), ManifestPath: catPath,
		Hardware: &typesv1.CapabilityProfile{RamTotalMb: 65536, Gpus: []*typesv1.GpuInfo{{UnifiedMemory: true}}}}
	svc.SetMemoryBudgetMB(32768)
	return svc, eng, loader
}

// kvPlan makes a loaded model's plan input cost real memory per token, as
// a GGUF header would (the test artifact is 4 bytes).
func kvPlan(svc *Service, id string, fileBytes, kvBytesPerToken int64) {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	svc.loads[id].Plan.FileBytes = fileBytes
	svc.loads[id].Plan.KVBytesPerToken = kvBytesPerToken
}

func hold(t *testing.T, eng *engine.Engine, model string, n int) []rt.TokenStream {
	t.Helper()
	out := make([]rt.TokenStream, 0, n)
	for range n {
		ts, err := eng.Complete(context.Background(), rt.CompletionRequest{Model: model, Kind: rt.KindChat,
			Messages: []rt.Message{{Role: "user", Content: "hi"}}, Params: rt.GenerationParams{Seed: 1, MaxTokens: 100000}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.Recv(); err != nil {
			t.Fatal(err)
		}
		out = append(out, ts)
	}
	return out
}

func slotsOf(svc *Service, id string) int { return svc.Layouts()[id].Slots }

func TestSlotsFollowDemand(t *testing.T) {
	old1, old2, old3 := growAfter, shrinkAfter, resizeCooldown
	t.Cleanup(func() { growAfter, shrinkAfter, resizeCooldown = old1, old2, old3 })
	growAfter, shrinkAfter, resizeCooldown = 3*time.Second, 5*time.Minute, time.Minute

	svc, eng, loader := scaleHarness(t, 50, "chat")
	ctx := context.Background()
	if err := svc.Load(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	// A chat model starts at the floor, not the ceiling.
	if l := svc.Layouts()["chat"]; l.Slots != 2 || l.CtxPerSlot != 16384 {
		t.Fatalf("layout at load = %+v, want 2 x 16384", l)
	}
	t0 := time.Now().Add(time.Hour) // past the load's cooldown
	clock := t0

	// One request at a time: nothing happens, however long.
	one := hold(t, eng, "chat", 1)
	for range 10 {
		clock = clock.Add(time.Second)
		svc.ScaleTick(ctx, clock)
	}
	if slotsOf(svc, "chat") != 2 || loader.starts() != 1 {
		t.Fatalf("a single request resized the model: slots=%d starts=%d", slotsOf(svc, "chat"), loader.starts())
	}

	// A burst: 7 requests on a 2-slot model. Two run, five wait at the gate.
	done := make(chan error, 6)
	for range 6 {
		go func() {
			ts, err := eng.Complete(ctx, rt.CompletionRequest{Model: "chat", Kind: rt.KindChat,
				Messages: []rt.Message{{Role: "user", Content: "hi"}}, Params: rt.GenerationParams{Seed: 1, MaxTokens: 5}})
			if err == nil {
				_, _, _, err = rt.Drain(ts)
			}
			done <- err
		}()
	}
	waitUntil(t, "burst registered as demand", func() bool { now, _, _ := eng.Demand("chat"); return now == 7 })
	// Not sustained yet: one tick is not three seconds.
	clock = clock.Add(time.Second)
	svc.ScaleTick(ctx, clock)
	if slotsOf(svc, "chat") != 2 {
		t.Fatal("grew on the first saturated sample")
	}
	clock = clock.Add(2 * time.Second)
	svc.ScaleTick(ctx, clock)
	clock = clock.Add(time.Second)
	svc.ScaleTick(ctx, clock)
	if l := svc.Layouts()["chat"]; l.Slots != 8 || l.CtxPerSlot != 16384 {
		t.Fatalf("layout after the burst = %+v, want 8 x 16384 (smallest power of two holding 7)", l)
	}
	// Nothing failed: the waiters moved to the new instance and finished,
	// and the request that was running on the old one is still running.
	for range 6 {
		if err := <-done; err != nil {
			t.Fatalf("a request failed across the resize: %v", err)
		}
	}
	if _, err := one[0].Recv(); err != nil {
		t.Fatalf("the in-flight request was broken by the resize: %v", err)
	}
	_ = one[0].Close()
	if eng.DefaultModel() != "chat" {
		t.Fatalf("default after the resize = %q", eng.DefaultModel())
	}

	// Cooldown: demand at the new slot count right away does not resize again.
	busy := hold(t, eng, "chat", 8)
	for range 5 {
		clock = clock.Add(time.Second)
		svc.ScaleTick(ctx, clock)
	}
	if slotsOf(svc, "chat") != 8 {
		t.Fatalf("resized inside the cooldown: %d slots", slotsOf(svc, "chat"))
	}
	// After it, sustained saturation doubles to the ceiling.
	clock = clock.Add(resizeCooldown)
	svc.ScaleTick(ctx, clock)
	if slotsOf(svc, "chat") != 16 {
		t.Fatalf("slots under sustained demand = %d, want the ceiling", slotsOf(svc, "chat"))
	}
	grownAt := clock
	for _, ts := range busy {
		if _, err := ts.Recv(); err != nil {
			t.Fatalf("in-flight request broken by the second resize: %v", err)
		}
		_ = ts.Close()
	}

	// Idle: no shrink until the window has passed...
	clock = clock.Add(time.Second)
	svc.ScaleTick(ctx, clock)
	clock = grownAt.Add(shrinkAfter - time.Second)
	svc.ScaleTick(ctx, clock)
	if slotsOf(svc, "chat") != 16 {
		t.Fatal("shrank before the idle window passed")
	}
	// ...then down, through the size that held the last peak (8), to the
	// floor once a whole window has seen nothing.
	clock = clock.Add(3 * time.Second)
	svc.ScaleTick(ctx, clock)
	if got := slotsOf(svc, "chat"); got != 8 {
		t.Fatalf("slots after the first idle window = %d, want 8 (the window's peak)", got)
	}
	clock = clock.Add(time.Second)
	svc.ScaleTick(ctx, clock)
	clock = clock.Add(shrinkAfter + time.Second)
	svc.ScaleTick(ctx, clock)
	if slotsOf(svc, "chat") != 2 {
		t.Fatalf("slots after idling = %d, want the floor", slotsOf(svc, "chat"))
	}
	// Replaced instances are shut down once empty, and their memory is
	// given back.
	waitUntil(t, "drained instances released", func() bool {
		svc.mu.Lock()
		defer svc.mu.Unlock()
		return svc.drainingMB == 0
	})
	if got := eng.Models(); len(got) != 1 {
		t.Fatalf("models loaded = %d", len(got))
	}
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A grow has to fit in free memory next to the instance it replaces:
// otherwise it settles for fewer slots, or does not happen, and backs off.
func TestGrowIsBoundedByMemory(t *testing.T) {
	old1, old2, old3 := growAfter, shrinkAfter, resizeCooldown
	t.Cleanup(func() { growAfter, shrinkAfter, resizeCooldown = old1, old2, old3 })
	growAfter, resizeCooldown = 3*time.Second, time.Minute

	svc, eng, loader := scaleHarness(t, 50, "chat")
	ctx := context.Background()
	if err := svc.Load(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	// 2 GB of weights and 112 KB per context token (the 3B's real figure):
	// 16 x 16384 would be ~28 GB of KV. Charge the running 2-slot instance
	// what it really costs.
	kvPlan(svc, "chat", 2<<30, 112<<10)
	svc.mu.Lock()
	svc.loads["chat"].EstimateMB = 6200
	svc.mu.Unlock()

	// 16 slots at the context floor do not fit beside the old instance in
	// a 20 GB budget: the plan keeps the floor and takes the slots that fit.
	svc.SetMemoryBudgetMB(20000)
	from, to, err := svc.Resize(ctx, "chat", 16)
	if err != nil || from != 2 {
		t.Fatalf("resize = %d -> %d, %v", from, to, err)
	}
	l := svc.Layouts()["chat"]
	if l.Slots != to || l.Slots <= 2 || l.Slots >= 16 || l.CtxPerSlot < 8192 {
		t.Fatalf("layout = %+v, want more than 2 and fewer than 16 slots at >= 8192", l)
	}
	used := svc.usedMB()
	if used > 20000 {
		t.Fatalf("resize overran the budget: %d MB charged (both instances)", used)
	}
	waitUntil(t, "old instance drained", func() bool { svc.mu.Lock(); defer svc.mu.Unlock(); return svc.drainingMB == 0 })

	// With the budget nearly full, growing further is refused, nothing is
	// started, and the model keeps serving at its layout.
	svc.SetMemoryBudgetMB(svc.usedMB() + 1024)
	starts := loader.starts()
	if _, _, err := svc.Resize(ctx, "chat", 16); !errors.Is(err, errNoRoom) {
		t.Fatalf("resize with no room = %v", err)
	}
	if loader.starts() != starts {
		t.Fatal("a runtime was started for a resize that does not fit")
	}
	ts := hold(t, eng, "chat", 1)
	_ = ts[0].Close()

	// A runtime that will not start leaves the model as it was.
	svc.SetMemoryBudgetMB(0)
	loader.mu.Lock()
	loader.fail = true
	loader.mu.Unlock()
	before := svc.Layouts()["chat"]
	if _, _, err := svc.Resize(ctx, "chat", 1); err == nil {
		t.Fatal("resize with a failing runtime succeeded")
	}
	if svc.Layouts()["chat"] != before || len(eng.Models()) != 1 {
		t.Fatalf("a failed resize changed the model: %+v", svc.Layouts()["chat"])
	}
	ts = hold(t, eng, "chat", 1)
	_ = ts[0].Close()
}

// Pinned slots (floor >= ceiling) and decision models do not scale.
func TestPinnedAndDecisionModelsDoNotScale(t *testing.T) {
	old1, old3 := growAfter, resizeCooldown
	t.Cleanup(func() { growAfter, resizeCooldown = old1, old3 })
	growAfter, resizeCooldown = time.Second, time.Second

	svc, eng, loader := scaleHarness(t, 50, "chat")
	svc.MinConcurrent = 4
	svc.Budget.MaxConcurrent = 4
	ctx := context.Background()
	if err := svc.Load(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	if slotsOf(svc, "chat") != 4 {
		t.Fatalf("pinned model loaded with %d slots, want 4", slotsOf(svc, "chat"))
	}
	streams := hold(t, eng, "chat", 4)
	clock := time.Now().Add(time.Hour)
	for range 10 {
		clock = clock.Add(time.Second)
		svc.ScaleTick(ctx, clock)
	}
	for _, ts := range streams {
		_ = ts.Close()
	}
	clock = clock.Add(time.Hour)
	svc.ScaleTick(ctx, clock)
	clock = clock.Add(time.Hour)
	svc.ScaleTick(ctx, clock)
	if slotsOf(svc, "chat") != 4 || loader.starts() != 1 {
		t.Fatalf("a pinned model was resized: slots=%d starts=%d", slotsOf(svc, "chat"), loader.starts())
	}
	// A decision model is marked not scalable at load.
	svc.mu.Lock()
	svc.loads["chat"].Scalable = false
	svc.loads["chat"].Slots = 2
	svc.mu.Unlock()
	svc.SetPlanLimits(PlanLimits{MaxConcurrent: 16, MaxContext: 16384, MinContext: 8192})
	svc.mu.Lock()
	svc.loads["chat"].Limits = svc.plan
	svc.mu.Unlock()
	streams = hold(t, eng, "chat", 4)
	for range 10 {
		clock = clock.Add(time.Second)
		svc.ScaleTick(ctx, clock)
	}
	for _, ts := range streams {
		_ = ts.Close()
	}
	if loader.starts() != 1 {
		t.Fatal("a non-scalable model was resized")
	}
}

// When another model needs the room, a grown idle model gives its extra
// slots back before anything is unloaded.
func TestMemoryPressureShrinksBeforeUnloading(t *testing.T) {
	svc, eng, _ := scaleHarness(t, 50, "chat", "other")
	ctx := context.Background()
	if err := svc.Load(ctx, "chat"); err != nil {
		t.Fatal(err)
	}
	// A non-default model, so admission would be allowed to unload it.
	if err := svc.Load(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Unload(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	inst, _ := rt.NewMockRuntime(0).Load(ctx, rt.ModelSpec{ID: "default-chat"}, rt.ResourceBudget{})
	eng.Register(rt.ModelSpec{ID: "default-chat"}, inst)
	if err := eng.SetDefault("default-chat"); err != nil {
		t.Fatal(err)
	}
	kvPlan(svc, "chat", 2<<30, 112<<10)
	if _, to, err := svc.Resize(ctx, "chat", 8); err != nil || to != 8 {
		t.Fatalf("grow to 8 = %d, %v", to, err)
	}
	waitUntil(t, "old instance drained", func() bool { svc.mu.Lock(); defer svc.mu.Unlock(); return svc.drainingMB == 0 })
	grown := svc.usedMB()
	if grown < 14000 {
		t.Fatalf("8 x 16384 at 112 KB/token charged only %d MB", grown)
	}
	// A budget with room for the floor layout plus the newcomer, not for
	// the grown one.
	svc.SetMemoryBudgetMB(grown + 1000)
	var cached []string
	svc.OnUnloaded = func(id string) { cached = append(cached, id) }
	if err := svc.Load(ctx, "other"); err != nil {
		t.Fatalf("load beside a grown model: %v", err)
	}
	if got := slotsOf(svc, "chat"); got != 2 {
		t.Fatalf("grown idle model has %d slots after pressure, want the floor", got)
	}
	loaded := map[string]bool{}
	for _, m := range eng.Models() {
		loaded[m.Spec.ID] = true
	}
	if !loaded["chat"] || !loaded["other"] || len(cached) != 0 {
		t.Fatalf("pressure unloaded a model instead of shrinking: loaded=%v cached=%v", loaded, cached)
	}
}

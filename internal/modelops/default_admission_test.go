package modelops

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// The laptop on 2026-11-26: the 35B default was still downloading when the
// mesh placed an 8B model, which loaded first and so became the engine's
// default. From then on the real default (28672 MB) could not load beside
// it (4831 MB) inside a 32768 MB budget, and nothing would unload the 8B
// model because it was "the default". 96 failed attempts over 8 hours.
func TestLoadDefaultUnloadsTheStandInDefault(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"big-default": 28672, "placed": 4831}, 0)
	svc.SetMemoryBudgetMB(32768)
	ctx := context.Background()

	if _, err := svc.LoadInstanceOrigin(ctx, "placed", models.OriginMesh); err != nil {
		t.Fatal(err)
	}
	if eng.DefaultModel() != "placed" {
		t.Fatalf("the first chat model loaded should stand in as default, got %q", eng.DefaultModel())
	}

	// An ordinary load still respects the running default.
	if err := svc.Load(ctx, "big-default"); !errors.Is(err, ErrOverMemory) {
		t.Fatalf("ordinary load: %v, want ErrOverMemory", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[placed]" {
		t.Fatalf("after the refused load: loaded = %v", got)
	}

	// The operator's default does not.
	if _, err := svc.LoadDefault(ctx, "big-default"); err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[big-default]" {
		t.Fatalf("loaded = %v, want only the default", got)
	}
	if eng.DefaultModel() != "big-default" {
		t.Fatalf("default = %q", eng.DefaultModel())
	}
	if !svc.Mgr.Has("placed") {
		t.Fatal("the stand-in's file must stay cached")
	}
}

// Other idle models go before the model that is answering bare requests.
func TestLoadDefaultKeepsTheStandInWhenOthersMakeEnoughRoom(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"stand-in": 1000, "other": 2000, "real": 3000}, 0)
	svc.SetMemoryBudgetMB(4000)
	ctx := context.Background()
	for _, id := range []string{"stand-in", "other"} {
		if err := svc.Load(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.LoadDefault(ctx, "real"); err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[real stand-in]" {
		t.Fatalf("loaded = %v, want the stand-in kept and other unloaded", got)
	}
}

// A stand-in with a request in flight is never cut off: the load is
// refused (start-up retries it) and goes through once the request ends.
func TestLoadDefaultWaitsForABusyStandIn(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"stand-in": 2000, "real": 3000}, 5)
	svc.SetMemoryBudgetMB(4000)
	ctx := context.Background()
	if err := svc.Load(ctx, "stand-in"); err != nil {
		t.Fatal(err)
	}
	stream, err := eng.Complete(ctx, rt.CompletionRequest{ID: "r", Model: "stand-in", Kind: rt.KindChat,
		Messages: []rt.Message{{Role: "user", Content: "x"}}, Params: rt.GenerationParams{Seed: 1, MaxTokens: 1000}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoadDefault(ctx, "real"); !errors.Is(err, ErrOverMemory) {
		t.Fatalf("with the stand-in busy: %v, want ErrOverMemory", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[stand-in]" {
		t.Fatalf("a busy stand-in must stay loaded, got %v", got)
	}
	_ = stream.Close()
	if _, err := svc.LoadDefault(ctx, "real"); err != nil {
		t.Fatalf("after the request ended: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[real]" {
		t.Fatalf("loaded = %v", got)
	}
}

// A default larger than the whole budget still loads once it is alone, as
// it does on a node with nothing else loaded.
func TestLoadDefaultLargerThanTheBudgetLoadsAlone(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"stand-in": 1000, "huge": 5000}, 0)
	svc.SetMemoryBudgetMB(3000)
	ctx := context.Background()
	if err := svc.Load(ctx, "stand-in"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.LoadDefault(ctx, "huge"); err != nil {
		t.Fatalf("LoadDefault: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[huge]" {
		t.Fatalf("loaded = %v", got)
	}
}

// failingLoader refuses one model and loads the rest.
type failingLoader struct {
	Loader
	fail string
}

func (f failingLoader) Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error) {
	if m.ID == f.fail {
		return nil, errors.New("runtime would not start")
	}
	return f.Loader.Load(ctx, m, res)
}

// Picking a new default in the app: the outgoing one makes room, the new
// one becomes the default.
func TestSwitchDefaultReplacesTheOutgoingDefault(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"old": 3000, "new": 3000}, 0)
	svc.SetMemoryBudgetMB(4000)
	ctx := context.Background()
	if err := svc.Load(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SwitchDefault(ctx, "new"); err != nil {
		t.Fatalf("SwitchDefault: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[new]" || eng.DefaultModel() != "new" {
		t.Fatalf("loaded = %v default = %q", got, eng.DefaultModel())
	}
}

// When both fit, nothing is unloaded and only the default moves.
func TestSwitchDefaultKeepsBothWhenTheyFit(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"old": 1000, "new": 1000}, 0)
	svc.SetMemoryBudgetMB(4000)
	ctx := context.Background()
	if err := svc.Load(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SwitchDefault(ctx, "new"); err != nil {
		t.Fatalf("SwitchDefault: %v", err)
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[new old]" || eng.DefaultModel() != "new" {
		t.Fatalf("loaded = %v default = %q", got, eng.DefaultModel())
	}
}

// A new default that will not start must not cost the node the one it had.
func TestSwitchDefaultPutsTheOldDefaultBackWhenTheNewOneFails(t *testing.T) {
	svc, eng := memHarness(t, map[string]int64{"old": 3000, "new": 3000}, 0)
	svc.SetMemoryBudgetMB(4000)
	svc.Loader = failingLoader{Loader: svc.Loader, fail: "new"}
	ctx := context.Background()
	if err := svc.Load(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SwitchDefault(ctx, "new"); err == nil {
		t.Fatal("SwitchDefault should report the failed load")
	}
	if got := loadedIDs(eng); fmt.Sprint(got) != "[old]" || eng.DefaultModel() != "old" {
		t.Fatalf("loaded = %v default = %q, want the old default back", got, eng.DefaultModel())
	}
}

package assign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// reports captures ModelState reports in order.
type reports struct {
	mu   sync.Mutex
	list []*typesv1.ModelState
}

func (r *reports) add(m *typesv1.ModelState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, m)
	return nil
}

func (r *reports) states(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.list {
		if m.GetModelId() == id {
			out = append(out, m.GetState())
		}
	}
	return out
}

type harness struct {
	svc     *Service
	ops     *modelops.Service
	eng     *engine.Engine
	rep     *reports
	policy  Policy
	battery bool
	mu      sync.Mutex
	blobs   map[string][]byte
	cancel  context.CancelFunc
}

// newHarness serves a catalog of the given models (id → blob) and wires a
// mock-runtime modelops behind an assign.Service with the worker running.
func newHarness(t *testing.T, maxDiskMB int64, blobs map[string][]byte) *harness {
	t.Helper()
	h := &harness{rep: &reports{}, blobs: blobs, policy: Policy{MeshManaged: true}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := filepath.Base(r.URL.Path)
		if b, ok := blobs[id]; ok {
			_, _ = w.Write(b)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cat := "models:\n"
	for id, b := range blobs {
		cat += fmt.Sprintf("  - id: %s\n    sha256: %s\n    artifact_url: %s/%s\n    size_bytes: %d\n", id, shaOf(b), srv.URL, id, len(b))
	}
	catPath := filepath.Join(dir, "catalog.yaml")
	if err := os.WriteFile(catPath, []byte(cat), 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := models.NewManager(filepath.Join(dir, "models"), maxDiskMB, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	h.eng = engine.New(nil, nil, nil)
	h.ops = &modelops.Service{
		Mgr: mgr, Eng: h.eng, Loader: rt.NewMockRuntime(0),
		Budget: rt.ResourceBudget{MaxConcurrent: 2}, Log: quietLog(), ManifestPath: catPath,
	}
	h.svc = &Service{
		Ops: h.ops, Mgr: mgr, Eng: h.eng, Log: quietLog(),
		OnBattery: func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.battery },
		Policy:    func() Policy { h.mu.Lock(); defer h.mu.Unlock(); return h.policy },
	}
	h.svc.SetReporter(h.rep.add)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(cancel)
	go h.svc.Run(ctx)
	return h
}

func (h *harness) spec(id string) *typesv1.ModelSpec {
	return &typesv1.ModelSpec{Id: id, Sha256: shaOf(h.blobs[id]), SizeBytes: uint64(len(h.blobs[id]))}
}

func (h *harness) loaded(id string) bool {
	for _, m := range h.eng.Models() {
		if m.Spec.ID == id {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func assignMsg(specs ...*typesv1.ModelSpec) *tunnelv1.ModelAssignment {
	return &tunnelv1.ModelAssignment{Assign: specs}
}

func TestAssignmentDownloadsLoadsAndReports(t *testing.T) {
	h := newHarness(t, 0, map[string][]byte{"tiny": []byte("gguf tiny")})
	h.svc.Apply(context.Background(), assignMsg(h.spec("tiny")))
	waitFor(t, "model loaded", func() bool { return h.loaded("tiny") })
	waitFor(t, "ready report", func() bool {
		st := h.rep.states("tiny")
		return len(st) > 0 && st[len(st)-1] == StateReady
	})
	if got := h.rep.states("tiny"); fmt.Sprint(got) != "[assigned downloading ready]" {
		t.Fatalf("reports = %v", got)
	}
	if o := h.svc.Mgr.Origin("tiny"); o != models.OriginMesh {
		t.Fatalf("origin = %q, want mesh", o)
	}
	if len(h.svc.Pending()) != 0 {
		t.Fatalf("pending after ready: %+v", h.svc.Pending())
	}
	// Idempotent: a repeated push just re-acknowledges ready.
	h.svc.Apply(context.Background(), assignMsg(h.spec("tiny")))
	waitFor(t, "second ready ack", func() bool { return len(h.rep.states("tiny")) == 4 })
	if st := h.rep.states("tiny"); st[3] != StateReady {
		t.Fatalf("repeat push reports = %v", st)
	}
}

func TestAssignmentDeclinedByPolicy(t *testing.T) {
	h := newHarness(t, 0, map[string][]byte{"a": []byte("a"), "b": []byte("b")})
	h.mu.Lock()
	h.policy = Policy{MeshManaged: false}
	h.mu.Unlock()
	h.svc.Apply(context.Background(), assignMsg(h.spec("a")))
	waitFor(t, "declined", func() bool { return len(h.rep.states("a")) == 1 })
	if st := h.rep.states("a"); st[0] != StateDeclined {
		t.Fatalf("mesh_managed=false: %v", st)
	}
	p, _ := h.svc.Get("a")
	if p.Error == "" {
		t.Fatal("declined without a reason")
	}

	h.mu.Lock()
	h.policy = Policy{MeshManaged: true, Exclude: []string{"b"}}
	h.mu.Unlock()
	h.svc.Apply(context.Background(), assignMsg(h.spec("b")))
	waitFor(t, "excluded declined", func() bool { return len(h.rep.states("b")) == 1 })
	if st := h.rep.states("b"); st[0] != StateDeclined {
		t.Fatalf("exclude: %v", st)
	}
	// Not in the heartbeat: declines are one-shot reports.
	if len(h.svc.States()) != 0 {
		t.Fatalf("States() = %v, want none", h.svc.States())
	}
	if h.loaded("a") || h.loaded("b") {
		t.Fatal("declined model was loaded")
	}
}

func TestAssignmentUnknownAndShaMismatch(t *testing.T) {
	h := newHarness(t, 0, map[string][]byte{"a": []byte("a")})
	h.svc.Apply(context.Background(), &tunnelv1.ModelAssignment{Assign: []*typesv1.ModelSpec{
		{Id: "nope", Sha256: "00"},
		{Id: "a", Sha256: "deadbeef"},
	}})
	waitFor(t, "both concluded", func() bool { return len(h.rep.states("nope")) == 1 && len(h.rep.states("a")) == 1 })
	if h.rep.states("nope")[0] != StateDeclined {
		t.Fatalf("unknown model: %v", h.rep.states("nope"))
	}
	if h.rep.states("a")[0] != StateFailed {
		t.Fatalf("sha mismatch: %v (must refuse, hash pinning)", h.rep.states("a"))
	}
}

func TestEvictionOnlyTouchesMeshPlacedModels(t *testing.T) {
	h := newHarness(t, 0, map[string][]byte{"mine": []byte("mine"), "theirs": []byte("theirs")})
	// The operator installs "mine" themselves; the mesh places "theirs".
	if err := h.ops.Load(context.Background(), "mine"); err != nil {
		t.Fatal(err)
	}
	h.svc.Apply(context.Background(), assignMsg(h.spec("theirs")))
	waitFor(t, "theirs loaded", func() bool { return h.loaded("theirs") })

	h.svc.Apply(context.Background(), &tunnelv1.ModelAssignment{EvictModelIds: []string{"mine", "theirs"}})
	waitFor(t, "theirs evicted", func() bool { return !h.loaded("theirs") })
	if !h.loaded("mine") || h.svc.Mgr.Origin("mine") != models.OriginOperator {
		t.Fatal("mesh eviction touched the operator's model")
	}
	if h.svc.Mgr.Origin("theirs") != "" {
		t.Fatal("evicted model still cached")
	}
	if st := h.rep.states("theirs"); st[len(st)-1] != StateEvicted {
		t.Fatalf("no evicted report: %v", st)
	}
	if len(h.rep.states("mine")) != 0 {
		t.Fatalf("operator model produced reports: %v", h.rep.states("mine"))
	}
}

func TestBudgetDeclinesWithoutEvictingOperatorModels(t *testing.T) {
	big := make([]byte, 3*1024*1024)
	h := newHarness(t, 4, map[string][]byte{"mine": big, "theirs": big}) // 4 MB budget
	if err := h.ops.Load(context.Background(), "mine"); err != nil {
		t.Fatal(err)
	}
	h.svc.Apply(context.Background(), assignMsg(h.spec("theirs")))
	waitFor(t, "declined", func() bool {
		st := h.rep.states("theirs")
		return len(st) > 0 && st[len(st)-1] == StateDeclined
	})
	if !h.loaded("mine") {
		t.Fatal("budget pressure evicted the operator's model")
	}
	p, _ := h.svc.Get("theirs")
	if p.Error == "" {
		t.Fatalf("declined without reason: %+v", p)
	}
}

func TestDownloadsWaitForACPower(t *testing.T) {
	old := batteryPoll
	batteryPoll = 20 * time.Millisecond
	t.Cleanup(func() { batteryPoll = old })

	h := newHarness(t, 0, map[string][]byte{"a": []byte("a")})
	h.mu.Lock()
	h.battery = true
	h.mu.Unlock()
	h.svc.Apply(context.Background(), assignMsg(h.spec("a")))
	time.Sleep(100 * time.Millisecond)
	if st := h.rep.states("a"); fmt.Sprint(st) != "[assigned]" {
		t.Fatalf("on battery, reports = %v (must stay assigned)", st)
	}
	if got := h.svc.States(); len(got) != 1 || got[0].GetState() != StateAssigned {
		t.Fatalf("heartbeat states = %v", got)
	}
	h.mu.Lock()
	h.battery = false
	h.mu.Unlock()
	waitFor(t, "loaded on AC", func() bool { return h.loaded("a") })
}

func TestMockRuntimeDeclines(t *testing.T) {
	svc := &Service{Log: quietLog(), Policy: func() Policy { return Policy{MeshManaged: true} }}
	rep := &reports{}
	svc.SetReporter(rep.add)
	svc.Apply(context.Background(), assignMsg(&typesv1.ModelSpec{Id: "x"}))
	if st := rep.states("x"); fmt.Sprint(st) != "[declined]" {
		t.Fatalf("reports = %v", st)
	}
}

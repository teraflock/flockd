package assign

import (
	"context"
	"fmt"
	"testing"

	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"

	"github.com/teraflock/flockd/internal/activity"
)

// reasons returns the reason of every report for id, in order.
func (r *reports) reasons(id string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, m := range r.list {
		if m.GetModelId() == id {
			out = append(out, m.GetReason())
		}
	}
	return out
}

func placedMsg(reason tunnelv1.PlacementReason, specs ...*typesv1.ModelSpec) *tunnelv1.ModelAssignment {
	ma := &tunnelv1.ModelAssignment{}
	for _, sp := range specs {
		ma.Placements = append(ma.Placements, &tunnelv1.Placement{ModelId: sp.GetId(), Reason: reason})
	}
	return ma
}

// A placement's reason (design note 2026-11-01) is kept on the pending
// record, echoed on the in-flight reports, written to the activity row,
// and lands on the cache entry so it outlives the pending record.
func TestPlacementReasonFollowsTheModel(t *testing.T) {
	h := newHarness(t, 0, map[string][]byte{"tiny": []byte("gguf tiny"), "st": []byte("gguf staged")})
	act := activity.New(0)
	h.svc.Activity = act

	ma := placedMsg(tunnelv1.PlacementReason_PLACEMENT_REASON_FLOOR, h.spec("tiny"))
	ma.Assign = []*typesv1.ModelSpec{h.spec("tiny")}
	h.svc.Apply(context.Background(), ma)
	if p, ok := h.svc.Get("tiny"); ok && p.Reason != ReasonFloor {
		t.Fatalf("pending reason = %q, want floor", p.Reason)
	}
	waitFor(t, "ready", func() bool { return h.loaded("tiny") && len(h.svc.Pending()) == 0 })
	waitFor(t, "ready report", func() bool { st := h.rep.states("tiny"); return len(st) == 3 })
	if got := h.rep.reasons("tiny"); fmt.Sprint(got) != "[floor floor ]" {
		t.Fatalf("report reasons = %q (assigned and downloading echo the placement; ready carries none)", got)
	}
	if got := h.svc.Mgr.Placement("tiny"); got != ReasonFloor {
		t.Fatalf("cache entry placement = %q, want floor", got)
	}
	if got := h.svc.PlacementReason("tiny"); got != ReasonFloor {
		t.Fatalf("PlacementReason = %q", got)
	}
	var row activity.Event
	for _, e := range act.List() {
		if e.Kind == activity.KindAssignment && e.Model == "tiny" {
			row = e
		}
	}
	if row.Detail != DescribeReason(ReasonFloor) {
		t.Fatalf("activity detail = %q", row.Detail)
	}

	// A later warm of the cached copy is a new placement: the reason moves.
	h.ops.Unload(context.Background(), "tiny")
	ma = placedMsg(tunnelv1.PlacementReason_PLACEMENT_REASON_WARM, h.spec("tiny"))
	ma.Assign = []*typesv1.ModelSpec{h.spec("tiny")}
	h.svc.Apply(context.Background(), ma)
	waitFor(t, "warmed", func() bool { return h.loaded("tiny") && h.svc.Mgr.Placement("tiny") == ReasonWarm })

	// A stage carries its reason to the cached report and the entry.
	ma = placedMsg(tunnelv1.PlacementReason_PLACEMENT_REASON_STAGE, h.spec("st"))
	ma.Stage = []*typesv1.ModelSpec{h.spec("st")}
	h.svc.Apply(context.Background(), ma)
	waitFor(t, "staged", func() bool { st := h.rep.states("st"); return len(st) == 3 && st[2] == StateCached })
	if got := h.rep.reasons("st"); fmt.Sprint(got) != "[stage stage stage]" {
		t.Fatalf("stage report reasons = %q", got)
	}
	if got := h.svc.Mgr.Placement("st"); got != ReasonStage {
		t.Fatalf("staged entry placement = %q", got)
	}

	// No placements (an older coordinator): nothing is claimed.
	h.svc.Apply(context.Background(), assignMsg(h.spec("st")))
	waitFor(t, "loaded", func() bool { return h.loaded("st") })
	if got := h.svc.Mgr.Placement("st"); got != ReasonStage {
		t.Fatalf("an assignment without a reason must keep the last one, got %q", got)
	}
}

// A declined or failed report carries the daemon's reason — the text the
// local API shows — so the coordinator records the cause, not just the
// verdict; the pending record keeps the placement reason beside it.
func TestConcludedReportsCarryTheReason(t *testing.T) {
	h := newHarness(t, 0, map[string][]byte{"a": []byte("a")})
	h.mu.Lock()
	h.policy = Policy{MeshManaged: true, Exclude: []string{"a"}}
	h.mu.Unlock()
	ma := placedMsg(tunnelv1.PlacementReason_PLACEMENT_REASON_DEMAND, h.spec("a"), &typesv1.ModelSpec{Id: "nope"})
	ma.Assign = []*typesv1.ModelSpec{h.spec("a"), {Id: "nope", Sha256: "00"}}
	h.svc.Apply(context.Background(), ma)
	waitFor(t, "both concluded", func() bool { return len(h.rep.states("a")) == 1 && len(h.rep.states("nope")) == 1 })
	p, _ := h.svc.Get("a")
	if p.State != StateDeclined || p.Error == "" || p.Reason != ReasonDemand {
		t.Fatalf("pending = %+v", p)
	}
	if got := h.rep.reasons("a"); len(got) != 1 || got[0] != p.Error {
		t.Fatalf("declined report reason = %q, want %q", got, p.Error)
	}
	if got := h.rep.reasons("nope"); len(got) != 1 || got[0] != "model not in this node's catalog" {
		t.Fatalf("unknown-model report reason = %q", got)
	}
	if got := h.svc.PlacementReason("nope"); got != ReasonDemand {
		t.Fatalf("PlacementReason(nope) = %q", got)
	}
}

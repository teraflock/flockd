package live

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/events"
)

func TestStartFinishAndOrder(t *testing.T) {
	tr := New(10)
	now := time.Unix(1000, 0)
	tr.clock = func() time.Time { return now }

	a := tr.Start("laya-q8_0", KindDecision, OriginMesh)
	now = now.Add(time.Second)
	b := tr.Start("llama", KindChat, "") // "" = the local API
	b.AddTokens(3)
	b.AddTokens(2)
	now = now.Add(500 * time.Millisecond)

	in := tr.InFlight()
	if len(in) != 2 || in[0].Model != "laya-q8_0" || in[1].Model != "llama" {
		t.Fatalf("in flight (oldest first) = %+v", in)
	}
	if in[0].ElapsedMS != 1500 || in[0].Origin != OriginMesh || in[0].Kind != KindDecision || in[0].Tokens != 0 {
		t.Fatalf("first = %+v", in[0])
	}
	if in[1].ElapsedMS != 500 || in[1].Origin != OriginLocal || in[1].Tokens != 5 {
		t.Fatalf("second = %+v", in[1])
	}
	c := tr.CountsByModel()
	if c["laya-q8_0"].Total != 1 || c["laya-q8_0"].ByKind[KindDecision] != 1 || c["llama"].ByKind[KindChat] != 1 {
		t.Fatalf("counts = %+v", c)
	}

	b.Finish(OutcomeCancelled, 7, -1) // no usage chunk: streamed count stands
	b.Finish(OutcomeOK, 99, 99)       // only the first Finish counts
	now = now.Add(time.Second)
	a.Finish(OutcomeOK, 172, 0)

	if got := tr.InFlight(); len(got) != 0 {
		t.Fatalf("in flight after finish = %+v", got)
	}
	rec := tr.Recent(0)
	if len(rec) != 2 || rec[0].Model != "laya-q8_0" || rec[1].Model != "llama" {
		t.Fatalf("recent (newest first) = %+v", rec)
	}
	if rec[0] != (Finished{ID: rec[0].ID, Model: "laya-q8_0", Kind: KindDecision, Origin: OriginMesh,
		StartedAt: time.Unix(1000, 0), DurationMS: 2500, PromptTokens: 172, Outcome: OutcomeOK}) {
		t.Fatalf("decision row = %+v", rec[0])
	}
	if rec[1].Outcome != OutcomeCancelled || rec[1].PromptTokens != 7 || rec[1].CompletionTokens != 5 || rec[1].DurationMS != 500 {
		t.Fatalf("chat row = %+v", rec[1])
	}
	if rec[0].ID == rec[1].ID || !strings.HasPrefix(rec[0].ID, "r") {
		t.Fatalf("ids = %q %q", rec[0].ID, rec[1].ID)
	}
	if got := tr.Recent(1); len(got) != 1 || got[0].Model != "laya-q8_0" {
		t.Fatalf("Recent(1) = %+v", got)
	}
}

func TestRingIsBounded(t *testing.T) {
	tr := New(5)
	for i := range 23 {
		tr.Start(fmt.Sprintf("m%d", i), KindChat, OriginLocal).Finish(OutcomeOK, 1, 1)
	}
	rec := tr.Recent(0)
	if len(rec) != 5 || rec[0].Model != "m22" || rec[4].Model != "m18" {
		t.Fatalf("ring = %+v", rec)
	}
	if len(tr.ring) != 5 || len(tr.inflight) != 0 {
		t.Fatalf("ring cap %d, inflight %d", len(tr.ring), len(tr.inflight))
	}
	if got := New(0); len(got.ring) != DefaultCapacity {
		t.Fatalf("default capacity = %d", len(got.ring))
	}
}

// What the tracker keeps and publishes is exactly these fields: there is
// nowhere for a prompt, an output, an error message or an outside
// identifier to go.
func TestNoContentRetained(t *testing.T) {
	want := map[string][]string{
		"InFlight": {"elapsed_ms", "id", "kind", "model", "origin", "started_at", "tokens"},
		"Finished": {"completion_tokens", "duration_ms", "id", "kind", "model", "origin", "outcome", "prompt_tokens", "started_at"},
	}
	for name, v := range map[string]any{"InFlight": InFlight{}, "Finished": Finished{}} {
		raw, _ := json.Marshal(v)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, want[name]) {
			t.Errorf("%s fields = %v, want %v", name, keys, want[name])
		}
	}
	// The handle has no field that could hold content either.
	ht := reflect.TypeOf(Handle{})
	for i := range ht.NumField() {
		switch ht.Field(i).Name {
		case "t", "seq", "model", "kind", "origin", "start", "tokens", "done":
		default:
			t.Errorf("Handle grew a field %q: make sure it holds no request content", ht.Field(i).Name)
		}
	}
}

func TestEventsPublished(t *testing.T) {
	hub := events.NewHub()
	ch, cancel := hub.Subscribe()
	defer cancel()
	tr := New(4)
	tr.Events = hub
	h := tr.Start("laya-q8_0", KindDecision, OriginChallenge)
	h.Finish(OutcomeInvalidInput, 0, 0)

	ev := <-ch
	st, ok := ev.Data.(InFlight)
	if ev.Type != EventStarted || !ok || st.Model != "laya-q8_0" || st.Origin != OriginChallenge || st.ElapsedMS != 0 {
		t.Fatalf("started event = %+v", ev)
	}
	ev = <-ch
	fin, ok := ev.Data.(Finished)
	if ev.Type != EventFinished || !ok || fin.ID != st.ID || fin.Outcome != OutcomeInvalidInput {
		t.Fatalf("finished event = %+v", ev)
	}
}

func TestNilTrackerAndHandle(t *testing.T) {
	var tr *Tracker
	h := tr.Start("m", KindChat, OriginLocal)
	h.AddTokens(3)
	h.Finish(OutcomeOK, 1, 1)
	if len(tr.InFlight()) != 0 || len(tr.Recent(0)) != 0 || len(tr.CountsByModel()) != 0 {
		t.Fatal("nil tracker returned rows")
	}
}

func TestConcurrentUse(t *testing.T) {
	tr := New(50)
	tr.Events = events.NewHub()
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				h := tr.Start(fmt.Sprintf("m%d", g%3), KindChat, OriginMesh)
				h.AddTokens(i % 7)
				_ = tr.InFlight()
				_ = tr.CountsByModel()
				h.Finish(OutcomeOK, 1, -1)
				_ = tr.Recent(10)
			}
		}()
	}
	wg.Wait()
	if n := len(tr.InFlight()); n != 0 {
		t.Fatalf("%d requests left in flight", n)
	}
	if n := len(tr.Recent(0)); n != 50 {
		t.Fatalf("ring holds %d, want 50", n)
	}
	seen := map[string]bool{}
	for _, f := range tr.Recent(0) {
		if seen[f.ID] {
			t.Fatalf("duplicate id %s", f.ID)
		}
		seen[f.ID] = true
	}
}

package localapi

import (
	"net/http"
	"sort"
	"time"

	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/models"
)

// defaultRecentRequests is how many finished requests GET /api/v1/requests
// returns when the caller does not say.
const defaultRecentRequests = 50

// nodeActivity is the "now" summary: every loaded model with what is in
// flight on it, and the operations that use the machine without a request
// (a runtime starting, a download). It rides on every Status, so it is a
// handful of map reads — nothing here touches the runtimes.
func (s *Server) nodeActivity(now time.Time) gen.NodeActivity {
	out := gen.NodeActivity{Models: []gen.ModelActivity{}, Operations: []gen.ModelOperation{}}
	counts := s.deps.Engine.Requests().CountsByModel()
	for _, m := range s.deps.Engine.Models() {
		row := gen.ModelActivity{Model: m.Spec.ID, Kind: gen.ModelActivityKindChat, InflightByKind: map[string]int{}}
		switch {
		case m.Spec.Decision:
			row.Kind = gen.ModelActivityKindDecision
		case m.Spec.Embeddings:
			row.Kind = gen.ModelActivityKindEmbedding
		}
		if c, ok := counts[m.Spec.ID]; ok {
			row.Inflight, row.InflightByKind = c.Total, c.ByKind
		}
		last := m.LoadedAt
		if u, ok := s.deps.Engine.Usage(m.Spec.ID); ok && u.LastUsed.After(m.LoadedAt) {
			t := u.LastUsed
			row.LastRequestAt, last = &t, t
		}
		if row.Inflight == 0 {
			idle := max(int64(now.Sub(last).Seconds()), 0)
			row.IdleSeconds = &idle
		}
		out.Models = append(out.Models, row)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].Model < out.Models[j].Model })

	if ops := s.deps.ModelOps; ops != nil {
		for id, at := range ops.Starting() {
			t := at
			out.Operations = append(out.Operations, gen.ModelOperation{Model: id, Op: gen.Loading, StartedAt: &t})
		}
	}
	if mgr := s.deps.Models; mgr != nil {
		for _, i := range mgr.List() {
			if i.State != models.StateDownloading {
				continue
			}
			rb, tb := i.ReceivedBytes, i.SizeBytes
			out.Operations = append(out.Operations, gen.ModelOperation{Model: i.ID, Op: gen.Downloading, ReceivedBytes: &rb, TotalBytes: &tb})
		}
	}
	sort.Slice(out.Operations, func(i, j int) bool {
		a, b := out.Operations[i], out.Operations[j]
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		return a.Op < b.Op
	})
	return out
}

// GetRequests implements gen.ServerInterface: the live activity view.
func (s *Server) GetRequests(w http.ResponseWriter, _ *http.Request, params gen.GetRequestsParams) {
	now := time.Now()
	limit := defaultRecentRequests
	if params.Recent != nil {
		limit = min(max(*params.Recent, 0), 200)
	}
	tr := s.deps.Engine.Requests()
	act := s.nodeActivity(now)
	resp := gen.RequestActivity{
		Now: now, Models: act.Models, Operations: act.Operations,
		Inflight: []gen.InflightRequest{}, Recent: []gen.FinishedRequest{},
	}
	for _, r := range tr.InFlight() {
		resp.Inflight = append(resp.Inflight, gen.InflightRequest{
			Id: r.ID, Model: r.Model, Kind: gen.RequestKind(r.Kind), Origin: gen.RequestOrigin(r.Origin),
			StartedAt: r.StartedAt, ElapsedMs: r.ElapsedMS, Tokens: r.Tokens,
		})
	}
	if limit > 0 {
		for _, r := range tr.Recent(limit) {
			resp.Recent = append(resp.Recent, gen.FinishedRequest{
				Id: r.ID, Model: r.Model, Kind: gen.RequestKind(r.Kind), Origin: gen.RequestOrigin(r.Origin),
				StartedAt: r.StartedAt, DurationMs: r.DurationMS,
				PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
				Outcome: gen.RequestOutcome(r.Outcome),
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

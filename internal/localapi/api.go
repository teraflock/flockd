package localapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/logging"
	"github.com/teraflock/flockd/internal/telemetry"
)

// The management API's routes and wire types are generated from
// api/openapi.yaml (make gen) — *Server implements gen.ServerInterface, so
// the spec cannot drift from the implementation. Handlers here fill the
// generated types from daemon internals.

func statsToGen(s telemetry.Snapshot) gen.Stats {
	return gen.Stats{
		TokensPerSec1m:     s.TokensPerSec1m,
		RequestsPerMin:     s.RequestsPerMin,
		TotalRequests:      s.TotalRequests,
		TotalTokens:        s.TotalTokens,
		Inflight:           s.Inflight,
		EarnedMicrocredits: s.EarnedMicrocred,
	}
}

func (s *Server) status() gen.Status {
	resp := gen.Status{
		NodeId:        s.deps.NodeID,
		Version:       s.deps.Version,
		Standalone:    s.deps.Standalone,
		State:         "serving",
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
		DefaultModel:  s.deps.Engine.DefaultModel(),
		ModelsLoaded:  len(s.deps.Engine.Models()),
		Stats:         statsToGen(s.deps.Engine.Stats().Snapshot()),
	}
	if m := s.deps.Mesh; m != nil {
		ms := m()
		resp.Enrolled = ms.Enrolled
		if ms.NodeID != "" {
			resp.NodeId = ms.NodeID
		}
		if !ms.CertExpiresAt.IsZero() {
			t := ms.CertExpiresAt
			resp.CertExpiresAt = &t
		}
	}
	if g := s.deps.Governor; g != nil {
		resp.State = g.State().String()
		resp.Inflight = g.Inflight()
		p := g.Power()
		resp.OnBattery = p.OnBattery
		resp.TempCelsius = p.TempCelsius
	}
	if hw := s.deps.Hardware; hw != nil {
		hs := &gen.Hardware{
			Os:       hw.GetOs(),
			Arch:     hw.GetArch(),
			CpuModel: hw.GetCpuModel(),
			CpuCores: int64(hw.GetCpuCores()),
			RamMb:    int64(hw.GetRamTotalMb()),
			Gpus:     []gen.Gpu{},
		}
		for _, g := range hw.GetGpus() {
			hs.Gpus = append(hs.Gpus, gen.Gpu{
				Vendor: g.GetVendor(), Model: g.GetModel(), VramMb: int64(g.GetVramMb()),
				Accel: g.GetAccel(), UnifiedMemory: g.GetUnifiedMemory(),
			})
		}
		resp.Hardware = hs
	}
	return resp
}

// GetStatus implements gen.ServerInterface.
func (s *Server) GetStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

// GetHealth is the one unauthenticated route: liveness for `tera up`,
// service managers, and the desktop app's daemon probe.
func (s *Server) GetHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, gen.Health{Ok: true, Version: s.deps.Version})
}

// ---- models ----

// ListModels implements gen.ServerInterface.
func (s *Server) ListModels(w http.ResponseWriter, r *http.Request) {
	loaded := map[string]bool{}
	for _, m := range s.deps.Engine.Models() {
		loaded[m.Spec.ID] = true
	}
	def := s.deps.Engine.DefaultModel()

	rows := []gen.ModelRow{}
	if s.deps.Models != nil {
		for _, i := range s.deps.Models.List() {
			row := gen.ModelRow{
				Id: i.ID, SizeBytes: i.SizeBytes, Pinned: i.Pinned,
				LastUsed: i.LastUsed, State: i.State,
				Loaded: loaded[i.ID], Default: i.ID == def,
			}
			if i.State == "downloading" {
				rb := i.ReceivedBytes
				row.ReceivedBytes = &rb
			}
			rows = append(rows, row)
			delete(loaded, i.ID)
		}
	}
	for id := range loaded { // loaded but not cached (mock runtime)
		rows = append(rows, gen.ModelRow{Id: id, State: "ready", Loaded: true, Default: id == def})
	}
	writeJSON(w, http.StatusOK, gen.ModelList{Models: rows})
}

// PinModel implements gen.ServerInterface.
func (s *Server) PinModel(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	if s.deps.Models == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "model cache not enabled (mock runtime)")
		return
	}
	var body gen.PinRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid body")
		return
	}
	if err := s.deps.Models.Pin(id, body.Pinned); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gen.Ok{Ok: true})
}

// DeleteModel implements gen.ServerInterface.
func (s *Server) DeleteModel(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	if s.deps.Models == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "model cache not enabled (mock runtime)")
		return
	}
	if entry := s.deps.Engine.Unregister(id); entry != nil {
		_ = entry.Instance.Shutdown(r.Context())
	}
	if err := s.deps.Models.Remove(id); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gen.Ok{Ok: true})
}

// ---- earnings ----

// GetEarnings implements gen.ServerInterface. Standalone/demo accounting
// until the ledger exists (Phase 2); numbers are honest simulations,
// labelled as such.
func (s *Server) GetEarnings(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Engine.Stats().Snapshot()
	credits := float64(snap.EarnedMicrocred) / 1e6
	usd := credits * 0.000001 * 1e6 // 1 credit = $0.000001 peg (SPEC §4.5)
	uptime := time.Since(s.start).Hours()
	perDay := 0.0
	if uptime > 0 {
		perDay = usd / uptime * 24
	}
	writeJSON(w, http.StatusOK, gen.Earnings{
		EarnedMicrocredits: snap.EarnedMicrocred,
		EarnedCredits:      credits,
		EstUsd:             usd,
		EstUsdPerDay:       perDay,
		LifetimeTokens:     snap.TotalTokens,
		Note:               "simulated standalone earnings; real accrual starts when the node is enrolled with a coordinator (Phase 2 ledger)",
	})
}

// ---- limits ----

// GetLimits implements gen.ServerInterface.
func (s *Server) GetLimits(w http.ResponseWriter, r *http.Request) {
	g := s.deps.Governor
	if g == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "governor not enabled")
		return
	}
	p := g.Policy()
	writeJSON(w, http.StatusOK, gen.Limits{
		ServePolicy:       p.Serve,
		IdleAfterSeconds:  int(p.IdleAfter.Seconds()),
		YieldGraceSeconds: int(p.YieldGrace.Seconds()),
		ServeOnBattery:    p.ServeOnBattery,
		MaxTempCelsius:    p.MaxTempCelsius,
		Schedule:          windowsToStrings(p.Schedule),
	})
}

// UpdateLimits implements gen.ServerInterface.
func (s *Server) UpdateLimits(w http.ResponseWriter, r *http.Request) {
	g := s.deps.Governor
	if g == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "governor not enabled")
		return
	}
	var lim gen.Limits
	if err := json.NewDecoder(r.Body).Decode(&lim); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	switch lim.ServePolicy {
	case "always", "idle-only", "scheduled":
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "serve_policy must be always|idle-only|scheduled")
		return
	}
	windows, err := governor.ParseWindows(lim.Schedule)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	p := g.Policy()
	p.Serve = lim.ServePolicy
	if lim.IdleAfterSeconds > 0 {
		p.IdleAfter = time.Duration(lim.IdleAfterSeconds) * time.Second
	}
	if lim.YieldGraceSeconds > 0 {
		p.YieldGrace = time.Duration(lim.YieldGraceSeconds) * time.Second
	}
	p.ServeOnBattery = lim.ServeOnBattery
	p.MaxTempCelsius = lim.MaxTempCelsius
	p.Schedule = windows
	g.SetPolicy(p)
	// Persist to the daemon-owned overlay (never the operator's
	// config.toml) so limits survive restarts. A write failure loses only
	// persistence, not the live change — log it and answer normally.
	if s.deps.DataDir != "" {
		err := config.SaveLimits(s.deps.DataDir, config.Governor{
			ServePolicy:    p.Serve,
			IdleAfter:      p.IdleAfter,
			YieldGrace:     p.YieldGrace,
			ServeOnBattery: p.ServeOnBattery,
			MaxTempCelsius: p.MaxTempCelsius,
			Schedule:       lim.Schedule,
		})
		if err != nil {
			s.deps.Log.Warn("limits applied but not persisted", "err", err)
		}
	}
	s.GetLimits(w, r)
}

func windowsToStrings(ws []governor.Window) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, fmt.Sprintf("%02d:%02d-%02d:%02d",
			w.StartMin/60, w.StartMin%60, w.EndMin/60, w.EndMin%60))
	}
	return out
}

// ---- logs ----

// GetLogs implements gen.ServerInterface.
func (s *Server) GetLogs(w http.ResponseWriter, r *http.Request, params gen.GetLogsParams) {
	n := 200
	if params.N != nil {
		n = *params.N
	} else if params.Limit != nil {
		n = *params.Limit
	}
	if n < 1 || n > 1024 {
		n = 200
	}
	entries := []gen.LogEntry{}
	if s.deps.LogRing != nil {
		for _, e := range s.deps.LogRing.Tail(n) {
			entries = append(entries, logEntryToGen(e))
		}
	}
	writeJSON(w, http.StatusOK, gen.LogList{Logs: entries})
}

func logEntryToGen(e logging.Entry) gen.LogEntry {
	out := gen.LogEntry{Time: e.Time, Level: e.Level, Message: e.Message}
	if e.Attrs != "" {
		a := e.Attrs
		out.Attrs = &a
	}
	return out
}

// ---- SSE events ----

// handleEvents streams live daemon events: a `status` snapshot every 2
// seconds and immediately on governor transitions, `model_progress` during
// downloads, `models_changed` on load/unload/default/download-complete, and
// (with ?logs=1) each `log` line as it happens. Hand-written (SSE and the
// query-token auth don't fit codegen); documented in api/openapi.yaml under
// the `events` tag.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	emit := func(name string, v any) bool {
		raw, err := json.Marshal(v)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	sendStatus := func() bool { return emit("status", s.status()) }

	// Nil channels block forever in select, which is exactly the wanted
	// behavior when a source isn't wired.
	var evCh <-chan events.Event
	if s.deps.Events != nil {
		ch, cancel := s.deps.Events.Subscribe()
		defer cancel()
		evCh = ch
	}
	var logCh <-chan logging.Entry
	if q := r.URL.Query().Get("logs"); q == "1" || q == "true" {
		if s.deps.LogRing != nil {
			ch, cancel := s.deps.LogRing.Subscribe()
			defer cancel()
			logCh = ch
		}
	}

	if !sendStatus() {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !sendStatus() {
				return
			}
		case ev := <-evCh:
			// Governor transitions become an immediate fresh snapshot —
			// clients already know how to read status, and the snapshot
			// carries the new state plus everything it affects.
			if ev.Type == "state_change" {
				if !sendStatus() {
					return
				}
				continue
			}
			if !emit(ev.Type, ev.Data) {
				return
			}
		case e := <-logCh:
			if !emit("log", logEntryToGen(e)) {
				return
			}
		}
	}
}

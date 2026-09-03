package localapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/assign"
	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/logging"
	"github.com/teraflock/flockd/internal/models"
	"github.com/teraflock/flockd/internal/telemetry"
	"github.com/teraflock/flockd/internal/update"
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
	hw := s.deps.Hardware
	if hw != nil {
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
		resp.Memory.TotalMb = int64(hw.GetRamTotalMb())
	}
	if ops := s.deps.ModelOps; ops != nil {
		m := ops.Memory()
		resp.Memory.UsedMb, resp.Memory.BudgetMb = m.UsedMB, m.BudgetMB
	}
	if mgr := s.deps.Models; mgr != nil {
		d := mgr.Stats()
		resp.Disk = gen.Disk{ModelsBytes: d.ModelsBytes, PartialBytes: d.PartialBytes,
			BudgetBytes: d.BudgetBytes, FreeBytes: d.FreeBytes, Dir: d.Dir}
	}
	if u := s.deps.Update; u != nil {
		if last, ok := u.Last(); ok {
			resp.Update = updateToGen(last)
		}
	}
	return resp
}

func updateToGen(r update.Result) *gen.Update {
	out := &gen.Update{Available: r.Available, Current: r.Current, Latest: r.Latest, CheckedAt: r.CheckedAt}
	if r.Minimum != "" {
		m := r.Minimum
		out.Minimum = &m
	}
	if r.BelowMinimum {
		b := true
		out.BelowMinimum = &b
	}
	if r.URL != "" {
		u := r.URL
		out.Url = &u
	}
	return out
}

// GetActivity implements gen.ServerInterface.
func (s *Server) GetActivity(w http.ResponseWriter, r *http.Request) {
	rows := []gen.ActivityEvent{}
	for _, e := range s.deps.Activity.List() {
		row := gen.ActivityEvent{Time: e.Time, Kind: e.Kind, Actor: e.Actor, Message: e.Message}
		if e.Model != "" {
			m := e.Model
			row.Model = &m
		}
		if e.Detail != "" {
			d := e.Detail
			row.Detail = &d
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, gen.ActivityList{Events: rows})
}

// CheckUpdate implements gen.ServerInterface: an on-demand version check.
// A feed that is unreachable (or not built yet) is a 502 here because the
// operator asked explicitly; the background checks stay silent.
func (s *Server) CheckUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "update checks are not enabled")
		return
	}
	res, err := s.deps.Update.Check(r.Context())
	if err != nil {
		if errors.Is(err, update.ErrFeedUnavailable) {
			writeOpenAIError(w, http.StatusBadGateway, "server_error", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "version check failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updateToGen(res))
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

	// Coordinator placements: attached to their cache row, or shown as a
	// row of their own while still queued (nothing on disk yet).
	assignments := map[string]assign.Pending{}
	if s.deps.Assign != nil {
		for _, p := range s.deps.Assign.Pending() {
			assignments[p.ID] = p
		}
	}
	var loadedMB map[string]int64
	if s.deps.ModelOps != nil {
		loadedMB = s.deps.ModelOps.Memory().Models
	}
	decorate := func(row *gen.ModelRow) {
		if !row.Loaded {
			return
		}
		if mb, ok := loadedMB[row.Id]; ok && mb > 0 {
			row.LoadedMb = &mb
		}
		if u, ok := s.deps.Engine.Usage(row.Id); ok && u.Inflight == 0 {
			t := u.LastUsed
			row.IdleSince = &t
		}
	}
	rows := []gen.ModelRow{}
	if s.deps.Models != nil {
		for _, i := range s.deps.Models.List() {
			row := gen.ModelRow{
				Id: i.ID, SizeBytes: i.SizeBytes, Pinned: i.Pinned,
				LastUsed: i.LastUsed, State: i.State, Origin: i.Origin,
				Loaded: loaded[i.ID], Default: i.ID == def,
			}
			if i.Path != "" {
				p := i.Path
				row.Path = &p
			}
			decorate(&row)
			if i.State == "downloading" {
				rb := i.ReceivedBytes
				row.ReceivedBytes = &rb
			}
			if p, ok := assignments[i.ID]; ok {
				row.Assignment = assignmentToGen(p)
				delete(assignments, i.ID)
			}
			rows = append(rows, row)
			delete(loaded, i.ID)
		}
	}
	for id := range loaded { // loaded but not cached (mock runtime)
		row := gen.ModelRow{Id: id, State: "ready", Loaded: true, Default: id == def, Origin: models.OriginOperator}
		decorate(&row)
		rows = append(rows, row)
		delete(assignments, id)
	}
	for _, p := range assignments {
		if p.State == assign.StateEvicted {
			continue // gone; nothing to show
		}
		state := "assigned"
		if p.State != assign.StateAssigned && p.State != assign.StateDownloading {
			state = p.State // declined/failed: visible, actionable
		}
		rows = append(rows, gen.ModelRow{
			Id: p.ID, State: state, Origin: models.OriginMesh, LastUsed: p.Since,
			Assignment: assignmentToGen(p),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Id < rows[j].Id })
	writeJSON(w, http.StatusOK, gen.ModelList{Models: rows})
}

func assignmentToGen(p assign.Pending) *gen.Assignment {
	a := &gen.Assignment{State: p.State, Since: p.Since}
	if p.Error != "" {
		e := p.Error
		a.Error = &e
	}
	return a
}

// meshManaged reads the live toggle (defaults on when not switchable).
func (s *Server) meshManaged() bool {
	if s.deps.MeshManaged == nil {
		return true
	}
	return s.deps.MeshManaged()
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
	var err error
	if s.deps.ModelOps != nil {
		// Unload + delete without a `cached` report in between.
		err = s.deps.ModelOps.Remove(r.Context(), id)
	} else {
		if entry := s.deps.Engine.Unregister(id); entry != nil {
			_ = entry.Instance.Shutdown(r.Context())
		}
		err = s.deps.Models.Remove(id)
	}
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	s.deps.Activity.Record(activity.KindEvicted, activity.ActorOperator, id, "operator removed "+id, "")
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
	mm := s.meshManaged()
	ll := s.liveLimits()
	writeJSON(w, http.StatusOK, gen.Limits{
		ServePolicy:       p.Serve,
		IdleAfterSeconds:  int(p.IdleAfter.Seconds()),
		YieldGraceSeconds: int(p.YieldGrace.Seconds()),
		ServeOnBattery:    p.ServeOnBattery,
		MaxTempCelsius:    p.MaxTempCelsius,
		Schedule:          windowsToStrings(p.Schedule),
		MeshManaged:       &mm,
		MaxDiskMb:         &ll.MaxDiskMB,
		RetentionDays:     &ll.RetentionDays,
		MaxRamMb:          &ll.MaxRAMMB,
		IdleUnloadSeconds: &ll.IdleUnloadS,
	})
}

// liveLimits reads the model/budget knobs from their live owners, falling
// back to the configured defaults where an owner is absent.
func (s *Server) liveLimits() config.LiveLimits {
	ll := s.deps.Defaults
	ll.MeshManaged = s.meshManaged()
	if m := s.deps.Models; m != nil {
		ll.MaxDiskMB = m.MaxDiskMB()
		ll.RetentionDays = m.RetentionDays()
	}
	if ops := s.deps.ModelOps; ops != nil {
		ll.MaxRAMMB = ops.ConfiguredMemoryBudgetMB()
		ll.IdleUnloadS = int(ops.IdleUnload().Seconds())
	}
	return ll
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
	if lim.MeshManaged != nil && s.deps.SetMeshManaged != nil {
		s.deps.SetMeshManaged(*lim.MeshManaged)
	}
	for _, v := range []*int64{lim.MaxDiskMb, lim.MaxRamMb} {
		if v != nil && *v < 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "max_disk_mb and max_ram_mb must be >= 0")
			return
		}
	}
	for _, v := range []*int{lim.RetentionDays, lim.IdleUnloadSeconds} {
		if v != nil && *v < 0 {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "retention_days and idle_unload_seconds must be >= 0")
			return
		}
	}
	if m := s.deps.Models; m != nil {
		if lim.MaxDiskMb != nil {
			m.SetMaxDiskMB(*lim.MaxDiskMb)
		}
		if lim.RetentionDays != nil {
			m.SetRetentionDays(*lim.RetentionDays)
		}
	}
	if ops := s.deps.ModelOps; ops != nil {
		if lim.MaxRamMb != nil {
			ops.SetMemoryBudgetMB(*lim.MaxRamMb)
		}
		if lim.IdleUnloadSeconds != nil {
			ops.SetIdleUnload(time.Duration(*lim.IdleUnloadSeconds) * time.Second)
		}
	}
	// No live owner (mock runtime): remember the values for persistence.
	if s.deps.Models == nil {
		if lim.MaxDiskMb != nil {
			s.deps.Defaults.MaxDiskMB = *lim.MaxDiskMb
		}
		if lim.RetentionDays != nil {
			s.deps.Defaults.RetentionDays = *lim.RetentionDays
		}
	}
	if s.deps.ModelOps == nil {
		if lim.MaxRamMb != nil {
			s.deps.Defaults.MaxRAMMB = *lim.MaxRamMb
		}
		if lim.IdleUnloadSeconds != nil {
			s.deps.Defaults.IdleUnloadS = *lim.IdleUnloadSeconds
		}
	}
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
		}, s.liveLimits())
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

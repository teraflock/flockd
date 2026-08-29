package localapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/telemetry"
)

// StatusResponse is GET /api/v1/status (also streamed on /api/v1/events).
type StatusResponse struct {
	NodeID        string             `json:"node_id"`
	Version       string             `json:"version"`
	Standalone    bool               `json:"standalone"`
	State         string             `json:"state"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	DefaultModel  string             `json:"default_model"`
	ModelsLoaded  int                `json:"models_loaded"`
	Inflight      int                `json:"inflight"`
	OnBattery     bool               `json:"on_battery"`
	TempCelsius   float64            `json:"temp_celsius"`
	Hardware      *HardwareSummary   `json:"hardware,omitempty"`
	Stats         telemetry.Snapshot `json:"stats"`
}

type HardwareSummary struct {
	OS       string       `json:"os"`
	Arch     string       `json:"arch"`
	CPUModel string       `json:"cpu_model"`
	CPUCores uint32       `json:"cpu_cores"`
	RAMMB    uint64       `json:"ram_mb"`
	GPUs     []GPUSummary `json:"gpus"`
}

type GPUSummary struct {
	Vendor  string `json:"vendor"`
	Model   string `json:"model"`
	VRAMMB  uint64 `json:"vram_mb"`
	Accel   string `json:"accel"`
	Unified bool   `json:"unified_memory"`
}

func (s *Server) status() StatusResponse {
	resp := StatusResponse{
		NodeID:        s.deps.NodeID,
		Version:       s.deps.Version,
		Standalone:    s.deps.Standalone,
		State:         "serving",
		UptimeSeconds: int64(time.Since(s.start).Seconds()),
		DefaultModel:  s.deps.Engine.DefaultModel(),
		ModelsLoaded:  len(s.deps.Engine.Models()),
		Stats:         s.deps.Engine.Stats().Snapshot(),
	}
	if g := s.deps.Governor; g != nil {
		resp.State = g.State().String()
		resp.Inflight = g.Inflight()
		p := g.Power()
		resp.OnBattery = p.OnBattery
		resp.TempCelsius = p.TempCelsius
	}
	if hw := s.deps.Hardware; hw != nil {
		hs := &HardwareSummary{
			OS:       hw.GetOs(),
			Arch:     hw.GetArch(),
			CPUModel: hw.GetCpuModel(),
			CPUCores: hw.GetCpuCores(),
			RAMMB:    hw.GetRamTotalMb(),
		}
		for _, g := range hw.GetGpus() {
			hs.GPUs = append(hs.GPUs, GPUSummary{
				Vendor: g.GetVendor(), Model: g.GetModel(), VRAMMB: g.GetVramMb(),
				Accel: g.GetAccel(), Unified: g.GetUnifiedMemory(),
			})
		}
		resp.Hardware = hs
	}
	return resp
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status())
}

// ---- models ----

// ModelRow merges cache state with loaded state.
type ModelRow struct {
	ID        string    `json:"id"`
	SizeBytes int64     `json:"size_bytes"`
	Pinned    bool      `json:"pinned"`
	LastUsed  time.Time `json:"last_used"`
	State     string    `json:"state"`
	Loaded    bool      `json:"loaded"`
	Default   bool      `json:"default"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	loaded := map[string]bool{}
	for _, m := range s.deps.Engine.Models() {
		loaded[m.Spec.ID] = true
	}
	def := s.deps.Engine.DefaultModel()

	var rows []ModelRow
	if s.deps.Models != nil {
		for _, i := range s.deps.Models.List() {
			rows = append(rows, ModelRow{
				ID: i.ID, SizeBytes: i.SizeBytes, Pinned: i.Pinned,
				LastUsed: i.LastUsed, State: i.State,
				Loaded: loaded[i.ID], Default: i.ID == def,
			})
			delete(loaded, i.ID)
		}
	}
	for id := range loaded { // loaded but not cached (mock runtime)
		rows = append(rows, ModelRow{ID: id, State: "ready", Loaded: true, Default: id == def})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": rows})
}

func (s *Server) handleModelPin(w http.ResponseWriter, r *http.Request) {
	if s.deps.Models == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "model cache not enabled (mock runtime)")
		return
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid body")
		return
	}
	if err := s.deps.Models.Pin(r.PathValue("id"), body.Pinned); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleModelRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Models == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "model cache not enabled (mock runtime)")
		return
	}
	id := r.PathValue("id")
	if entry := s.deps.Engine.Unregister(id); entry != nil {
		_ = entry.Instance.Shutdown(r.Context())
	}
	if err := s.deps.Models.Remove(id); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- earnings ----

// EarningsResponse is standalone/demo accounting until the ledger exists
// (Phase 2). Numbers are honest simulations, labelled as such.
type EarningsResponse struct {
	EarnedMicrocredits int64   `json:"earned_microcredits"`
	EarnedCredits      float64 `json:"earned_credits"`
	EstUSD             float64 `json:"est_usd"`
	EstUSDPerDay       float64 `json:"est_usd_per_day"`
	LifetimeTokens     int64   `json:"lifetime_tokens"`
	Escrow             float64 `json:"escrow_credits"`
	Note               string  `json:"note"`
}

func (s *Server) handleEarnings(w http.ResponseWriter, r *http.Request) {
	snap := s.deps.Engine.Stats().Snapshot()
	credits := float64(snap.EarnedMicrocred) / 1e6
	usd := credits * 0.000001 * 1e6 // 1 credit = $0.000001 peg (SPEC §4.5)
	uptime := time.Since(s.start).Hours()
	perDay := 0.0
	if uptime > 0 {
		perDay = usd / uptime * 24
	}
	writeJSON(w, http.StatusOK, EarningsResponse{
		EarnedMicrocredits: snap.EarnedMicrocred,
		EarnedCredits:      credits,
		EstUSD:             usd,
		EstUSDPerDay:       perDay,
		LifetimeTokens:     snap.TotalTokens,
		Note:               "simulated standalone earnings; real accrual starts when the node is enrolled with a coordinator (Phase 2 ledger)",
	})
}

// ---- limits ----

// Limits mirrors the governor policy knobs exposed to operators.
type Limits struct {
	ServePolicy    string   `json:"serve_policy"`
	IdleAfterSec   int      `json:"idle_after_seconds"`
	YieldGraceSec  int      `json:"yield_grace_seconds"`
	ServeOnBattery bool     `json:"serve_on_battery"`
	MaxTempCelsius float64  `json:"max_temp_celsius"`
	Schedule       []string `json:"schedule"`
}

func (s *Server) handleLimitsGet(w http.ResponseWriter, r *http.Request) {
	g := s.deps.Governor
	if g == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "governor not enabled")
		return
	}
	p := g.Policy()
	lim := Limits{
		ServePolicy:    p.Serve,
		IdleAfterSec:   int(p.IdleAfter.Seconds()),
		YieldGraceSec:  int(p.YieldGrace.Seconds()),
		ServeOnBattery: p.ServeOnBattery,
		MaxTempCelsius: p.MaxTempCelsius,
		Schedule:       windowsToStrings(p.Schedule),
	}
	writeJSON(w, http.StatusOK, lim)
}

func (s *Server) handleLimitsPut(w http.ResponseWriter, r *http.Request) {
	g := s.deps.Governor
	if g == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "governor not enabled")
		return
	}
	var lim Limits
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
	if lim.IdleAfterSec > 0 {
		p.IdleAfter = time.Duration(lim.IdleAfterSec) * time.Second
	}
	if lim.YieldGraceSec > 0 {
		p.YieldGrace = time.Duration(lim.YieldGraceSec) * time.Second
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
	s.handleLimitsGet(w, r)
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

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	n := 200
	q := r.URL.Query().Get("n")
	if q == "" {
		q = r.URL.Query().Get("limit") // alias; both are natural to reach for
	}
	if q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 && v <= 1024 {
			n = v
		}
	}
	if s.deps.LogRing == nil {
		writeJSON(w, http.StatusOK, map[string]any{"logs": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.deps.LogRing.Tail(n)})
}

// ---- SSE events ----

// handleEvents streams a status snapshot every 2 seconds — the live tok/s
// ticker behind the TUI and web dashboard.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	send := func() bool {
		raw, err := json.Marshal(s.status())
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", raw); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send() {
		return
	}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
			if !send() {
				return
			}
		}
	}
}

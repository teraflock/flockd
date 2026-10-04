package localapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/memory"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
)

// The limits API (GET/PUT /api/v1/limits) is how the desktop app, the
// dashboard and `tera limits` change the operator's settings: the app
// never writes config.toml. flockd#55 widened it from the governor, store
// and memory knobs to every setting that is safe to change from an app.
//
// What stays file-only, and why, is the table in docs/config.md
// ("Settings and the limits API"): anything that can strand the node
// (coordinator address, TLS, data dir, the local API's bind address),
// weaken what it trusts (catalog and runtime sources, signatures) or is
// plumbing with no operator value.

// SettingsValues are the settings added by flockd#55, as plain values.
type SettingsValues struct {
	MaxConcurrent  int // budget.max_concurrent, 0 = auto
	MinConcurrent  int // budget.min_concurrent, 0 = auto
	MaxVRAMPercent int
	MaxContext     int
	MinContext     int
	ContextLength  int
	Exclude        []string
	DefaultModel   string // "" = none
	RequireAuthV1  bool
	LogLevel       string
}

func (v SettingsValues) normalized() SettingsValues {
	if v.MaxVRAMPercent == 0 {
		v.MaxVRAMPercent = config.Default().Budget.MaxVRAMPercent
	}
	if v.LogLevel == "" {
		v.LogLevel = "info"
	}
	v.LogLevel = strings.ToLower(v.LogLevel)
	if v.Exclude == nil {
		v.Exclude = []string{}
	}
	v.DefaultModel = config.NormalizeDefaultModel(v.DefaultModel)
	return v
}

// SettingsDeps is what the daemon hands the limits API for them.
type SettingsDeps struct {
	// Boot is what the daemon started with: the running value of the
	// settings that only a restart applies.
	Boot SettingsValues
	// Overlay is the keys limits.toml already held at startup; a save
	// carries them forward and adds whatever a PUT sets.
	Overlay config.LimitsExtra
	// ApplyPlan applies the slot ceiling and context limits to a running
	// daemon: later loads, the mesh dispatch cap, and a reload of idle
	// loaded models. Nil = nothing to apply (they are still saved).
	ApplyPlan func(modelops.PlanLimits)
	// SetExclude replaces models.exclude for later placements. May be nil.
	SetExclude func([]string)
	// SetLogLevel changes the daemon's log level. Nil = the level is only
	// saved (and reported as pending a restart).
	SetLogLevel func(string)
}

func planLimitsOf(v SettingsValues) modelops.PlanLimits {
	return modelops.PlanLimits{MaxConcurrent: v.MaxConcurrent, MinConcurrent: v.MinConcurrent, MaxContext: v.MaxContext,
		MinContext: v.MinContext, ContextLength: v.ContextLength}
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

// limits assembles the Limits object, metadata included.
func (s *Server) limits() gen.Limits {
	p := s.deps.Governor.Policy()
	mm := s.meshManaged()
	ll := s.liveLimits()
	s.setMu.Lock()
	cur := s.cur
	cur.Exclude = slices.Clone(cur.Exclude)
	s.setMu.Unlock()
	lvl := gen.LimitsLogLevel(cur.LogLevel)
	lim := gen.Limits{
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
		MaxConcurrent:     &cur.MaxConcurrent,
		MinConcurrent:     &cur.MinConcurrent,
		MaxContext:        &cur.MaxContext,
		MinContext:        &cur.MinContext,
		ContextLength:     &cur.ContextLength,
		MaxVramPercent:    &cur.MaxVRAMPercent,
		Exclude:           &cur.Exclude,
		DefaultModel:      &cur.DefaultModel,
		RequireAuthV1:     &cur.RequireAuthV1,
		LogLevel:          &lvl,
	}
	settings := s.settingsMeta(lim, cur)
	lim.Settings = &settings
	return lim
}

// GetLimits implements gen.ServerInterface.
func (s *Server) GetLimits(w http.ResponseWriter, _ *http.Request) {
	if s.deps.Governor == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "governor not enabled")
		return
	}
	writeJSON(w, http.StatusOK, s.limits())
}

// validateLimits checks a whole PUT before anything is applied, so a bad
// field never leaves the node half-changed. It returns the schedule
// windows and the settings as they would be after the PUT.
func (s *Server) validateLimits(lim gen.Limits, cur SettingsValues) ([]governor.Window, SettingsValues, error) {
	switch lim.ServePolicy {
	case "always", "idle-only", "scheduled":
	default:
		return nil, cur, fmt.Errorf("serve_policy must be always|idle-only|scheduled")
	}
	windows, err := governor.ParseWindows(lim.Schedule)
	if err != nil {
		return nil, cur, err
	}
	if lim.IdleAfterSeconds < 0 || lim.YieldGraceSeconds < 0 {
		return nil, cur, fmt.Errorf("idle_after_seconds and yield_grace_seconds must be >= 0 (0 = unchanged)")
	}
	if lim.MaxTempCelsius < 0 || lim.MaxTempCelsius > 110 {
		return nil, cur, fmt.Errorf("max_temp_celsius must be 0 (off) to 110")
	}
	for _, v := range []*int64{lim.MaxDiskMb, lim.MaxRamMb} {
		if v != nil && *v < 0 {
			return nil, cur, fmt.Errorf("max_disk_mb and max_ram_mb must be >= 0")
		}
	}
	for _, v := range []*int{lim.RetentionDays, lim.IdleUnloadSeconds} {
		if v != nil && *v < 0 {
			return nil, cur, fmt.Errorf("retention_days and idle_unload_seconds must be >= 0")
		}
	}

	next := cur
	if v := lim.MaxConcurrent; v != nil {
		if *v < 0 || *v > config.MaxConcurrentLimit {
			return nil, cur, fmt.Errorf("max_concurrent must be 0 (auto) to %d", config.MaxConcurrentLimit)
		}
		next.MaxConcurrent = *v
	}
	if v := lim.MinConcurrent; v != nil {
		if *v < 0 || *v > config.MaxConcurrentLimit {
			return nil, cur, fmt.Errorf("min_concurrent must be 0 (auto) to %d", config.MaxConcurrentLimit)
		}
		next.MinConcurrent = *v
	}
	ctxField := func(name string, v *int, floor int, dst *int) error {
		if v == nil {
			return nil
		}
		if *v != 0 && (*v < floor || *v > config.MaxContextLimit) {
			return fmt.Errorf("%s must be 0 or %d to %d tokens", name, floor, config.MaxContextLimit)
		}
		*dst = *v
		return nil
	}
	if err := ctxField("max_context", lim.MaxContext, 1024, &next.MaxContext); err != nil {
		return nil, cur, err
	}
	if err := ctxField("min_context", lim.MinContext, config.MinContextFloor, &next.MinContext); err != nil {
		return nil, cur, err
	}
	if err := ctxField("context_length", lim.ContextLength, config.MinContextFloor, &next.ContextLength); err != nil {
		return nil, cur, err
	}
	if next.MaxContext > 0 && next.MinContext > next.MaxContext {
		return nil, cur, fmt.Errorf("min_context (%d) must not be above max_context (%d)", next.MinContext, next.MaxContext)
	}
	if v := lim.MaxVramPercent; v != nil {
		if *v < 1 || *v > 100 {
			return nil, cur, fmt.Errorf("max_vram_percent must be 1 to 100")
		}
		next.MaxVRAMPercent = *v
	}
	if v := lim.Exclude; v != nil {
		if len(*v) > config.MaxExcludeEntries {
			return nil, cur, fmt.Errorf("exclude holds at most %d model ids", config.MaxExcludeEntries)
		}
		out := make([]string, 0, len(*v))
		for _, id := range *v {
			id = strings.TrimSpace(id)
			if err := models.ValidateID(id); err != nil {
				return nil, cur, fmt.Errorf("exclude: %q is not a model id", id)
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		next.Exclude = out
	}
	if v := lim.DefaultModel; v != nil {
		id := config.NormalizeDefaultModel(strings.TrimSpace(*v))
		if id != "" {
			if err := models.ValidateID(id); err != nil {
				return nil, cur, fmt.Errorf("default_model: %q is not a model id (empty or \"none\" = load nothing)", id)
			}
		}
		// Only a chat model can be the default (the engine's rule); say
		// so now for a model this node knows is not one.
		if id != "" && id != cur.DefaultModel {
			if kind := s.nonChatKind(id); kind != "" {
				return nil, cur, fmt.Errorf("default_model: %q is %s and cannot be the default, which answers chat requests that name no model", id, kind)
			}
		}
		next.DefaultModel = id
	}
	if v := lim.RequireAuthV1; v != nil {
		next.RequireAuthV1 = *v
	}
	if v := lim.LogLevel; v != nil {
		lvl := strings.ToLower(string(*v))
		if !slices.Contains(config.LogLevels, lvl) {
			return nil, cur, fmt.Errorf("log_level must be debug|info|warn|error")
		}
		next.LogLevel = lvl
	}
	return windows, next, nil
}

// nonChatKind names why id cannot be the default ("a decision model",
// "an embedding model"), going by the loaded model or the catalog; ""
// when it is a chat model or unknown here.
func (s *Server) nonChatKind(id string) string {
	kind := func(decision, embeddings bool) string {
		switch {
		case decision:
			return "a decision model"
		case embeddings:
			return "an embedding model"
		}
		return ""
	}
	for _, m := range s.deps.Engine.Models() {
		if m.Spec.ID == id {
			return kind(m.Spec.Decision, m.Spec.Embeddings)
		}
	}
	if ops := s.deps.ModelOps; ops != nil {
		if cat, err := ops.Catalog(context.Background(), false); err == nil {
			if e, ok := cat.Find(id); ok {
				return kind(e.Decision, e.Embeddings)
			}
		}
	}
	return ""
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
	s.setMu.Lock()
	prev := s.cur
	windows, next, err := s.validateLimits(lim, prev)
	if err != nil {
		s.setMu.Unlock()
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// A field a PUT changes is the operator's from now on: it goes into
	// the overlay. Fields never changed through the API stay with
	// config.toml — a client that reads the object and writes it all back
	// (the app saving one toggle) must not freeze every other value.
	if next.MaxConcurrent != prev.MaxConcurrent {
		s.overlay.MaxConcurrent = &next.MaxConcurrent
	}
	if next.MinConcurrent != prev.MinConcurrent {
		s.overlay.MinConcurrent = &next.MinConcurrent
	}
	if next.MaxContext != prev.MaxContext {
		s.overlay.MaxContext = &next.MaxContext
	}
	if next.MinContext != prev.MinContext {
		s.overlay.MinContext = &next.MinContext
	}
	if next.ContextLength != prev.ContextLength {
		s.overlay.ContextLength = &next.ContextLength
	}
	if next.MaxVRAMPercent != prev.MaxVRAMPercent {
		s.overlay.MaxVRAMPercent = &next.MaxVRAMPercent
	}
	if !slices.Equal(next.Exclude, prev.Exclude) {
		ex := slices.Clone(next.Exclude)
		s.overlay.Exclude = &ex
	}
	if next.DefaultModel != prev.DefaultModel {
		s.overlay.DefaultModel = &next.DefaultModel
	}
	if next.RequireAuthV1 != prev.RequireAuthV1 {
		s.overlay.RequireAuthV1 = &next.RequireAuthV1
	}
	if next.LogLevel != prev.LogLevel {
		s.overlay.LogLevel = &next.LogLevel
	}
	s.cur = next
	overlay := s.overlay
	s.setMu.Unlock()

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
	// The flockd#55 settings: live ones now, plan limits by reloading
	// idle models, the rest at the next start (they are only saved).
	s.requireAuthV1.Store(next.RequireAuthV1)
	if next.LogLevel != prev.LogLevel && s.deps.Settings.SetLogLevel != nil {
		s.deps.Settings.SetLogLevel(next.LogLevel)
	}
	if !slices.Equal(prev.Exclude, next.Exclude) && s.deps.Settings.SetExclude != nil {
		s.deps.Settings.SetExclude(slices.Clone(next.Exclude))
	}
	if planLimitsOf(prev) != planLimitsOf(next) && s.deps.Settings.ApplyPlan != nil {
		s.deps.Settings.ApplyPlan(planLimitsOf(next))
	}

	// The default model applies now as well as at the next start: make it
	// the default if it is loaded, load it first if it is only on disk.
	if next.DefaultModel != prev.DefaultModel && next.DefaultModel != "" {
		s.applyDefaultModel(next.DefaultModel)
	}
	s.saveOverlay(overlay)
	writeJSON(w, http.StatusOK, s.limits())
}

// saveOverlay persists the settings to the daemon-owned overlay (never the
// operator's config.toml) so they survive restarts. A write failure loses
// only persistence, not the live change — it is logged.
func (s *Server) saveOverlay(overlay config.LimitsExtra) {
	g := s.deps.Governor
	if s.deps.DataDir == "" || g == nil {
		return
	}
	p := g.Policy()
	err := config.SaveLimits(s.deps.DataDir, config.Governor{
		ServePolicy:    p.Serve,
		IdleAfter:      p.IdleAfter,
		YieldGrace:     p.YieldGrace,
		ServeOnBattery: p.ServeOnBattery,
		MaxTempCelsius: p.MaxTempCelsius,
		Schedule:       windowsToStrings(p.Schedule),
	}, s.liveLimits(), overlay)
	if err != nil {
		s.deps.Log.Warn("settings applied but not persisted", "err", err)
	}
}

// rememberDefault records the operator's choice of default model (make
// default, or default_model through the limits API) so the next start
// loads the same model: models.default in limits.toml.
func (s *Server) rememberDefault(id string) {
	s.setMu.Lock()
	s.cur.DefaultModel = id
	s.overlay.DefaultModel = &id
	overlay := s.overlay
	s.setMu.Unlock()
	s.saveOverlay(overlay)
}

// applyDefaultModel makes id the running default: at once when it is
// loaded, after loading it when it is on disk. Anything else (not
// installed yet) waits for the next start, which loads it.
func (s *Server) applyDefaultModel(id string) {
	for _, m := range s.deps.Engine.Models() {
		if m.Spec.ID == id {
			if err := s.deps.Engine.SetDefault(id); err != nil {
				s.deps.Log.Warn("default model not switched", "model", id, "err", err)
			}
			return
		}
	}
	ops := s.deps.ModelOps
	if ops == nil || s.deps.Models == nil || !s.deps.Models.Has(id) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := ops.Load(ctx, id); err != nil {
			s.deps.Log.Warn("default model not loaded; it will be loaded at the next start", "model", id, "err", err)
			return
		}
		if err := ops.SetDefault(id); err != nil {
			s.deps.Log.Warn("default model not switched", "model", id, "err", err)
		}
	}()
}

// ---- metadata ----

func strPtr(s string) *string { return &s }
func f32(v float64) *float32  { f := float32(v); return &f }

// settingsMeta is the `settings` array: one entry per Limits field, in
// display order (common first). Titles and descriptions are the help text
// an app shows; api/openapi.yaml documents the same fields for readers of
// the spec, and TestSettingsCoverEveryLimitsField keeps the two lists in
// step.
func (s *Server) settingsMeta(lim gen.Limits, cur SettingsValues) []gen.LimitSetting {
	def := config.Default()
	boot := s.deps.Settings.Boot
	hw := s.deps.Hardware
	autoSlots := memory.DefaultSlots(hw)

	type opt func(*gen.LimitSetting)
	unit := func(u string) opt { return func(m *gen.LimitSetting) { m.Unit = strPtr(u) } }
	rng := func(lo, hi float64) opt {
		return func(m *gen.LimitSetting) {
			m.Min = f32(lo)
			if hi > 0 {
				m.Max = f32(hi)
			}
		}
	}
	zero := func(z string) opt { return func(m *gen.LimitSetting) { m.ZeroMeans = strPtr(z) } }
	options := func(o ...string) opt { return func(m *gen.LimitSetting) { m.Options = &o } }
	effective := func(v any) opt { return func(m *gen.LimitSetting) { m.Effective = v } }
	// pending marks a restart setting whose saved value is not the one
	// the daemon is running with.
	pending := func(running any, differs bool) opt {
		return func(m *gen.LimitSetting) {
			if differs {
				m.Effective, m.PendingRestart = running, true
			}
		}
	}
	mk := func(key, cfgKey, title, desc string, tier gen.LimitSettingTier, apply gen.LimitSettingApply,
		typ gen.LimitSettingType, defv, configured any, opts ...opt) gen.LimitSetting {
		m := gen.LimitSetting{Key: key, ConfigKey: cfgKey, Title: title, Description: desc, Tier: tier,
			Apply: apply, Type: typ, Default: defv, Configured: configured}
		for _, o := range opts {
			o(&m)
		}
		return m
	}

	slotsOpts := []opt{unit("slots"), rng(0, config.MaxConcurrentLimit), zero("auto")}
	if cur.MaxConcurrent == 0 {
		slotsOpts = append(slotsOpts, effective(autoSlots))
	}
	ramOpts := []opt{unit("MB"), rng(0, 0), zero("auto")}
	if ops := s.deps.ModelOps; ops != nil && *lim.MaxRamMb == 0 {
		ramOpts = append(ramOpts, effective(ops.MemoryBudgetMB()))
	}
	// The floor in force: auto resolves to 2, and it never exceeds the
	// ceiling (at the ceiling the slot count is pinned).
	ceiling := cur.MaxConcurrent
	if ceiling == 0 {
		ceiling = autoSlots
	}
	floor := cur.MinConcurrent
	if floor == 0 {
		floor = modelops.DefaultFloorSlots
	}
	floor = min(floor, ceiling)
	floorOpts := []opt{unit("slots"), rng(0, config.MaxConcurrentLimit), zero("auto")}
	if floor != cur.MinConcurrent {
		floorOpts = append(floorOpts, effective(floor))
	}
	// log_level is live when the daemon wired a setter, else restart.
	logApply, logOpts := gen.Live, []opt{options(config.LogLevels...)}
	if s.deps.Settings.SetLogLevel == nil {
		logApply = gen.Restart
		logOpts = append(logOpts, pending(boot.LogLevel, boot.LogLevel != cur.LogLevel))
	}
	// The default in force: the engine's. It differs from the configured
	// one while that model is not loaded (still loading, not installed,
	// or "none" chosen with a default still running).
	var defaultOpts []opt
	if running := s.deps.Engine.DefaultModel(); running != cur.DefaultModel {
		defaultOpts = append(defaultOpts, effective(running))
	}
	minCtxOpts := []opt{unit("tokens"), rng(0, config.MaxContextLimit), zero("8192")}
	if cur.MinContext == 0 {
		minCtxOpts = append(minCtxOpts, effective(memory.DefaultMinContext))
	}

	return []gen.LimitSetting{
		// ---- common ----
		mk("serve_policy", "governor.serve_policy", "When to serve",
			"When this machine serves the mesh. idle-only: only while you are away from the keyboard and mouse. scheduled: only inside the schedule windows. always: whenever it is on.",
			gen.Common, gen.Live, gen.Enum, def.Governor.ServePolicy, lim.ServePolicy, options("always", "idle-only", "scheduled")),
		mk("schedule", "governor.schedule", "Schedule",
			"Daily serving windows for the scheduled policy, each \"HH:MM-HH:MM\" in local time; a start later than its end wraps overnight (\"22:00-08:00\"). An empty list never serves.",
			gen.Common, gen.Live, gen.StringList, []string{}, lim.Schedule),
		mk("idle_after_seconds", "governor.idle_after", "Idle after",
			"With idle-only, how long keyboard and mouse must be quiet before the node starts serving.",
			gen.Common, gen.Live, gen.Integer, int(def.Governor.IdleAfter.Seconds()), lim.IdleAfterSeconds, unit("seconds"), rng(1, 0)),
		mk("serve_on_battery", "governor.serve_on_battery", "Serve on battery",
			"Serve while running on battery power. Off, the node pauses when unplugged.",
			gen.Common, gen.Live, gen.Boolean, def.Governor.ServeOnBattery, lim.ServeOnBattery),
		mk("max_temp_celsius", "governor.max_temp_celsius", "Thermal limit",
			"Pause serving above this temperature. 0 turns the check off.",
			gen.Common, gen.Live, gen.Number, def.Governor.MaxTempCelsius, lim.MaxTempCelsius, unit("°C"), rng(0, 110), zero("off")),
		mk("mesh_managed", "models.mesh_managed", "Let the mesh place models",
			"Let the mesh download, load and later evict models on this node, inside the disk budget and never touching models you installed. Off, the node serves only what you installed, and earns only from those.",
			gen.Common, gen.Live, gen.Boolean, def.Models.MeshManaged, *lim.MeshManaged),
		mk("max_disk_mb", "models.max_disk_mb", "Disk budget for models",
			"How much disk the model store may use. Least recently used models that are not pinned are evicted to stay under it.",
			gen.Common, gen.Live, gen.Integer, def.Models.MaxDiskMB, *lim.MaxDiskMb, unit("MB"), rng(0, 0), zero("unlimited")),
		mk("max_ram_mb", "budget.max_ram_mb", "Memory budget for models",
			"How much memory loaded models may use together. Auto is about half of the machine's memory on Apple Silicon and the GPU share on discrete GPUs. A load that does not fit unloads idle models first.",
			gen.Common, gen.Live, gen.Integer, def.Budget.MaxRAMMB, *lim.MaxRamMb, ramOpts...),
		mk("max_concurrent", "budget.max_concurrent", "Concurrent requests per model",
			"The most requests one model serves at once: the ceiling on a model's slots and on mesh dispatches to this node. A chat model starts with fewer slots and grows toward this ceiling while requests are waiting; every slot holds its own context in memory, so a lower ceiling leaves room for more models. Auto is 16 with a GPU or Apple Silicon and 2 on CPU.",
			gen.Common, gen.Reload, gen.Integer, def.Budget.MaxConcurrent, cur.MaxConcurrent, slotsOpts...),
		mk("idle_unload_seconds", "models.idle_unload_s", "Unload idle models after",
			"Unload a loaded model after this long without a request, freeing its memory; it is loaded again on demand in seconds. The default model is exempt.",
			gen.Common, gen.Live, gen.Integer, def.Models.IdleUnloadS, *lim.IdleUnloadSeconds, unit("seconds"), rng(0, 0), zero("never")),

		// ---- advanced ----
		mk("yield_grace_seconds", "governor.yield_grace", "Yield grace",
			"When you come back to the machine, how long a request in flight may run on before it is cancelled.",
			gen.Advanced, gen.Live, gen.Integer, int(def.Governor.YieldGrace.Seconds()), lim.YieldGraceSeconds, unit("seconds"), rng(1, 0)),
		mk("retention_days", "models.retention_days", "Remove unused models after",
			"Evict models that are not pinned and have not been used for this many days.",
			gen.Advanced, gen.Live, gen.Integer, def.Models.RetentionDays, *lim.RetentionDays, unit("days"), rng(0, 0), zero("never")),
		mk("min_concurrent", "budget.min_concurrent", "Slots a model starts with",
			"How many requests a chat model can serve at once when it is loaded. The node adds slots, up to the concurrent-requests ceiling, while requests are waiting, and gives them back when demand is gone or another model needs the memory. Set it to the ceiling to pin a fixed slot count.",
			gen.Advanced, gen.Reload, gen.Integer, def.Budget.MinConcurrent, cur.MinConcurrent, floorOpts...),
		mk("max_context", "runtime.max_context", "Largest context per request",
			"The largest context, in tokens, a single request gets. Memory per model grows with slots x context, so a lower cap leaves room for more slots or more models. 0 uses each model's own window.",
			gen.Advanced, gen.Reload, gen.Integer, def.Runtime.MaxContext, cur.MaxContext, unit("tokens"), rng(0, config.MaxContextLimit), zero("model window")),
		mk("min_context", "runtime.min_context", "Smallest context per request",
			"The least context a request is given: when memory is short the node gives up slots before going below it. Must not be above the largest context.",
			gen.Advanced, gen.Reload, gen.Integer, def.Runtime.MinContext, cur.MinContext, minCtxOpts...),
		mk("context_length", "runtime.context_length", "Pin context exactly",
			"Give every request exactly this many tokens of context, on every model, decision models included, instead of planning it from memory. Leave at 0 unless you know the workload.",
			gen.Advanced, gen.Reload, gen.Integer, def.Runtime.ContextLength, cur.ContextLength, unit("tokens"), rng(0, config.MaxContextLimit), zero("planned")),
		mk("max_vram_percent", "budget.max_vram_percent", "GPU memory share",
			"The share of GPU memory models may use: it sizes the automatic memory budget on discrete GPUs and how many layers are offloaded.",
			gen.Advanced, gen.Restart, gen.Integer, def.Budget.MaxVRAMPercent, cur.MaxVRAMPercent, unit("percent"), rng(1, 100),
			pending(boot.MaxVRAMPercent, boot.MaxVRAMPercent != cur.MaxVRAMPercent)),
		mk("exclude", "models.exclude", "Models the mesh may not place",
			"Model ids the mesh is never allowed to place on this node. A model already placed stays until it is evicted.",
			gen.Advanced, gen.Live, gen.StringList, []string{}, cur.Exclude),
		mk("default_model", "models.default", "Default model",
			"The node's default model: it answers requests that name no model, and it is the model the daemon loads when it starts. Choosing one makes it the default right away (it is loaded first if it is on disk) and is remembered across restarts; \"Make default\" on a model sets the same value. Must be a chat model. Empty (or \"none\") loads nothing at start and serves what the mesh places.",
			gen.Advanced, gen.Live, gen.String, def.Models.Default, cur.DefaultModel, defaultOpts...),
		mk("require_auth_v1", "local_api.require_auth_v1", "Require the token for local inference",
			"Require the bearer token on the local inference routes (/v1/*) too. Off, any program on this machine can use the loaded models without a key.",
			gen.Advanced, gen.Live, gen.Boolean, def.LocalAPI.RequireAuthV1, cur.RequireAuthV1),
		mk("log_level", "log.level", "Log level",
			"How much the daemon logs. debug is verbose and meant for troubleshooting.",
			gen.Advanced, logApply, gen.Enum, def.Log.Level, cur.LogLevel, logOpts...),
	}
}

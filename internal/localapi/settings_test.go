package localapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
	"github.com/teraflock/flockd/internal/telemetry"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// settingsHarness is a governor-backed server with the flockd#55 hooks
// recorded.
type settingsHarness struct {
	srv *httptest.Server
	dir string

	mu      sync.Mutex
	plans   []modelops.PlanLimits
	exclude [][]string
}

func newSettingsServer(t *testing.T, boot SettingsValues, overlay config.LimitsExtra) *settingsHarness {
	t.Helper()
	h := &settingsHarness{dir: t.TempDir()}
	eng := engine.New(nil, nil, nil)
	inst, err := rt.NewMockRuntime(0).Load(context.Background(), rt.ModelSpec{ID: "chat"}, rt.ResourceBudget{})
	if err != nil {
		t.Fatal(err)
	}
	eng.Register(rt.ModelSpec{ID: "chat"}, inst)
	s := New(Deps{
		Engine: eng, Governor: servingGovernor(t), Log: quietLog(), NodeID: "n", Version: "test", Token: testToken, DataDir: h.dir,
		// Apple Silicon: auto slots resolve to 16.
		Hardware: &typesv1.CapabilityProfile{Os: "darwin", Arch: "arm64", Gpus: []*typesv1.GpuInfo{{Model: "M4", Accel: "metal", UnifiedMemory: true}}},
		Defaults: config.LiveLimits{MeshManaged: true, MaxDiskMB: 61440, IdleUnloadS: 900},
		Settings: SettingsDeps{
			Boot: boot, Overlay: overlay,
			ApplyPlan:  func(l modelops.PlanLimits) { h.mu.Lock(); h.plans = append(h.plans, l); h.mu.Unlock() },
			SetExclude: func(ids []string) { h.mu.Lock(); h.exclude = append(h.exclude, ids); h.mu.Unlock() },
		},
	})
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	return h
}

const baseLimits = `"serve_policy":"always","idle_after_seconds":120,"yield_grace_seconds":2,"serve_on_battery":false,"max_temp_celsius":90,"schedule":[]`

func (h *settingsHarness) get(t *testing.T) (gen.Limits, map[string]gen.LimitSetting) {
	t.Helper()
	var lim gen.Limits
	if code := apiDo(t, h.srv, http.MethodGet, "/api/v1/limits", "", &lim); code != http.StatusOK {
		t.Fatalf("GET limits = %d", code)
	}
	return lim, settingsByKey(t, lim)
}

func settingsByKey(t *testing.T, lim gen.Limits) map[string]gen.LimitSetting {
	t.Helper()
	if lim.Settings == nil {
		t.Fatal("limits has no settings metadata")
	}
	out := map[string]gen.LimitSetting{}
	for _, m := range *lim.Settings {
		out[m.Key] = m
	}
	return out
}

func (h *settingsHarness) put(t *testing.T, extra string) (int, gen.Limits) {
	t.Helper()
	body := "{" + baseLimits
	if extra != "" {
		body += "," + extra
	}
	body += "}"
	var lim gen.Limits
	code := apiDo(t, h.srv, http.MethodPut, "/api/v1/limits", body, &lim)
	return code, lim
}

// Every Limits field has a metadata entry and the other way round, with
// the fields an app needs to render it.
func TestSettingsCoverEveryLimitsField(t *testing.T) {
	h := newSettingsServer(t, SettingsValues{MaxContext: 16384, MinContext: 8192}, config.LimitsExtra{})
	lim, meta := h.get(t)

	raw, _ := json.Marshal(lim)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "settings")
	for key := range fields {
		if _, ok := meta[key]; !ok {
			t.Errorf("limits field %q has no settings entry", key)
		}
	}
	want := reflect.TypeOf(gen.Limits{}).NumField() - 1 // every field but `settings`
	if len(meta) != want || len(fields) != want {
		t.Fatalf("%d settings entries, %d fields returned, %d fields in the schema", len(meta), len(fields), want)
	}
	tiers := map[gen.LimitSettingTier]int{}
	seenAdvanced := false
	for _, m := range *lim.Settings {
		if _, ok := fields[m.Key]; !ok {
			t.Errorf("settings entry %q is not a limits field", m.Key)
		}
		if m.Title == "" || len(m.Description) < 20 || m.ConfigKey == "" || !strings.Contains(m.ConfigKey, ".") {
			t.Errorf("%s: incomplete metadata %+v", m.Key, m)
		}
		if !m.Tier.Valid() || !m.Apply.Valid() || !m.Type.Valid() {
			t.Errorf("%s: tier/apply/type = %q %q %q", m.Key, m.Tier, m.Apply, m.Type)
		}
		if m.Type == gen.Enum && (m.Options == nil || len(*m.Options) == 0) {
			t.Errorf("%s: enum without options", m.Key)
		}
		if (m.Type == gen.Integer || m.Type == gen.Number) && (m.Unit == nil || m.Min == nil) {
			t.Errorf("%s: number without unit/min", m.Key)
		}
		// Display order: common first, then advanced.
		if m.Tier == gen.Advanced {
			seenAdvanced = true
		} else if seenAdvanced {
			t.Errorf("%s: a common setting after the advanced ones", m.Key)
		}
		tiers[m.Tier]++
		// `configured` is the field's own value.
		got, _ := json.Marshal(m.Configured)
		if !bytes.Equal(got, fields[m.Key]) {
			t.Errorf("%s: configured %s != field %s", m.Key, got, fields[m.Key])
		}
	}
	if tiers[gen.Common] != 10 || tiers[gen.Advanced] != 11 {
		t.Fatalf("tiers = %v", tiers)
	}
	for key, apply := range map[string]gen.LimitSettingApply{
		"max_concurrent": gen.Reload, "max_context": gen.Reload, "min_context": gen.Reload, "context_length": gen.Reload,
		"max_vram_percent": gen.Restart, "default_model": gen.Live, "log_level": gen.Restart, // no SetLogLevel wired here
		"min_concurrent": gen.Reload,
		"exclude":        gen.Live, "require_auth_v1": gen.Live, "serve_policy": gen.Live, "max_ram_mb": gen.Live,
	} {
		if meta[key].Apply != apply {
			t.Errorf("%s apply = %q, want %q", key, meta[key].Apply, apply)
		}
	}
	if meta["max_concurrent"].Tier != gen.Common || meta["max_context"].Tier != gen.Advanced {
		t.Errorf("tiers: max_concurrent %q, max_context %q", meta["max_concurrent"].Tier, meta["max_context"].Tier)
	}
}

func TestSettingsEffectiveAndApply(t *testing.T) {
	h := newSettingsServer(t, SettingsValues{MaxContext: 16384, MinContext: 8192, DefaultModel: "llama", LogLevel: "info"}, config.LimitsExtra{})
	lim, meta := h.get(t)

	// max_concurrent 0 = auto, resolved for this machine.
	mc := meta["max_concurrent"]
	if *lim.MaxConcurrent != 0 || mc.Configured != float64(0) || mc.Effective != float64(16) || mc.PendingRestart ||
		mc.ZeroMeans == nil || *mc.ZeroMeans != "auto" || *mc.Max != 64 {
		t.Fatalf("max_concurrent auto = %+v", mc)
	}
	if meta["max_context"].Effective != nil || meta["max_context"].Configured != float64(16384) {
		t.Fatalf("max_context = %+v", meta["max_context"])
	}

	// Slots and context: applied through ApplyPlan (reload of idle models),
	// effective == configured once set, persisted, config.toml untouched.
	code, lim := h.put(t, `"max_concurrent":4,"max_context":8192`)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	meta = settingsByKey(t, lim)
	if *lim.MaxConcurrent != 4 || *lim.MaxContext != 8192 || meta["max_concurrent"].Effective != nil {
		t.Fatalf("after PUT: %v %v %+v", *lim.MaxConcurrent, *lim.MaxContext, meta["max_concurrent"])
	}
	h.mu.Lock()
	plans := append([]modelops.PlanLimits(nil), h.plans...)
	h.mu.Unlock()
	if len(plans) != 1 || plans[0] != (modelops.PlanLimits{MaxConcurrent: 4, MaxContext: 8192, MinContext: 8192}) {
		t.Fatalf("ApplyPlan calls = %+v", plans)
	}
	raw, err := os.ReadFile(config.LimitsPath(h.dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"max_concurrent = 4", "max_context = 8192"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("overlay lacks %q:\n%s", want, raw)
		}
	}
	// Only what was set is in the overlay: min_context, the default model
	// and the rest stay with config.toml.
	for _, not := range []string{"min_context", "default =", "max_vram_percent", "exclude", "require_auth_v1", "level"} {
		if bytes.Contains(raw, []byte(not)) {
			t.Fatalf("overlay holds %q, which was never set:\n%s", not, raw)
		}
	}
	// A PUT that changes nothing about the plan does not reload models,
	// and a client writing the whole object back (as an app saving one
	// toggle does) does not move the untouched values into the overlay.
	whole, _ := json.Marshal(lim)
	if code := apiDo(t, h.srv, http.MethodPut, "/api/v1/limits", string(whole), nil); code != http.StatusOK {
		t.Fatalf("PUT of the whole object = %d", code)
	}
	raw, _ = os.ReadFile(config.LimitsPath(h.dir))
	for _, not := range []string{"min_context", "default =", "max_vram_percent", "exclude", "require_auth_v1", "level"} {
		if bytes.Contains(raw, []byte(not)) {
			t.Fatalf("writing the object back froze %q into the overlay:\n%s", not, raw)
		}
	}
	h.mu.Lock()
	n := len(h.plans)
	h.mu.Unlock()
	if n != 1 {
		t.Fatalf("an unchanged plan was applied again (%d calls)", n)
	}

	// Restart settings: saved, reported as pending with the running value.
	code, lim = h.put(t, `"default_model":"none","log_level":"debug","max_vram_percent":60`)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	meta = settingsByKey(t, lim)
	// default_model is live: "none" is saved for the next start, never
	// pending; the default still running shows as the effective value.
	if d := meta["default_model"]; *lim.DefaultModel != "" || d.PendingRestart || d.Apply != gen.Live || d.Effective != "chat" || d.Configured != "" {
		t.Fatalf("default_model = %+v", d)
	}
	if l := meta["log_level"]; !l.PendingRestart || l.Effective != "info" || l.Configured != "debug" {
		t.Fatalf("log_level = %+v", l)
	}
	if v := meta["max_vram_percent"]; !v.PendingRestart || v.Effective != float64(80) || v.Configured != float64(60) {
		t.Fatalf("max_vram_percent = %+v", v)
	}
	raw, _ = os.ReadFile(config.LimitsPath(h.dir))
	for _, want := range []string{`default = "none"`, `level = "debug"`, "max_vram_percent = 60", "max_concurrent = 4"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("overlay lacks %q:\n%s", want, raw)
		}
	}
	// Setting it back clears the pending state.
	_, lim = h.put(t, `"log_level":"info"`)
	if l := settingsByKey(t, lim)["log_level"]; l.PendingRestart || l.Effective != nil {
		t.Fatalf("log_level reset = %+v", l)
	}

	// Live ones: exclude and require_auth_v1.
	code, lim = h.put(t, `"exclude":["big-model"," big-model ","other"],"require_auth_v1":true`)
	if code != http.StatusOK || len(*lim.Exclude) != 2 || !*lim.RequireAuthV1 {
		t.Fatalf("PUT = %d %+v", code, lim.Exclude)
	}
	h.mu.Lock()
	ex := h.exclude
	h.mu.Unlock()
	if len(ex) != 1 || len(ex[0]) != 2 || ex[0][0] != "big-model" || ex[0][1] != "other" {
		t.Fatalf("SetExclude calls = %v", ex)
	}
	// /v1 now needs the token, immediately.
	resp, err := http.Post(h.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":2}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/v1 without a token after require_auth_v1 = %d", resp.StatusCode)
	}
}

// A PUT is validated whole before anything is applied.
func TestSettingsValidation(t *testing.T) {
	h := newSettingsServer(t, SettingsValues{MaxContext: 16384, MinContext: 8192}, config.LimitsExtra{})
	for name, extra := range map[string]string{
		"slots over the limit":      `"max_concurrent":65`,
		"negative slots":            `"max_concurrent":-1`,
		"tiny max_context":          `"max_context":512`,
		"huge max_context":          `"max_context":2000000`,
		"min above max":             `"min_context":32768`,
		"tiny pin":                  `"context_length":100`,
		"vram percent 0":            `"max_vram_percent":0`,
		"vram percent 101":          `"max_vram_percent":101`,
		"bad log level":             `"log_level":"loud"`,
		"exclude with a path":       `"exclude":["../etc/passwd"]`,
		"default model with a path": `"default_model":"a/b"`,
		"negative disk":             `"max_disk_mb":-1`,
	} {
		// Each bad PUT also carries good changes that must NOT be applied.
		code, _ := h.put(t, extra+`,"mesh_managed":false,"serve_on_battery":true`)
		if code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d, want 400", name, code)
		}
	}
	if code := apiDo(t, h.srv, http.MethodPut, "/api/v1/limits", `{"serve_policy":"sometimes","schedule":[]}`, nil); code != http.StatusBadRequest {
		t.Errorf("bad serve_policy = %d", code)
	}
	lim, _ := h.get(t)
	if lim.ServeOnBattery || *lim.MaxConcurrent != 0 || *lim.MaxContext != 16384 {
		t.Fatalf("a rejected PUT changed something: %+v", lim)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.plans) != 0 || len(h.exclude) != 0 {
		t.Fatalf("a rejected PUT reached the hooks: %v %v", h.plans, h.exclude)
	}
	if _, err := os.Stat(config.LimitsPath(h.dir)); err == nil {
		t.Fatal("a rejected PUT wrote limits.toml")
	}
}

// Keys limits.toml already holds are carried forward by a later save, and
// the valid edge values are accepted.
func TestSettingsOverlayCarriedForward(t *testing.T) {
	four := 4
	h := newSettingsServer(t, SettingsValues{MaxConcurrent: 4, MaxContext: 16384, MinContext: 8192}, config.LimitsExtra{MaxConcurrent: &four})
	if code, _ := h.put(t, `"max_context":0,"min_context":256,"context_length":4096,"max_vram_percent":100`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	raw, _ := os.ReadFile(config.LimitsPath(h.dir))
	for _, want := range []string{"max_concurrent = 4", "max_context = 0", "min_context = 256", "context_length = 4096", "max_vram_percent = 100"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("overlay lacks %q:\n%s", want, raw)
		}
	}
	lim, meta := h.get(t)
	if *lim.MaxConcurrent != 4 || meta["max_concurrent"].Effective != nil {
		t.Fatalf("max_concurrent = %v %+v", *lim.MaxConcurrent, meta["max_concurrent"])
	}
}

// The slot floor (flockd#54) and the live log level.
func TestSettingsSlotFloorAndLiveLogLevel(t *testing.T) {
	h := newSettingsServer(t, SettingsValues{MaxContext: 16384, MinContext: 8192, LogLevel: "info"}, config.LimitsExtra{})
	_, meta := h.get(t)
	// Auto floor resolves to 2; the ceiling is auto 16 on this hardware.
	if f := meta["min_concurrent"]; f.Configured != float64(0) || f.Effective != float64(2) || f.Tier != gen.Advanced || f.Apply != gen.Reload {
		t.Fatalf("min_concurrent auto = %+v", f)
	}
	// A floor above the ceiling is the pin: effective = the ceiling.
	code, lim := h.put(t, `"max_concurrent":4,"min_concurrent":8`)
	if code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if f := settingsByKey(t, lim)["min_concurrent"]; f.Configured != float64(8) || f.Effective != float64(4) {
		t.Fatalf("min_concurrent above the ceiling = %+v", f)
	}
	h.mu.Lock()
	plans := append([]modelops.PlanLimits(nil), h.plans...)
	h.mu.Unlock()
	if len(plans) != 1 || plans[0].MinConcurrent != 8 || plans[0].MaxConcurrent != 4 {
		t.Fatalf("ApplyPlan calls = %+v", plans)
	}
	raw, _ := os.ReadFile(config.LimitsPath(h.dir))
	if !bytes.Contains(raw, []byte("min_concurrent = 8")) {
		t.Fatalf("overlay lacks min_concurrent:\n%s", raw)
	}
	if code, _ := h.put(t, `"min_concurrent":65`); code != http.StatusBadRequest {
		t.Fatalf("min_concurrent 65 = %d", code)
	}

	// With a setter wired, log_level applies live and is never pending.
	var levels []string
	s := New(Deps{Engine: engine.New(nil, nil, nil), Governor: servingGovernor(t), Log: quietLog(), Token: testToken,
		Settings: SettingsDeps{Boot: SettingsValues{LogLevel: "info"}, SetLogLevel: func(l string) { levels = append(levels, l) }}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	var got gen.Limits
	if code := apiDo(t, srv, http.MethodPut, "/api/v1/limits", "{"+baseLimits+`,"log_level":"debug"}`, &got); code != http.StatusOK {
		t.Fatalf("PUT log_level = %d", code)
	}
	if l := settingsByKey(t, got)["log_level"]; l.Apply != gen.Live || l.PendingRestart || l.Effective != nil || l.Configured != "debug" {
		t.Fatalf("live log_level = %+v", l)
	}
	if len(levels) != 1 || levels[0] != "debug" {
		t.Fatalf("SetLogLevel calls = %v", levels)
	}
}

// "Make default" lasts: it switches the running default at once and is
// saved as default_model, which is what the next start loads. The setting
// reports configured == effective and nothing pending.
func TestMakeDefaultPersists(t *testing.T) {
	srv, dir, deps := newOpsServerDeps(t)
	_ = srv
	deps.Governor = servingGovernor(t)
	deps.Settings.Boot = SettingsValues{DefaultModel: "mock-8b-instruct"}
	s := New(deps)
	gsrv := httptest.NewServer(s.Handler())
	defer gsrv.Close()
	eng := deps.Engine
	for _, spec := range []rt.ModelSpec{{ID: "second-chat"}, {ID: "laya", Decision: true}, {ID: "embed", Embeddings: true}} {
		inst, err := rt.NewMockRuntime(0).Load(context.Background(), spec, rt.ResourceBudget{})
		if err != nil {
			t.Fatal(err)
		}
		eng.Register(spec, inst)
	}
	getDefault := func() (gen.Limits, gen.LimitSetting) {
		var lim gen.Limits
		if code := apiDo(t, gsrv, http.MethodGet, "/api/v1/limits", "", &lim); code != http.StatusOK {
			t.Fatalf("GET limits = %d", code)
		}
		return lim, settingsByKey(t, lim)["default_model"]
	}
	if _, d := getDefault(); d.Configured != "mock-8b-instruct" || d.Effective != nil || d.PendingRestart || d.Apply != gen.Live {
		t.Fatalf("before: %+v", d)
	}

	// Make default: live now...
	if code := apiDo(t, gsrv, http.MethodPost, "/api/v1/models/second-chat/default", "", nil); code != http.StatusOK {
		t.Fatalf("make default = %d", code)
	}
	if eng.DefaultModel() != "second-chat" {
		t.Fatalf("running default = %q", eng.DefaultModel())
	}
	// ...reported as configured == effective, nothing pending...
	lim, d := getDefault()
	if *lim.DefaultModel != "second-chat" || d.Configured != "second-chat" || d.Effective != nil || d.PendingRestart {
		t.Fatalf("after make default: %v %+v", *lim.DefaultModel, d)
	}
	// ...and what the next start reads.
	raw, err := os.ReadFile(config.LimitsPath(dir))
	if err != nil {
		t.Fatalf("limits.toml not written: %v", err)
	}
	if !bytes.Contains(raw, []byte(`default = "second-chat"`)) {
		t.Fatalf("limits.toml lacks the default:\n%s", raw)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("data_dir = \""+filepath.ToSlash(dir)+"\"\n[models]\ndefault = \"from-config\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models.Default != "second-chat" || cfg.Models.BaseDefault != "from-config" {
		t.Fatalf("next start: default=%q base=%q", cfg.Models.Default, cfg.Models.BaseDefault)
	}
	extra, _ := config.LoadLimitsExtra(dir)
	if extra.DefaultModel == nil || *extra.DefaultModel != "second-chat" {
		t.Fatalf("overlay default = %v", extra.DefaultModel)
	}

	// A refused make-default (decision / embedding / not loaded) changes
	// nothing, live or saved.
	for id, want := range map[string]int{"laya": http.StatusBadRequest, "embed": http.StatusBadRequest, "nope": http.StatusNotFound} {
		if code := apiDo(t, gsrv, http.MethodPost, "/api/v1/models/"+id+"/default", "", nil); code != want {
			t.Errorf("make default %s = %d, want %d", id, code, want)
		}
	}
	if _, d := getDefault(); eng.DefaultModel() != "second-chat" || d.Configured != "second-chat" {
		t.Fatalf("a refused make-default changed the default: %q %+v", eng.DefaultModel(), d)
	}

	// The same through the limits API: live for a loaded chat model,
	// refused for a decision or embedding model, saved either way it is
	// accepted.
	put := func(extra string) int {
		return apiDo(t, gsrv, http.MethodPut, "/api/v1/limits", "{"+baseLimits+","+extra+"}", nil)
	}
	if code := put(`"default_model":"mock-8b-instruct"`); code != http.StatusOK || eng.DefaultModel() != "mock-8b-instruct" {
		t.Fatalf("PUT default_model = %d, running default %q", code, eng.DefaultModel())
	}
	for _, id := range []string{"laya", "embed"} {
		if code := put(`"default_model":"` + id + `"`); code != http.StatusBadRequest {
			t.Errorf("PUT default_model %s = %d, want 400", id, code)
		}
	}
	// On disk but not loaded: loaded, then made the default.
	if err := deps.ModelOps.Fetch(context.Background(), "cat-model", models.OriginOperator); err != nil {
		t.Fatal(err)
	}
	if code := put(`"default_model":"cat-model"`); code != http.StatusOK {
		t.Fatalf("PUT default_model cat-model = %d", code)
	}
	deadline := time.Now().Add(5 * time.Second)
	for eng.DefaultModel() != "cat-model" {
		if time.Now().After(deadline) {
			t.Fatalf("a default on disk was not loaded and switched to; running default %q", eng.DefaultModel())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// "none": saved for the next start; the running default stays and is
	// shown as the effective value.
	if code := put(`"default_model":"none"`); code != http.StatusOK {
		t.Fatalf("PUT default_model none = %d", code)
	}
	if lim, d := getDefault(); *lim.DefaultModel != "" || d.Effective != "cat-model" || d.PendingRestart {
		t.Fatalf("after none: %+v", d)
	}
	raw, _ = os.ReadFile(config.LimitsPath(dir))
	if !bytes.Contains(raw, []byte(`default = "none"`)) {
		t.Fatalf("limits.toml after none:\n%s", raw)
	}
}

// Status carries lifetime and session counters side by side.
func TestStatusLifetimeAndSessionCounters(t *testing.T) {
	stats := telemetry.NewStats()
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	stats.Restore(telemetry.Counters{Tokens: 1000, Requests: 10, EarnedMicrocred: 55000, Since: since})
	eng := engine.New(nil, stats, nil)
	inst, _ := rt.NewMockRuntime(0).Load(context.Background(), rt.ModelSpec{ID: "chat"}, rt.ResourceBudget{})
	eng.Register(rt.ModelSpec{ID: "chat"}, inst)
	s := New(Deps{Engine: eng, Log: quietLog(), Token: testToken, Standalone: true})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":5,"seed":1}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var st gen.Status
	if code := apiDo(t, srv, http.MethodGet, "/api/v1/status", "", &st); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	x := st.Stats
	if x.SessionRequests != 1 || x.SessionTokens == 0 || x.TotalRequests != 11 || x.TotalTokens != 1000+x.SessionTokens ||
		x.EarnedMicrocredits != 55000+x.SessionEarnedMicrocredits || !x.LifetimeSince.Equal(since) {
		t.Fatalf("stats = %+v", x)
	}
	// The estimated daily rate is this run's, not the lifetime total over
	// this run's uptime.
	var e gen.Earnings
	if code := apiDo(t, srv, http.MethodGet, "/api/v1/earnings", "", &e); code != http.StatusOK {
		t.Fatalf("earnings = %d", code)
	}
	if e.LifetimeTokens != x.TotalTokens || e.EarnedMicrocredits != x.EarnedMicrocredits {
		t.Fatalf("earnings = %+v", e)
	}
	// The estimated daily rate is this run's earnings over this run's
	// uptime — not the lifetime total, which would inflate after a restart.
	got := estimatedEarnings(telemetry.Snapshot{EarnedMicrocred: 100e6, SessionEarnedMicrocred: 1e6, TotalTokens: 5},
		time.Now().Add(-12*time.Hour), "")
	if got.EstUsd < 99.99 || got.EstUsd > 100.01 || got.EstUsdPerDay < 1.99 || got.EstUsdPerDay > 2.01 || got.LifetimeTokens != 5 {
		t.Fatalf("estimated earnings = %+v, want $100 lifetime and ~$2/day from this run", got)
	}
}

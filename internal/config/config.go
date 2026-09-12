// Package config loads flockd configuration from defaults, an optional TOML
// file, and FLOCKD_-prefixed environment variables (koanf). Every knob is
// documented in docs/config.md.
package config

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

// Config is the fully resolved daemon configuration.
type Config struct {
	DataDir string `koanf:"data_dir"`

	Log      Log      `koanf:"log"`
	LocalAPI LocalAPI `koanf:"local_api"`
	Runtime  Runtime  `koanf:"runtime"`
	Governor Governor `koanf:"governor"`
	Budget   Budget   `koanf:"budget"`
	Models   Models   `koanf:"models"`
	Tunnel   Tunnel   `koanf:"tunnel"`
	Enroll   Enroll   `koanf:"enroll"`
	Update   Update   `koanf:"update"`
}

type Log struct {
	Level  string `koanf:"level"`  // debug|info|warn|error
	Format string `koanf:"format"` // text|json
	// File, when set, receives a copy of every record (appended, never
	// rotated) in addition to stderr. The Windows logon task sets it via
	// --log-file: a task has no console, so it is the daemon's only sink.
	File string `koanf:"file"`
}

type LocalAPI struct {
	// Listen address. Loopback only by default; changing this is dangerous
	// and documented with warnings in docs/config.md.
	Listen string `koanf:"listen"`
	// RequireAuthV1 requires the bearer token on the OpenAI-compatible /v1
	// endpoints too (the daemon-management /api/v1 always requires it).
	RequireAuthV1 bool `koanf:"require_auth_v1"`
}

type Runtime struct {
	// Kind selects the runtime adapter: "llamacpp" or "mock".
	Kind string `koanf:"kind"`
	// LlamaServerPath, when set, uses an existing llama-server binary
	// instead of downloading one from the artifact manifest.
	LlamaServerPath string `koanf:"llama_server_path"`
	// ArtifactManifestURL points at the JSON manifest of pinned, SHA-verified
	// llama-server builds (see internal/runtime/llamacpp/artifact.go).
	ArtifactManifestURL string `koanf:"artifact_manifest_url"`
	// MockTokensPerSec controls the synthetic generation speed of the mock
	// runtime (tests, --standalone demos).
	MockTokensPerSec float64 `koanf:"mock_tokens_per_sec"`
	// ContextLength pins the per-request (per-slot) context exactly
	// (0 = planned from the memory budget, flockd#46). Slots still adapt.
	ContextLength int `koanf:"context_length"`
	// MaxContext caps per-request context, in tokens (0 = the model's
	// training window). The planner spends spare memory on context up to
	// this cap, on every slot; the KV cache is sized by slots × context.
	MaxContext int `koanf:"max_context"`
	// MinContext is the per-request floor: the planner gives up slots
	// before a request gets less than this (0 = 8192).
	MinContext int `koanf:"min_context"`
	// ArtifactSigningKey overrides the daemon's built-in pin for the key
	// that runtime manifests and tarballs are cosign-signed with
	// (internal/runtime/llamacpp/trust.go): either an inline PEM public
	// key ("-----BEGIN PUBLIC KEY-----", ECDSA P-256) or the path to a PEM
	// file. For self-hosted coordinators and development catalogs signed
	// with their own key. Never taken from the manifest or artifact host.
	ArtifactSigningKey string `koanf:"artifact_signing_key"`
	// RequireSignature refuses runtime manifests and artifacts that carry
	// no cosign signature, refuses cached extracts that were trusted by
	// sha256 alone, and refuses to run without a pinned signing key.
	// Default true since flockd#25 stage 2: the stable manifest and every
	// artifact it lists are signed (runtimes llamacpp-b9892-4 onward) and
	// the daemon embeds the Teraflock key. A cached runtime that predates
	// signing is refused on the next start and re-fetched from stable;
	// false is for self-hosted catalogs that have not adopted signing.
	RequireSignature bool `koanf:"require_signature"`
}

// ArtifactSigningKeyPEM resolves Runtime.ArtifactSigningKey to PEM bytes:
// an inline PEM is returned as-is, anything else is read as a file path.
// nil when unset (the daemon then relies on its embedded pin, if any).
func (r Runtime) ArtifactSigningKeyPEM() ([]byte, error) {
	key := strings.TrimSpace(r.ArtifactSigningKey)
	if key == "" {
		return nil, nil
	}
	if strings.HasPrefix(key, "-----BEGIN") {
		return []byte(key + "\n"), nil
	}
	b, err := os.ReadFile(key)
	if err != nil {
		return nil, fmt.Errorf("config: runtime.artifact_signing_key: %w", err)
	}
	return b, nil
}

type Governor struct {
	// ServePolicy: "always", "idle-only" or "scheduled".
	ServePolicy string `koanf:"serve_policy"`
	// IdleAfter is how long input must be quiet before the node counts as
	// idle (idle-only policy).
	IdleAfter time.Duration `koanf:"idle_after"`
	// YieldGrace is the drain-or-cancel window on operator activity
	// (SPEC §4.1 instant-yield). Default 2s.
	YieldGrace time.Duration `koanf:"yield_grace"`
	// PollInterval is how often idle/power signals are sampled.
	PollInterval time.Duration `koanf:"poll_interval"`
	// ServeOnBattery: never serve on battery by default.
	ServeOnBattery bool `koanf:"serve_on_battery"`
	// MaxTempCelsius pauses serving above this temperature (0 disables).
	MaxTempCelsius float64 `koanf:"max_temp_celsius"`
	// Schedule windows for serve_policy=scheduled, e.g. ["22:00-08:00"].
	Schedule []string `koanf:"schedule"`
}

type Budget struct {
	MaxVRAMPercent int `koanf:"max_vram_percent"`
	// MaxRAMMB is the memory budget for loaded models; 0 = auto (half of
	// physical memory on unified-memory machines, vram × max_vram_percent
	// on discrete GPUs). Loads beyond it unload idle models first, then
	// are refused (mesh placements stay on disk as `cached`).
	MaxRAMMB int64 `koanf:"max_ram_mb"`
	// MaxConcurrent is the slot ceiling per loaded model (llama-server
	// --parallel) and the node's dispatch cap. 0 = auto by accelerator
	// class (16 with a GPU or unified memory, 2 CPU-only; memory.DefaultSlots).
	// Memory decides the actual slot count per load (flockd#46).
	MaxConcurrent int `koanf:"max_concurrent"`
}

type Models struct {
	// ManifestPath or ManifestURL locates the model catalog (YAML/JSON,
	// teraflock/models format).
	ManifestPath string `koanf:"manifest_path"`
	ManifestURL  string `koanf:"manifest_url"`
	// Default is the model id served in standalone mode.
	Default string `koanf:"default"`
	// MaxDiskMB is the model-cache disk budget; LRU eviction below it.
	MaxDiskMB int64 `koanf:"max_disk_mb"`
	// Pin lists model ids exempt from eviction; Exclude are never assigned.
	Pin     []string `koanf:"pin"`
	Exclude []string `koanf:"exclude"`
	// MeshManaged lets the coordinator place models on this node (download,
	// load, and later evict what it placed) inside max_disk_mb, minus pin
	// and exclude. Off = the node serves only what the operator installed.
	// Default on: an enrolled node that never takes placements never earns.
	MeshManaged bool `koanf:"mesh_managed"`
	// IdleUnloadS unloads a loaded model after this many seconds without a
	// request (0 = never; the default model is exempt). Reload is a mmap
	// re-open, seconds not a download.
	IdleUnloadS int `koanf:"idle_unload_s"`
	// RetentionDays evicts unpinned, unloaded models unused for this many
	// days (0 = never). Mesh and operator models alike.
	RetentionDays int `koanf:"retention_days"`
}

type Update struct {
	// FeedURL is the version feed the daemon polls hourly
	// ({flockd:{latest,minimum,url},desktop:{latest,url}}).
	FeedURL string `koanf:"feed_url"`
}

type Tunnel struct {
	// CoordinatorAddr is the coordinator endpoint (host:port).
	CoordinatorAddr string `koanf:"coordinator_addr"`
	// Standalone runs the in-process fake coordinator (Phase 0).
	Standalone bool `koanf:"standalone"`
	// HeartbeatInterval between heartbeats (coordinator may override).
	HeartbeatInterval time.Duration `koanf:"heartbeat_interval"`
	// ReconnectMin/Max bound the jittered exponential backoff.
	ReconnectMin time.Duration `koanf:"reconnect_min"`
	ReconnectMax time.Duration `koanf:"reconnect_max"`
	// InsecureSkipVerify disables server cert verification (dev only).
	InsecureSkipVerify bool `koanf:"insecure_skip_verify"`
	// CACert pins an extra root for the coordinator's server certificate:
	// the path of a PEM file, or the PEM itself. Only a self-hosted or
	// staging coordinator whose certificate is issued by its own mesh CA
	// (or a private CA) needs it — tunnel.teraflock.ai presents a public
	// certificate. It applies to every dial (enrollment, cert rotation,
	// the mTLS session) alongside the system roots, which stay trusted;
	// it is never a substitute for insecure_skip_verify's blanket skip.
	// (teraflock/flockd#4)
	CACert string `koanf:"ca_cert"`
	// Insecure dials the coordinator over plaintext gRPC instead of TLS.
	// The dev coordinator (`just run-coordinator`) serves plaintext until
	// mTLS termination lands; never enable this against a real deployment.
	Insecure bool `koanf:"insecure"`
}

type Enroll struct {
	// LoginURL is the browser URL opened by `tera login`.
	LoginURL string `koanf:"login_url"`
}

// Default returns the built-in defaults.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		DataDir: filepath.Join(home, ".teraflock"),
		Log:     Log{Level: "info", Format: "text"},
		LocalAPI: LocalAPI{
			Listen:        "127.0.0.1:7777",
			RequireAuthV1: false,
		},
		Runtime: Runtime{
			Kind: "llamacpp",
			// Pinned llama-server builds published by teraflock/runtimes;
			// runtime.llama_server_path overrides for self-built binaries.
			ArtifactManifestURL: "https://teraflock-downloads.s3.amazonaws.com/runtimes/llamacpp/manifest.json",
			RequireSignature:    true,
			MockTokensPerSec:    120,
			MaxContext:          16384,
			MinContext:          8192,
		},
		Governor: Governor{
			ServePolicy:    "idle-only",
			IdleAfter:      2 * time.Minute,
			YieldGrace:     2 * time.Second,
			PollInterval:   2 * time.Second,
			ServeOnBattery: false,
			MaxTempCelsius: 90,
		},
		Budget: Budget{
			MaxVRAMPercent: 80,
			MaxRAMMB:       0, // 0 = auto (half of system RAM)
			MaxConcurrent:  0, // 0 = auto by accelerator class (flockd#46)
		},
		Models: Models{
			// The hosted flat catalog (models repo CI publishes it); a
			// local manifest_path overrides for development.
			ManifestURL: "https://teraflock-downloads.s3.amazonaws.com/catalog/catalog.json",
			Default:     "llama-3.2-3b-instruct-q4_k_m",
			MaxDiskMB:   60 * 1024,
			MeshManaged: true,
			IdleUnloadS: 900,
		},
		Tunnel: Tunnel{
			CoordinatorAddr:   "tunnel.teraflock.ai:443",
			HeartbeatInterval: 5 * time.Second,
			ReconnectMin:      time.Second,
			ReconnectMax:      2 * time.Minute,
		},
		Enroll: Enroll{
			LoginURL: "https://teraflock.ai/claim",
		},
		Update: Update{
			FeedURL: "https://api.teraflock.ai/v1/versions",
		},
	}
}

// Load resolves configuration: defaults <- TOML file (if path != "" or the
// default path exists) <- FLOCKD_* environment variables.
//
// Env mapping: FLOCKD_LOCAL_API__LISTEN=... maps to local_api.listen
// (double underscore = section separator).
func Load(path string) (Config, error) {
	k := koanf.New(".")
	cfg := Default()

	if err := k.Load(structs.Provider(cfg, "koanf"), nil); err != nil {
		return cfg, fmt.Errorf("config: load defaults: %w", err)
	}

	if path == "" {
		def := filepath.Join(cfg.DataDir, "config.toml")
		if _, err := os.Stat(def); err == nil {
			path = def
		}
	}
	if path != "" {
		if err := k.Load(file.Provider(path), toml.Parser()); err != nil {
			return cfg, fmt.Errorf("config: load %s: %w", path, err)
		}
	}

	if err := k.Load(env.Provider("FLOCKD_", ".", func(s string) string {
		s = strings.TrimPrefix(s, "FLOCKD_")
		s = strings.ToLower(s)
		return strings.ReplaceAll(s, "__", ".")
	}), nil); err != nil {
		return cfg, fmt.Errorf("config: load env: %w", err)
	}

	if err := k.Unmarshal("", &cfg); err != nil {
		return cfg, fmt.Errorf("config: unmarshal: %w", err)
	}
	cfg.Models.Default = NormalizeDefaultModel(cfg.Models.Default)

	// Live-edited limits (PUT /api/v1/limits) persist in a daemon-owned
	// overlay so the operator's config.toml — comments and all — is never
	// rewritten by the API. The overlay is the operator's most recent
	// intent, so it wins over config.toml and env for the keys it holds.
	overlay := LimitsPath(cfg.DataDir)
	if _, err := os.Stat(overlay); err == nil {
		if err := k.Load(file.Provider(overlay), toml.Parser()); err != nil {
			return cfg, fmt.Errorf("config: load %s: %w", overlay, err)
		}
		if err := k.Unmarshal("", &cfg); err != nil {
			return cfg, fmt.Errorf("config: unmarshal %s: %w", overlay, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// LimitsPath is the daemon-owned governor-limits overlay location.
func LimitsPath(dataDir string) string {
	return filepath.Join(dataDir, "limits.toml")
}

// LiveLimits are the non-governor knobs the limits API edits live; they
// ride the same overlay as the governor policy.
type LiveLimits struct {
	MeshManaged   bool
	MaxDiskMB     int64
	RetentionDays int
	IdleUnloadS   int
	MaxRAMMB      int64
}

// SaveLimits persists live-edited limits to the overlay file that Load
// applies on the next start. Only the operator-facing limit knobs are
// written; poll_interval and the rest stay wherever the operator set them.
func SaveLimits(dataDir string, g Governor, l LiveLimits) error {
	var b strings.Builder
	b.WriteString("# Written by flockd when limits change via the API or app.\n")
	b.WriteString("# These override [governor], [models] and [budget] in config.toml; delete this file to undo.\n\n")
	b.WriteString("[governor]\n")
	fmt.Fprintf(&b, "serve_policy = %q\n", g.ServePolicy)
	fmt.Fprintf(&b, "idle_after = %q\n", g.IdleAfter.String())
	fmt.Fprintf(&b, "yield_grace = %q\n", g.YieldGrace.String())
	fmt.Fprintf(&b, "serve_on_battery = %t\n", g.ServeOnBattery)
	fmt.Fprintf(&b, "max_temp_celsius = %g\n", g.MaxTempCelsius)
	b.WriteString("schedule = [")
	for i, w := range g.Schedule {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", w)
	}
	b.WriteString("]\n")
	b.WriteString("\n[models]\n")
	fmt.Fprintf(&b, "mesh_managed = %t\n", l.MeshManaged)
	fmt.Fprintf(&b, "max_disk_mb = %d\n", l.MaxDiskMB)
	fmt.Fprintf(&b, "retention_days = %d\n", l.RetentionDays)
	fmt.Fprintf(&b, "idle_unload_s = %d\n", l.IdleUnloadS)
	b.WriteString("\n[budget]\n")
	fmt.Fprintf(&b, "max_ram_mb = %d\n", l.MaxRAMMB)

	tmp := LimitsPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return fmt.Errorf("config: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, LimitsPath(dataDir)); err != nil {
		return fmt.Errorf("config: finalize limits overlay: %w", err)
	}
	return nil
}

// Validate rejects nonsensical configurations early.
func (c Config) Validate() error {
	switch c.Governor.ServePolicy {
	case "always", "idle-only", "scheduled":
	default:
		return fmt.Errorf("config: invalid governor.serve_policy %q (want always|idle-only|scheduled)", c.Governor.ServePolicy)
	}
	switch c.Runtime.Kind {
	case "llamacpp", "mock":
	default:
		return fmt.Errorf("config: invalid runtime.kind %q (want llamacpp|mock)", c.Runtime.Kind)
	}
	if c.Budget.MaxVRAMPercent < 1 || c.Budget.MaxVRAMPercent > 100 {
		return fmt.Errorf("config: budget.max_vram_percent must be 1-100, got %d", c.Budget.MaxVRAMPercent)
	}
	if c.Budget.MaxConcurrent < 0 {
		return fmt.Errorf("config: budget.max_concurrent must be >= 0 (0 = auto by accelerator class)")
	}
	if c.Runtime.MinContext < 0 || c.Runtime.MaxContext < 0 || c.Runtime.ContextLength < 0 {
		return fmt.Errorf("config: runtime.min_context, max_context and context_length must be >= 0")
	}
	if c.Budget.MaxRAMMB < 0 {
		return fmt.Errorf("config: budget.max_ram_mb must be >= 0 (0 = auto)")
	}
	if c.Models.IdleUnloadS < 0 || c.Models.RetentionDays < 0 {
		return fmt.Errorf("config: models.idle_unload_s and models.retention_days must be >= 0 (0 = never)")
	}
	// A pinned CA that cannot be read or parsed fails startup rather than
	// the first dial: the whole point of the setting is a coordinator that
	// system roots reject, so a silently ignored value would reproduce
	// exactly the failure it exists to fix.
	pem, err := c.Tunnel.CACertPEM()
	if err != nil {
		return err
	}
	if len(pem) > 0 && !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return fmt.Errorf("config: tunnel.ca_cert holds no CERTIFICATE block")
	}
	return nil
}

// CACertPEM resolves Tunnel.CACert: nil when unset, the value itself when
// it is inline PEM, otherwise the contents of the file it names.
func (t Tunnel) CACertPEM() ([]byte, error) {
	v := strings.TrimSpace(t.CACert)
	if v == "" {
		return nil, nil
	}
	if strings.HasPrefix(v, "-----BEGIN") {
		return []byte(v), nil
	}
	b, err := os.ReadFile(v)
	if err != nil {
		return nil, fmt.Errorf("config: tunnel.ca_cert: %w", err)
	}
	return b, nil
}

// NoDefaultModel is the models.default (and flockd --default-model) value
// meaning "load nothing at startup": the node serves what the mesh places
// or the operator loads (flockd#49). Empty means the same; this spelling
// exists because an empty flag or env value reads as unset.
const NoDefaultModel = "none"

// NormalizeDefaultModel maps the "none" spelling to empty.
func NormalizeDefaultModel(id string) string {
	if strings.EqualFold(strings.TrimSpace(id), NoDefaultModel) {
		return ""
	}
	return id
}

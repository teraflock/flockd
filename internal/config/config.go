// Package config loads flockd configuration from defaults, an optional TOML
// file, and FLOCKD_-prefixed environment variables (koanf). Every knob is
// documented in docs/config.md.
package config

import (
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
}

type Log struct {
	Level  string `koanf:"level"`  // debug|info|warn|error
	Format string `koanf:"format"` // text|json
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
	// ContextLength override passed to llama-server (0 = model default).
	ContextLength int `koanf:"context_length"`
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
	MaxVRAMPercent int   `koanf:"max_vram_percent"`
	MaxRAMMB       int64 `koanf:"max_ram_mb"`
	MaxConcurrent  int   `koanf:"max_concurrent"`
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
			MockTokensPerSec:    120,
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
			MaxConcurrent:  2,
		},
		Models: Models{
			// The hosted flat catalog (models repo CI publishes it); a
			// local manifest_path overrides for development.
			ManifestURL: "https://teraflock-downloads.s3.amazonaws.com/catalog/catalog.json",
			Default:     "llama-3.2-3b-instruct-q4_k_m",
			MaxDiskMB:   60 * 1024,
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

// SaveLimits persists live-edited governor limits to the overlay file that
// Load applies on the next start. Only the operator-facing limit knobs are
// written; poll_interval and the rest stay wherever the operator set them.
func SaveLimits(dataDir string, g Governor) error {
	var b strings.Builder
	b.WriteString("# Written by flockd when limits change via the API or app.\n")
	b.WriteString("# These override [governor] in config.toml; delete this file to undo.\n\n")
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
	if c.Budget.MaxConcurrent < 1 {
		return fmt.Errorf("config: budget.max_concurrent must be >= 1")
	}
	return nil
}

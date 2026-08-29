// Command flockd is the Teraflock node daemon: hardware detection, model
// management, a supervised llama.cpp runtime, resource governance, the
// loopback OpenAI-compatible API, and the coordinator tunnel (in-process
// fake coordinator in --standalone / Phase 0 mode).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/enroll"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/hardware"
	"github.com/teraflock/flockd/internal/localapi"
	"github.com/teraflock/flockd/internal/logging"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	rt "github.com/teraflock/flockd/internal/runtime"
	"github.com/teraflock/flockd/internal/runtime/llamacpp"
	"github.com/teraflock/flockd/internal/telemetry"
	"github.com/teraflock/flockd/internal/tunnel"
	"github.com/teraflock/flockd/internal/tunnel/fakecoord"
	"github.com/teraflock/flockd/web"
	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// version is stamped by GoReleaser via -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "flockd:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to config.toml (default <data_dir>/config.toml)")
		standalone  = flag.Bool("standalone", false, "run with the in-process fake coordinator (no control plane needed)")
		runtimeKind = flag.String("runtime", "", "runtime adapter: llamacpp|mock (overrides config)")
		listen      = flag.String("listen", "", "local API listen address (overrides config)")
		dataDir     = flag.String("data-dir", "", "data directory (overrides config)")
		logLevel    = flag.String("log-level", "", "debug|info|warn|error (overrides config)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("flockd", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *runtimeKind != "" {
		cfg.Runtime.Kind = *runtimeKind
	}
	if *listen != "" {
		cfg.LocalAPI.Listen = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *standalone {
		cfg.Tunnel.Standalone = true
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log, ring := logging.New(cfg.Log.Level, cfg.Log.Format)
	slog.SetDefault(log)
	hardware.Version = version

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// ---- hardware ----
	hw, err := hardware.Detect(ctx, cfg.DataDir)
	if err != nil {
		return err
	}
	log.Info("hardware detected",
		"os", hw.GetOs(), "arch", hw.GetArch(),
		"cpu", hw.GetCpuModel(), "cores", hw.GetCpuCores(),
		"ram_mb", hw.GetRamTotalMb(), "gpus", len(hw.GetGpus()),
		"accel", hardware.BestAccel(hw))

	// ---- identity & token ----
	identity, err := enroll.LoadOrGenerateIdentity(cfg.DataDir)
	if err != nil {
		return err
	}
	token, err := localapi.LoadOrCreateToken(cfg.DataDir)
	if err != nil {
		return err
	}
	log.Info("node identity", "fingerprint", identity.Fingerprint())

	// ---- governor ----
	windows, err := governor.ParseWindows(cfg.Governor.Schedule)
	if err != nil {
		return err
	}
	gov := governor.New(governor.Policy{
		Serve:          cfg.Governor.ServePolicy,
		IdleAfter:      cfg.Governor.IdleAfter,
		YieldGrace:     cfg.Governor.YieldGrace,
		PollInterval:   cfg.Governor.PollInterval,
		ServeOnBattery: cfg.Governor.ServeOnBattery,
		MaxTempCelsius: cfg.Governor.MaxTempCelsius,
		Schedule:       windows,
	}, governor.NewPlatformIdleSource(), governor.NewPlatformPowerSource(), nil, log)
	go gov.Run(ctx)

	// ---- event bus (SSE) ----
	hub := events.NewHub()
	go func() {
		ch := gov.Subscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case st := <-ch:
				hub.Publish("state_change", map[string]string{"state": st.String()})
			}
		}
	}()

	// ---- models & runtime ----
	stats := telemetry.NewStats()
	var mgr *models.Manager
	if cfg.Runtime.Kind == "llamacpp" {
		mgr, err = models.NewManager(filepath.Join(cfg.DataDir, "models"), cfg.Models.MaxDiskMB, log)
		if err != nil {
			return err
		}
	}
	touch := func(string) {}
	if mgr != nil {
		touch = mgr.Touch
		mgr.OnProgress = func(id string, received, total int64) {
			hub.Publish("model_progress", map[string]any{
				"model": id, "received_bytes": received, "total_bytes": total,
			})
		}
	}
	eng := engine.New(gov, stats, touch)

	budget := rt.ResourceBudget{
		MaxVRAMPercent: cfg.Budget.MaxVRAMPercent,
		MaxRAMMB:       cfg.Budget.MaxRAMMB,
		MaxConcurrent:  cfg.Budget.MaxConcurrent,
	}

	// modelops backs both startup model loading and the local API's
	// on-demand download/load/switch routes (llamacpp only: the mock
	// runtime has no catalog or artifact cache to operate on).
	var ops *modelops.Service
	if cfg.Runtime.Kind == "llamacpp" {
		adapter := &llamacpp.Adapter{
			Fetcher: &llamacpp.Fetcher{
				ManifestURL: cfg.Runtime.ArtifactManifestURL,
				BinaryPath:  cfg.Runtime.LlamaServerPath,
				CacheDir:    filepath.Join(cfg.DataDir, "runtimes"),
			},
			Accel:         hardware.BestAccel(hw),
			VRAMMB:        hardware.BestVRAMMB(hw),
			Log:           log,
			ContextLength: cfg.Runtime.ContextLength,
		}
		ops = &modelops.Service{
			Mgr:          mgr,
			Eng:          eng,
			Loader:       adapter,
			Budget:       budget,
			Log:          log,
			ManifestPath: cfg.Models.ManifestPath,
			ManifestURL:  cfg.Models.ManifestURL,
			OnLoaded:     func(inst rt.Instance) { reportRuntimeBuild(hw, inst, log) },
			Events:       hub,
		}
	}

	if err := loadDefaultModel(ctx, cfg, hw, ops, mgr, eng, budget, log); err != nil {
		return err
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, m := range eng.Models() {
			_ = m.Instance.Shutdown(shutCtx)
		}
	}()

	// ---- tunnel ----
	// The node ID is assigned by the coordinator at enrollment, not derived
	// locally: the key fingerprint identifies the *key*, the node ID
	// identifies the enrolled node, and only the latter is what a session
	// may announce. Unenrolled daemons serve locally under the fingerprint.
	nodeID := identity.Fingerprint()
	mesh := &meshRunner{}
	// connectLocked (re)starts the tunnel client for creds; the previous
	// client, if any, is stopped first. Callers hold mesh.mu.
	connectLocked := func(creds *enroll.Credentials) error {
		dialer, err := meshDialer(cfg, identity, creds)
		if err != nil {
			return err
		}
		tctx, cancel := context.WithCancel(ctx)
		if err := startTunnel(tctx, cfg, dialer, cfg.Tunnel.CoordinatorAddr, creds.CoordinatorPubKey, creds.NodeID, identity, hw, gov, stats, eng, mgr, budget, log); err != nil {
			cancel()
			return err
		}
		if mesh.stop != nil {
			mesh.stop()
		}
		mesh.stop = cancel
		mesh.creds = creds
		return nil
	}

	var enrollNow func(ctx context.Context, code string) error
	if cfg.Tunnel.Standalone {
		coord, err := fakecoord.New()
		if err != nil {
			return err
		}
		defer coord.Stop()
		creds, err := standaloneEnroll(ctx, cfg, coord.Dialer(), coord.Addr(), identity, hw)
		if err != nil {
			return err
		}
		if err := startTunnel(ctx, cfg, coord.Dialer(), coord.Addr(), coord.PubKey(), creds.NodeID, identity, hw, gov, stats, eng, mgr, budget, log); err != nil {
			return err
		}
		nodeID = creds.NodeID
		log.Info("standalone mode: in-process fake coordinator running", "node_id", nodeID, "hint", "OPENAI_BASE_URL=http://"+cfg.LocalAPI.Listen+"/v1")
	} else {
		creds, err := ensureEnrolled(ctx, cfg, identity, hw, log)
		if err != nil {
			return err
		}
		switch {
		case creds != nil:
			mesh.mu.Lock()
			err := connectLocked(creds)
			mesh.mu.Unlock()
			if err != nil {
				return err
			}
			log.Info("tunnel client started", "coordinator", cfg.Tunnel.CoordinatorAddr, "node_id", creds.NodeID)
			nodeID = creds.NodeID
		default:
			log.Warn("node not enrolled: serving locally only", "hint", "run `tera login` to join the mesh, use the desktop app, or start with --standalone")
		}

		// Enrollment over the API (POST /api/v1/enroll): enroll — or
		// re-enroll, which the startup path can't do while old credentials
		// exist — and swap the tunnel live, no restart.
		enrollNow = func(rctx context.Context, code string) error {
			mesh.mu.Lock()
			if mesh.enrolling {
				mesh.mu.Unlock()
				return localapi.ErrEnrollInProgress
			}
			mesh.enrolling = true
			mesh.mu.Unlock()
			defer func() {
				mesh.mu.Lock()
				mesh.enrolling = false
				mesh.mu.Unlock()
			}()

			conn, err := bootstrapDialer(cfg).Dial(rctx, cfg.Tunnel.CoordinatorAddr)
			if err != nil {
				return fmt.Errorf("enrollment dial %s: %w", cfg.Tunnel.CoordinatorAddr, err)
			}
			defer conn.Close()
			ectx, cancel := context.WithTimeout(rctx, 30*time.Second)
			defer cancel()
			creds, err := enroll.Enroll(ectx, tunnelv1.NewTunnelServiceClient(conn), identity, code, hw, cfg.DataDir)
			if err != nil {
				return err
			}

			mesh.mu.Lock()
			err = connectLocked(creds)
			mesh.mu.Unlock()
			if err != nil {
				return fmt.Errorf("enrolled (node %s) but tunnel start failed: %w", creds.NodeID, err)
			}
			log.Info("enrolled via API", "node_id", creds.NodeID, "cert_expires", creds.CertExpiresAt.Format(time.RFC3339))
			// SSE subscribers get a fresh status snapshot immediately —
			// it now carries enrolled=true and the new node id.
			hub.Publish("state_change", map[string]string{"state": gov.State().String()})
			return nil
		}
	}

	// ---- local API ----
	webFS, err := web.Dist()
	if err != nil {
		log.Warn("embedded dashboard unavailable", "err", err)
		webFS = nil
	}
	srv := localapi.New(localapi.Deps{
		Engine:        eng,
		Governor:      gov,
		Models:        mgr,
		ModelOps:      ops,
		Events:        hub,
		DataDir:       cfg.DataDir,
		LogRing:       ring,
		Hardware:      hw,
		Log:           log,
		WebFS:         webFS,
		NodeID:        nodeID,
		Version:       version,
		Standalone:    cfg.Tunnel.Standalone,
		Mesh: func() localapi.MeshStatus {
			mesh.mu.Lock()
			defer mesh.mu.Unlock()
			if mesh.creds != nil {
				return localapi.MeshStatus{
					Enrolled:      true,
					NodeID:        mesh.creds.NodeID,
					CertExpiresAt: mesh.creds.CertExpiresAt,
				}
			}
			return localapi.MeshStatus{NodeID: nodeID}
		},
		Enroll: enrollNow,
		RequireAuthV1: cfg.LocalAPI.RequireAuthV1,
		Token:         token,
	})
	log.Info("flockd ready",
		"local_api", "http://"+cfg.LocalAPI.Listen,
		"dashboard", "http://"+cfg.LocalAPI.Listen+"/",
		"openai_base_url", "http://"+cfg.LocalAPI.Listen+"/v1")
	return srv.ListenAndServe(ctx, cfg.LocalAPI.Listen)
}

// meshRunner owns the live tunnel-client lifecycle so enrollment over the
// API can start or replace it without a daemon restart.
type meshRunner struct {
	mu        sync.Mutex
	creds     *enroll.Credentials
	stop      context.CancelFunc
	enrolling bool
}

// loadDefaultModel makes one model servable at startup. Further models
// arrive on demand through the local API (modelops) or the coordinator's
// ModelAssignment.
func loadDefaultModel(ctx context.Context, cfg config.Config, hw *typesv1.CapabilityProfile, ops *modelops.Service, mgr *models.Manager, eng *engine.Engine, budget rt.ResourceBudget, log *slog.Logger) error {
	switch cfg.Runtime.Kind {
	case "mock":
		mock := rt.NewMockRuntime(cfg.Runtime.MockTokensPerSec)
		// The mock is a fake in-memory model — Models.Default names a real
		// GGUF in the hosted catalog and is only meaningful on the llamacpp
		// path. Use a dedicated fixture id here so README/smoke examples
		// (mock-8b-instruct) hold no matter what the config default is.
		spec := rt.ModelSpec{ID: "mock-8b-instruct", ContextLength: 8192, Embeddings: true}
		inst, err := mock.Load(ctx, spec, budget)
		if err != nil {
			return err
		}
		eng.Register(spec, inst)
		reportRuntimeBuild(hw, inst, log)
		log.Info("mock runtime loaded", "model", spec.ID, "tok_per_sec", cfg.Runtime.MockTokensPerSec)
		return nil

	case "llamacpp":
		// Catalog fetch + artifact ensure + runtime load + registration all
		// live in modelops now, shared with the on-demand API routes.
		// OnLoaded stamps the runtime build id (reportRuntimeBuild).
		log.Info("ensuring default model", "model", cfg.Models.Default)
		if _, err := ops.LoadInstance(ctx, cfg.Models.Default); err != nil {
			return fmt.Errorf("load default model %q: %w", cfg.Models.Default, err)
		}
		for _, pin := range cfg.Models.Pin {
			if pin == cfg.Models.Default {
				_ = mgr.Pin(pin, true)
			}
		}
		log.Info("llama-server loaded", "model", cfg.Models.Default)
		return nil
	default:
		return fmt.Errorf("unknown runtime kind %q", cfg.Runtime.Kind)
	}
}

// reportRuntimeBuild stamps the loaded runtime's build id into the
// capability profile. The coordinator uses it to pick fingerprint challenges
// whose expected outputs were generated on this exact build; without it the
// node cannot be fingerprint-verified at all (SPEC §2.2).
func reportRuntimeBuild(hw *typesv1.CapabilityProfile, inst rt.Instance, log *slog.Logger) {
	b, ok := inst.(rt.BuildIdentified)
	if !ok {
		return
	}
	hw.RuntimeBuildId = b.RuntimeBuildID()
	log.Info("runtime build identified", "runtime_build_id", hw.RuntimeBuildId)
}

// startTunnel runs the session client in the background. nodeID must be the
// coordinator-assigned ID from enrollment — the session is rejected
// otherwise.
func startTunnel(ctx context.Context, cfg config.Config, dialer tunnel.Dialer, addr string, coordKey []byte, nodeID string, identity *enroll.Identity, hw *typesv1.CapabilityProfile, gov *governor.Governor, stats *telemetry.Stats, eng *engine.Engine, mgr *models.Manager, budget rt.ResourceBudget, log *slog.Logger) error {
	protoBudget := &typesv1.ResourceBudget{
		MaxVramPercent:        uint32(cfg.Budget.MaxVRAMPercent),
		MaxRamMb:              uint64(max(cfg.Budget.MaxRAMMB, 0)),
		MaxConcurrentRequests: uint32(cfg.Budget.MaxConcurrent),
		MaxDiskMb:             uint64(max(cfg.Models.MaxDiskMB, 0)),
		ServeOnBattery:        cfg.Governor.ServeOnBattery,
		MaxTempCelsius:        uint32(cfg.Governor.MaxTempCelsius),
		ServePolicy:           cfg.Governor.ServePolicy,
	}
	modelStates := func() []*typesv1.ModelState {
		var out []*typesv1.ModelState
		for _, m := range eng.Models() {
			out = append(out, &typesv1.ModelState{ModelId: m.Spec.ID, State: "ready"})
		}
		return out
	}

	client, err := tunnel.NewClient(tunnel.Options{
		Dialer:            dialer,
		Addr:              addr,
		NodeID:            nodeID,
		CoordinatorPubKey: coordKey,
		Engine:            eng,
		Admit:             gov,
		HeartbeatInterval: cfg.Tunnel.HeartbeatInterval,
		ReconnectMin:      cfg.Tunnel.ReconnectMin,
		ReconnectMax:      cfg.Tunnel.ReconnectMax,
		Log:               log,
		Hello: func() *tunnelv1.Hello {
			return &tunnelv1.Hello{
				NodeId:        nodeID,
				DaemonVersion: version,
				Capability:    hw,
				Models:        modelStates(),
				Budget:        protoBudget,
			}
		},
		Heartbeat: func() *tunnelv1.Heartbeat {
			power := gov.Power()
			return stats.BuildHeartbeat(telemetry.HeartbeatInput{
				State:      gov.State().NodeState(),
				QueueDepth: gov.Inflight(),
				GPUTempC:   power.TempCelsius,
				OnBattery:  power.OnBattery,
				Models:     modelStates(),
			})
		},
		OnModelAssignment: func(ctx context.Context, ma *tunnelv1.ModelAssignment) {
			// Phase 1: download+load assigned models, evict listed ones.
			// Requires the llamacpp runtime; logged for now on mock nodes.
			log.Info("model assignment received", "assign", len(ma.GetAssign()), "evict", len(ma.GetEvictModelIds()))
		},
	})
	if err != nil {
		return err
	}
	go client.Run(ctx)
	return nil
}

// standaloneEnroll enrolls against the in-process fake coordinator. Its
// credentials live in a `standalone/` subdirectory so that running
// `flockd --standalone` on a machine that has really joined the mesh cannot
// overwrite that node's mesh identity. The fake is ephemeral, so this
// re-enrolls on every start.
func standaloneEnroll(ctx context.Context, cfg config.Config, dialer tunnel.Dialer, addr string, identity *enroll.Identity, hw *typesv1.CapabilityProfile) (*enroll.Credentials, error) {
	conn, err := dialer.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	creds, err := enroll.Enroll(ctx, tunnelv1.NewTunnelServiceClient(conn), identity, "standalone",
		hw, filepath.Join(cfg.DataDir, "standalone"))
	if err != nil {
		return nil, fmt.Errorf("standalone enrollment: %w", err)
	}
	return creds, nil
}

// ensureEnrolled returns the node's mesh credentials, performing enrollment
// first when `tera login` has left a claim code behind. It returns (nil, nil)
// when the node has neither credentials nor a pending claim code — that is
// the ordinary "serving locally only" state, not an error.
func ensureEnrolled(ctx context.Context, cfg config.Config, identity *enroll.Identity, hw *typesv1.CapabilityProfile, log *slog.Logger) (*enroll.Credentials, error) {
	if enroll.Enrolled(cfg.DataDir) {
		creds, err := enroll.LoadCredentials(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		return rotateIfNeeded(ctx, cfg, identity, creds, hw, log)
	}

	code, err := enroll.ReadClaimCode(cfg.DataDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Bootstrap dial: the node has no client cert yet, so this leg is
	// server-authenticated only. The credentials it returns (CA + pinned
	// coordinator key) are what make every later connection mTLS.
	log.Info("claim code found: enrolling with coordinator", "coordinator", cfg.Tunnel.CoordinatorAddr)
	conn, err := bootstrapDialer(cfg).Dial(ctx, cfg.Tunnel.CoordinatorAddr)
	if err != nil {
		return nil, fmt.Errorf("enrollment dial %s: %w", cfg.Tunnel.CoordinatorAddr, err)
	}
	defer conn.Close()

	enrollCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	creds, err := enroll.Enroll(enrollCtx, tunnelv1.NewTunnelServiceClient(conn), identity, code, hw, cfg.DataDir)
	if err != nil {
		// Keep the claim code: a coordinator that is merely unreachable
		// should not cost the operator their code. A rejected code is
		// reported plainly so they know to get a fresh one.
		return nil, fmt.Errorf("enrollment failed (claim code kept at %s): %w", enroll.ClaimCodePath(cfg.DataDir), err)
	}
	// Claim codes are one-shot at the coordinator; a spent one would only
	// produce confusing failures on later restarts.
	if err := enroll.ClearClaimCode(cfg.DataDir); err != nil {
		log.Warn("could not remove spent claim code", "err", err)
	}
	log.Info("enrolled with mesh", "node_id", creds.NodeID, "cert_expires", creds.CertExpiresAt.Format(time.RFC3339))
	return creds, nil
}

// rotateIfNeeded refreshes the client certificate when it is near expiry
// (SPEC §6: 30-day certs, rotate in the last 7 days). Rotation reuses the
// Enroll RPC over the bootstrap (server-auth) dial — possession of the node
// key is the credential, not a claim code — so a failure here must never
// take the node down: it keeps serving on the old cert and retries on the
// next restart.
func rotateIfNeeded(ctx context.Context, cfg config.Config, identity *enroll.Identity, creds *enroll.Credentials, hw *typesv1.CapabilityProfile, log *slog.Logger) (*enroll.Credentials, error) {
	if time.Until(creds.CertExpiresAt) > 7*24*time.Hour {
		return creds, nil
	}
	log.Info("client cert near expiry: rotating", "expires", creds.CertExpiresAt.Format(time.RFC3339))
	conn, err := bootstrapDialer(cfg).Dial(ctx, cfg.Tunnel.CoordinatorAddr)
	if err != nil {
		log.Warn("cert rotation dial failed; keeping current cert", "err", err)
		return creds, nil
	}
	defer conn.Close()
	rotateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	fresh, err := enroll.RotateIfNeeded(rotateCtx, tunnelv1.NewTunnelServiceClient(conn), identity, creds, hw, cfg.DataDir)
	if err != nil {
		log.Warn("cert rotation failed; keeping current cert", "err", err)
		return creds, nil
	}
	return fresh, nil
}

// bootstrapDialer dials the coordinator before the node holds a client cert
// (enrollment only). Plaintext in dev, server-authenticated TLS otherwise.
func bootstrapDialer(cfg config.Config) tunnel.Dialer {
	if cfg.Tunnel.Insecure {
		return tunnel.InsecureDialer{}
	}
	return &tunnel.TLSDialer{TLS: &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: cfg.Tunnel.InsecureSkipVerify, //nolint:gosec // dev flag, documented
	}}
}

// meshDialer builds the session dialer from enrollment credentials: mTLS
// with the coordinator CA pinned as the only root.
func meshDialer(cfg config.Config, identity *enroll.Identity, creds *enroll.Credentials) (tunnel.Dialer, error) {
	if cfg.Tunnel.Insecure {
		return tunnel.InsecureDialer{}, nil
	}
	tlsCfg, err := enroll.ClientTLSConfig(identity, creds, cfg.Tunnel.InsecureSkipVerify)
	if err != nil {
		return nil, err
	}
	return &tunnel.TLSDialer{TLS: tlsCfg}, nil
}

// Command tera is the operator CLI/TUI for the Teraflock node daemon. All
// commands are clients of the daemon's local API on localhost:7777.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/teraflock/flockd/internal/browser"
	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/enroll"
	"github.com/teraflock/flockd/internal/hardware"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/runtime/llamacpp"
	"github.com/teraflock/flockd/internal/svc"
)

var version = "dev"

var (
	flagAPI     string
	flagDataDir string
	flagToken   string
)

// Brand palette v1 (website/docs/brand.md): gold for the brand voice,
// orange (never gold) for warnings. Hex degrades to ANSI where needed.
var (
	styleTitle = lipgloss.NewStyle().Foreground(lipgloss.Color("#f5b60d")).Bold(true)
	styleOK    = lipgloss.NewStyle().Foreground(lipgloss.Color("#34d399"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c"))
	styleDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("#8b95a9"))
)

func main() {
	root := &cobra.Command{
		Use:           "tera",
		Short:         "Teraflock node CLI — earn credits serving open-weight LLM inference on idle hardware",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagAPI, "api", "http://127.0.0.1:7777", "daemon local API base URL")
	root.PersistentFlags().StringVar(&flagDataDir, "data-dir", "", "daemon data directory (for the auth token)")
	root.PersistentFlags().StringVar(&flagToken, "token", "", "local API bearer token (default: $TERA_TOKEN, else <data-dir>/local_api_token)")

	root.AddCommand(
		cmdUp(), cmdDown(), cmdStatus(), cmdLogin(), cmdModels(), cmdLimits(),
		cmdEarnings(), cmdRedeem(), cmdDashboard(), cmdLogs(), cmdChat(), cmdMCP(), cmdToken(), cmdVersion(), cmdUninstall(),
	)

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, styleWarn.Render("error:"), err)
		// The client knows the fix for the errors it produces (daemon
		// down, governor holding back, model not installed): say it.
		if r := client.Remedy(err); r != "" && !strings.Contains(err.Error(), r) {
			fmt.Fprintln(os.Stderr, styleDim.Render("  "+r))
		}
		os.Exit(1)
	}
}

// findFlockd locates the daemon binary (next to tera, then PATH).
func findFlockd() (string, error) {
	self, err := os.Executable()
	if err == nil {
		cand := filepath.Join(filepath.Dir(self), "flockd"+exeSuffix())
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	return exec.LookPath("flockd")
}

// exeSuffix is the binary suffix for a sibling lookup (LookPath handles
// PATHEXT on its own).
func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func cmdUp() *cobra.Command {
	var standalone bool
	c := &cobra.Command{
		Use:   "up",
		Short: "Install and start the flockd service (launchd / systemd --user / Windows logon task)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			bin, err := findFlockd()
			if err != nil {
				return fmt.Errorf("flockd binary not found (install it next to tera or on PATH): %w", err)
			}
			daemonArgs := []string{}
			if standalone {
				daemonArgs = append(daemonArgs, "--standalone")
			}
			logPath := filepath.Join(dataDir(), "flockd.log")
			ctx := cmd.Context()
			// Refuse to install a service that will crash-loop: fetch the
			// pinned runtime manifest and confirm a build exists for this
			// box. Skipped for the mock runtime (no artifacts needed).
			if err := preflightRuntime(ctx); err != nil {
				return err
			}
			m := svc.NewManager()
			opts := svc.Options{
				LogPath: logPath,
				// Platform notes from the installer (Linux lingering): shown
				// after the success line so they read as footnotes, not
				// failures.
				Warn: func(msg string) { fmt.Println(styleWarn.Render("!"), msg) },
				Info: func(msg string) { fmt.Println(styleDim.Render("  " + msg)) },
			}
			if err := m.Install(ctx, bin, daemonArgs, opts); err != nil {
				return err
			}
			if err := m.Start(ctx); err != nil {
				return err
			}
			// The service manager returns as soon as the process exec'd —
			// a daemon that dies during boot (missing runtime, bad config)
			// looks identical to a healthy start. Poll the local API until
			// it answers, so `tera up` fails loudly instead of leaving a
			// crash-looping unit behind.
			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			if err := waitForDaemon(waitCtx, flagAPI); err != nil {
				fmt.Println(styleWarn.Render("✗"), "flockd installed but not responding on", flagAPI)
				if tail := tailFlockdLogs(ctx, logPath); tail != "" {
					fmt.Println(styleDim.Render("  last log lines:"))
					for _, line := range strings.Split(strings.TrimRight(tail, "\n"), "\n") {
						fmt.Println(styleDim.Render("    " + line))
					}
				}
				return fmt.Errorf("daemon did not come up within 10s (try `tera down` to stop the restart loop): %w", err)
			}
			fmt.Println(styleOK.Render("✓"), "flockd service installed and started")
			fmt.Println(styleDim.Render("  check it with `tera status` or `tera dashboard`"))
			return nil
		},
	}
	c.Flags().BoolVar(&standalone, "standalone", false, "run the daemon with the in-process fake coordinator")
	return c
}

// preflightRuntime refuses to install a service unit that would crash-loop
// because no llama-server build exists for this OS/arch/accel in the pinned
// catalog. Cheap: one JSON fetch + a pure pick(). Skipped for the mock
// runtime and when a local llama_server_path is already configured.
func preflightRuntime(ctx context.Context) error {
	cfg, err := config.Load("")
	if err != nil {
		return fmt.Errorf("preflight: load config: %w", err)
	}
	if cfg.Runtime.Kind != "llamacpp" {
		return nil
	}
	preCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	hw, err := hardware.Detect(preCtx, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("preflight: hardware detect: %w", err)
	}
	f := &llamacpp.Fetcher{
		ManifestURL: cfg.Runtime.ArtifactManifestURL,
		BinaryPath:  cfg.Runtime.LlamaServerPath,
	}
	if err := f.Preflight(preCtx, hardware.AccelPreference(hw)...); err != nil {
		return fmt.Errorf("%w\n\n"+
			"Options:\n"+
			"  · set runtime.kind = \"mock\" in ~/.teraflock/config.toml for a\n"+
			"    quick smoke test (deterministic tokens, no artifacts needed)\n"+
			"  · set runtime.llama_server_path to a locally built llama-server\n"+
			"  · wait for a matching build to publish in the runtime catalog", err)
	}
	return nil
}

// waitForDaemon polls the local API until it answers or ctx expires. Any
// HTTP response counts as "up" — 401 from the auth wall is fine, we only
// need to know the listener is bound. The daemon binds the port as the very
// last step of boot, so a successful probe proves it got past hardware
// detection, model load, and runtime fetch.
func waitForDaemon(ctx context.Context, base string) error {
	url := strings.TrimSuffix(base, "/") + "/api/v1/status"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return lastErr
		case <-ticker.C:
		}
	}
}

// tailFlockdLogs returns the last ~20 lines of daemon output for the
// current platform: journalctl on Linux, the LogPath the service was
// pointed at on macOS (launchd StandardOutPath) and Windows (the logon
// task passes it as flockd --log-file). Best-effort — an empty return
// means "we couldn't get anything," not an error to surface to the user.
func tailFlockdLogs(ctx context.Context, logPath string) string {
	if runtime.GOOS == "linux" {
		out, err := exec.CommandContext(ctx,
			"journalctl", "--user", "-u", "flockd", "-n", "20", "--no-pager", "--output=cat",
		).Output()
		if err == nil {
			return string(out)
		}
	}
	// launchd (macOS) and any fallback path: read the log file the service
	// was pointed at. Read the whole file — startup crash logs are small.
	if logPath == "" {
		return ""
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

func cmdDown() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop the flockd service",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := svc.NewManager().Stop(cmd.Context()); err != nil {
				return err
			}
			fmt.Println(styleOK.Render("✓"), "flockd stopped")
			return nil
		},
	}
}

func cmdStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show node status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := newClient()
			if err != nil {
				return err
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			stateStyle := styleWarn
			if st.State == "serving" {
				stateStyle = styleOK
			}
			fmt.Println(styleTitle.Render("/// Teraflock node"), styleDim.Render(st.NodeId))
			fmt.Printf("  state      %s\n", stateStyle.Render(st.State))
			fmt.Printf("  version    %s%s\n", st.Version, map[bool]string{true: " (standalone)", false: ""}[st.Standalone])
			fmt.Printf("  uptime     %s\n", (time.Duration(st.UptimeSeconds) * time.Second).String())
			fmt.Printf("  model      %s (%d loaded)\n", st.DefaultModel, st.ModelsLoaded)
			fmt.Printf("  tok/s(1m)  %.1f   in-flight %d   total reqs %d\n",
				st.Stats.TokensPerSec1m, st.Inflight, st.Stats.TotalRequests)
			if st.Hardware != nil {
				gpus := make([]string, 0, len(st.Hardware.Gpus))
				for _, g := range st.Hardware.Gpus {
					gpus = append(gpus, fmt.Sprintf("%s (%s, %dGB)", g.Model, g.Accel, g.VramMb/1024))
				}
				fmt.Printf("  hardware   %s/%s · %s · %dGB RAM\n             %s\n",
					st.Hardware.Os, st.Hardware.Arch, st.Hardware.CpuModel,
					st.Hardware.RamMb/1024, strings.Join(gpus, ", "))
			}
			if st.RuntimeAccel != nil && *st.RuntimeAccel != "" {
				fmt.Printf("  runtime    llama-server %s build\n", *st.RuntimeAccel)
			}
			power := "AC power"
			if st.OnBattery {
				power = styleWarn.Render("on battery")
			}
			fmt.Printf("  power      %s\n", power)
			if st.Memory.BudgetMb > 0 || st.Memory.UsedMb > 0 {
				fmt.Printf("  memory     %.1fGB of %.1fGB budget used by models (%.0fGB total)\n",
					float64(st.Memory.UsedMb)/1024, float64(st.Memory.BudgetMb)/1024, float64(st.Memory.TotalMb)/1024)
			}
			if st.Disk.Dir != "" {
				budget := "unlimited"
				if st.Disk.BudgetBytes > 0 {
					budget = gb(st.Disk.BudgetBytes) + " budget"
				}
				partial := ""
				if st.Disk.PartialBytes > 0 {
					partial = fmt.Sprintf(" · %s partial", gb(st.Disk.PartialBytes))
				}
				fmt.Printf("  disk       %s models · %s · %s free%s\n             %s\n",
					gb(st.Disk.ModelsBytes), budget, gb(st.Disk.FreeBytes), partial, styleDim.Render(st.Disk.Dir))
			}
			if line := updateLine(st.Update); line != "" {
				fmt.Printf("  update     %s\n", styleWarn.Render(line))
			}
			return nil
		},
	}
}

func cmdLogin() *cobra.Command {
	var loginURL, claimCode, verifier string
	c := &cobra.Command{
		Use:   "login",
		Short: "Enroll this node via browser (PKCE loopback handoff)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --claim-code skips the browser entirely: headless boxes, SSH
			// sessions, and dev meshes have no browser to hand off to.
			if claimCode == "" {
				flow := &enroll.LoginFlow{LoginURL: loginURL}
				ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
				defer cancel()
				fmt.Println("Opening browser to claim this node…")
				res, openURL, err := flow.Run(ctx)
				if openURL != "" {
					fmt.Println(styleDim.Render("If the browser didn't open, visit:\n  " + openURL))
				}
				if err != nil {
					return err
				}
				claimCode, verifier = res.ClaimCode, res.Verifier
			}
			// The daemon consumes these on its next start (`tera`/`flockd` are
			// separate processes, so the data dir is the handoff). The
			// verifier is what makes a browser-flow code redeemable by this
			// machine only; --claim-code writes none (and clears a stale one).
			if err := enroll.SaveClaimCode(dataDir(), claimCode); err != nil {
				return err
			}
			if err := enroll.SaveClaimVerifier(dataDir(), verifier); err != nil {
				return err
			}
			fmt.Println(styleOK.Render("✓"), "claim code stored")
			// When the service is already installed and running, finish the
			// job: enrollment happens on daemon start, so restart it now
			// instead of telling the operator to.
			m := svc.NewManager()
			if st, err := m.Status(cmd.Context()); err == nil && st == svc.StatusRunning {
				fmt.Println("  restarting flockd to complete enrollment…")
				if err := m.Stop(cmd.Context()); err != nil {
					return fmt.Errorf("stop service: %w (run `tera down && tera up` manually)", err)
				}
				if err := m.Start(cmd.Context()); err != nil {
					return fmt.Errorf("start service: %w (run `tera up` manually)", err)
				}
				fmt.Println(styleOK.Render("✓"), "daemon restarted — check `tera status` in a moment")
				return nil
			}
			fmt.Println(styleDim.Render("  run `tera up` to start the daemon and complete enrollment"))
			return nil
		},
	}
	c.Flags().StringVar(&loginURL, "url", config.Default().Enroll.LoginURL, "signup/claim page URL")
	c.Flags().StringVar(&claimCode, "claim-code", "", "enroll with this claim code instead of opening a browser")
	return c
}

func cmdModels() *cobra.Command {
	c := &cobra.Command{Use: "models", Short: "Manage local models"}
	c.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List cached/loaded models",
			RunE: func(cmd *cobra.Command, _ []string) error {
				cl, err := newClient()
				if err != nil {
					return err
				}
				mr, err := cl.Models(cmd.Context())
				if err != nil {
					return err
				}
				if len(mr.Models) == 0 {
					fmt.Println(styleDim.Render("no models cached"))
					return nil
				}
				fmt.Println(modelsHeader())
				for _, m := range mr.Models {
					fmt.Println(modelRowLine(m))
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "pin <model-id>",
			Short: "Pin a model (exempt from LRU eviction)",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, err := newClient()
				if err != nil {
					return err
				}
				if err := cl.PinModel(cmd.Context(), args[0], true); err != nil {
					return err
				}
				fmt.Println(styleOK.Render("✓"), "pinned", args[0])
				return nil
			},
		},
		cmdModelsPull(), cmdModelsLoad(), cmdModelsUnload(), cmdModelsDefault(),
		&cobra.Command{
			Use:   "rm <model-id>",
			Short: "Remove a model from the local cache",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				cl, err := newClient()
				if err != nil {
					return err
				}
				if err := cl.DeleteModel(cmd.Context(), args[0]); err != nil {
					return err
				}
				fmt.Println(styleOK.Render("✓"), "removed", args[0])
				return nil
			},
		},
	)
	return c
}

func cmdLimits() *cobra.Command {
	var (
		policy     string
		battery    bool
		maxTemp    float64
		schedule   []string
		maxDisk    int64
		maxRAM     int64
		retention  int
		idleUnload int
		setAny     bool
	)
	c := &cobra.Command{
		Use:   "limits",
		Short: "Show or set resource-governance limits",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			lim, err := cl.Limits(cmd.Context())
			if err != nil {
				return err
			}
			setAny = cmd.Flags().Changed("serve") || cmd.Flags().Changed("serve-on-battery") ||
				cmd.Flags().Changed("max-temp") || cmd.Flags().Changed("schedule") ||
				cmd.Flags().Changed("max-disk-mb") || cmd.Flags().Changed("max-ram-mb") ||
				cmd.Flags().Changed("retention-days") || cmd.Flags().Changed("idle-unload")
			if setAny {
				if cmd.Flags().Changed("max-disk-mb") {
					lim.MaxDiskMb = &maxDisk
				}
				if cmd.Flags().Changed("max-ram-mb") {
					lim.MaxRamMb = &maxRAM
				}
				if cmd.Flags().Changed("retention-days") {
					lim.RetentionDays = &retention
				}
				if cmd.Flags().Changed("idle-unload") {
					lim.IdleUnloadSeconds = &idleUnload
				}
				if cmd.Flags().Changed("serve") {
					lim.ServePolicy = policy
				}
				if cmd.Flags().Changed("serve-on-battery") {
					lim.ServeOnBattery = battery
				}
				if cmd.Flags().Changed("max-temp") {
					lim.MaxTempCelsius = maxTemp
				}
				if cmd.Flags().Changed("schedule") {
					lim.Schedule = schedule
				}
				if lim, err = cl.UpdateLimits(cmd.Context(), lim); err != nil {
					return err
				}
				fmt.Println(styleOK.Render("✓"), "limits updated")
			}
			fmt.Printf("  serve policy       %s\n", lim.ServePolicy)
			fmt.Printf("  idle after         %ds\n", lim.IdleAfterSeconds)
			fmt.Printf("  yield grace        %ds\n", lim.YieldGraceSeconds)
			fmt.Printf("  serve on battery   %v\n", lim.ServeOnBattery)
			fmt.Printf("  max temp           %.0f°C\n", lim.MaxTempCelsius)
			fmt.Printf("  schedule           %s\n", strings.Join(lim.Schedule, ", "))
			if lim.MeshManaged != nil {
				fmt.Printf("  mesh managed       %v\n", *lim.MeshManaged)
			}
			if lim.MaxDiskMb != nil {
				fmt.Printf("  max disk           %d MB (0 = unlimited)\n", *lim.MaxDiskMb)
			}
			if lim.RetentionDays != nil {
				fmt.Printf("  retention          %d days (0 = never)\n", *lim.RetentionDays)
			}
			if lim.MaxRamMb != nil {
				fmt.Printf("  max ram            %d MB (0 = auto)\n", *lim.MaxRamMb)
			}
			if lim.IdleUnloadSeconds != nil {
				fmt.Printf("  idle unload        %ds (0 = never)\n", *lim.IdleUnloadSeconds)
			}
			return nil
		},
	}
	c.Flags().StringVar(&policy, "serve", "", "serve policy: always|idle-only|scheduled")
	c.Flags().Int64Var(&maxDisk, "max-disk-mb", 0, "model store budget in MB (0 = unlimited)")
	c.Flags().Int64Var(&maxRAM, "max-ram-mb", 0, "memory budget for loaded models in MB (0 = auto)")
	c.Flags().IntVar(&retention, "retention-days", 0, "evict unpinned models unused for N days (0 = never)")
	c.Flags().IntVar(&idleUnload, "idle-unload", 0, "unload idle models after N seconds (0 = never)")
	c.Flags().BoolVar(&battery, "serve-on-battery", false, "allow serving on battery")
	c.Flags().Float64Var(&maxTemp, "max-temp", 90, "thermal pause threshold °C (0 disables)")
	c.Flags().StringSliceVar(&schedule, "schedule", nil, "serving windows, e.g. 22:00-08:00")
	return c
}

func cmdEarnings() *cobra.Command {
	return &cobra.Command{
		Use:   "earnings",
		Short: "Show credits earned",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			e, err := cl.Earnings(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Println(styleTitle.Render("Earnings"))
			fmt.Printf("  credits          %.6f\n", e.EarnedCredits)
			fmt.Printf("  est USD          $%.6f\n", e.EstUsd)
			fmt.Printf("  est USD/day      $%.4f\n", e.EstUsdPerDay)
			fmt.Printf("  lifetime tokens  %d\n", e.LifetimeTokens)
			if e.Note != "" {
				fmt.Println(styleDim.Render("  note: " + e.Note))
			}
			return nil
		},
	}
}

// defaultRedeemURL is the customer console's redeem page (app.teraflock.com,
// plan 12). The page carries the current truth about payouts — the CLI only
// says where to go, so it never advertises a minimum or a rail that does
// not exist yet (flockd#32).
const defaultRedeemURL = "https://app.teraflock.com/redeem"

func cmdRedeem() *cobra.Command {
	var redeemURL string
	c := &cobra.Command{
		Use:   "redeem",
		Short: "Redeem earned credits (opens the console's redeem page)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "Redeem earned credits at", redeemURL)
			openOrPrint(cmd, redeemURL)
			return nil
		},
	}
	c.Flags().StringVar(&redeemURL, "url", defaultRedeemURL, "redeem page URL")
	return c
}

// openOrPrint opens url in the browser; when that fails (headless box, SSH
// session, no launcher) it says so and prints the URL to open by hand
// instead of silently doing nothing.
func openOrPrint(cmd *cobra.Command, url string) {
	if err := browser.Open(url); err != nil {
		fmt.Fprintln(cmd.OutOrStdout(), styleWarn.Render("could not open a browser:"), err)
		fmt.Fprintln(cmd.OutOrStdout(), styleDim.Render("  open "+url+" manually"))
	}
}

// cmdToken prints the local API bearer token. The web dashboard needs it
// pasted in, and every 401 from the CLI points here.
func cmdToken() *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Print the local API bearer token (for the web dashboard)",
		RunE: func(_ *cobra.Command, _ []string) error {
			dir := dataDir()
			path := filepath.Join(dir, client.TokenFile)
			raw, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("no token at %s — is this the data dir flockd uses? "+
					"Override with --data-dir or FLOCKD_DATA_DIR", path)
			}
			// Bare token on stdout so `tera token | pbcopy` works; the hint
			// goes to stderr so it never pollutes a pipe.
			fmt.Fprintln(os.Stderr, styleDim.Render("token from "+path+" — paste into http://127.0.0.1:7777"))
			fmt.Println(strings.TrimSpace(string(raw)))
			return nil
		},
	}
}

func cmdVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("tera", version)
		},
	}
}

func cmdUninstall() *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the flockd service (one-command clean uninstall)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := svc.NewManager().Uninstall(cmd.Context()); err != nil {
				return err
			}
			fmt.Println(styleOK.Render("✓"), "flockd service removed")
			if purge {
				dd := dataDir()
				if err := os.RemoveAll(dd); err != nil {
					return fmt.Errorf("purge %s: %w", dd, err)
				}
				fmt.Println(styleOK.Render("✓"), "data directory removed:", dd)
			} else {
				fmt.Println(styleDim.Render("  models, keys and config kept in " + dataDir() + " (use --purge to delete)"))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete the data directory (models, node key, token)")
	return c
}

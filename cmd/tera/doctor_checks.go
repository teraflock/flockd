package main

// The checks of `tera doctor`. Each is a plain function of a context and a
// doctorEnv: everything it learns about the machine comes through the
// small interfaces on that env, so a test hands it fakes and no check
// touches a real network, service manager or daemon. See doctor.go for the
// runner and doctor_probe.go for the production implementations.

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/enroll"
	"github.com/teraflock/flockd/internal/hardware"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/runtime/llamacpp"
	"github.com/teraflock/flockd/internal/svc"
	"github.com/teraflock/flockd/internal/update"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// ---- what a check can ask about the machine ----

// installFacts is where the binaries are. It is gathered once, before the
// checks run, because every remedy needs to know how to spell `tera`.
type installFacts struct {
	// TeraPath is this binary.
	TeraPath string
	// OnPath: typing `tera` in a shell runs this binary.
	OnPath bool
	// OtherTera is a different tera that PATH resolves to instead.
	OtherTera string
	// FlockdPath is the daemon binary `tera up` would install (next to
	// tera, then PATH); FlockdErr is set when there is none.
	FlockdPath string
	FlockdErr  error
}

// doctorFlockd asks a flockd binary for its version.
type doctorFlockd interface {
	Version(ctx context.Context, path string) (string, error)
}

// doctorService is the read-only part of svc.Manager.
type doctorService interface {
	Status(ctx context.Context) (svc.Status, error)
}

// doctorLinger reads systemd lingering (svc.Linger). applies is false off
// Linux.
type doctorLinger interface {
	Linger(ctx context.Context) (user string, on, known, applies bool)
}

// doctorDaemon is the part of the local API client the checks read.
type doctorDaemon interface {
	Status(ctx context.Context) (gen.Status, error)
	Models(ctx context.Context) (gen.ModelList, error)
	Earnings(ctx context.Context) (gen.Earnings, error)
	Catalog(ctx context.Context, refresh bool) (gen.CatalogList, error)
}

// doctorLogs returns the last lines of the daemon's own log ("" = none).
type doctorLogs interface {
	Tail(ctx context.Context) string
}

// doctorEnroll reads the enrollment state in the data dir. Credentials
// returns an error matching os.ErrNotExist when the node never enrolled.
type doctorEnroll interface {
	Credentials() (*enroll.Credentials, error)
	// ClaimPending: a claim code is stored and waits for the daemon.
	ClaimPending() bool
}

// doctorMesh opens one connection to the coordinator and says how far it
// got (doctor_probe.go).
type doctorMesh interface {
	Probe(ctx context.Context, t meshTarget) meshProbe
}

// doctorHardware is hardware detection as the daemon does it at boot.
type doctorHardware interface {
	Detect(ctx context.Context) (*typesv1.CapabilityProfile, error)
	// Lanes is the accelerator preference chain for the profile.
	Lanes(p *typesv1.CapabilityProfile) []string
	NVIDIA(ctx context.Context) []hardware.NVIDIAGPU
}

// doctorRuntime resolves, without downloading it, the llama-server build
// the daemon would run for a lane chain.
type doctorRuntime interface {
	Resolve(ctx context.Context, lanes ...string) (llamacpp.Selection, error)
}

// doctorDisk reports free space on the volume holding a path.
type doctorDisk interface {
	FreeBytes(path string) (uint64, error)
}

// doctorEnv is everything the checks see. Production wiring is
// newDoctorEnv; tests fill the fields with fakes.
type doctorEnv struct {
	ctx context.Context // parent of the probes several checks share
	now func() time.Time

	goos, goarch string
	home         string
	teraVersion  string

	cfg     config.Config
	cfgErr  error  // the configuration did not load; cfg is the defaults
	cfgPath string // where the configuration file is (or would be)
	dataDir string
	apiBase string
	// apiFromConfig: apiBase came from local_api.listen, not --api.
	apiFromConfig bool
	// tokenSource is where the local token was found, or where it was
	// looked for (client.Resolved.TokenSource). Never the token itself.
	tokenSource string

	install  installFacts
	flockd   doctorFlockd
	service  doctorService
	linger   doctorLinger
	daemon   doctorDaemon
	logs     doctorLogs
	enroll   doctorEnroll
	mesh     doctorMesh
	hardware doctorHardware
	runtime  doctorRuntime
	disk     doctorDisk

	daemonOnce sync.Once
	daemonSnap *daemonSnapshot
	svcOnce    sync.Once
	svcStatus  svc.Status
	svcErr     error
	flockdOnce sync.Once
	flockdVer  string
	flockdErr  error
}

// Budgets of the probes inside a check; each check's own timeout
// (doctorChecks) is the sum of what it may wait for, plus slack.
const (
	doctorStatusTimeout   = 3 * time.Second
	doctorAPITimeout      = 4 * time.Second
	doctorServiceTimeout  = 8 * time.Second // Windows asks PowerShell, which starts slowly
	doctorFlockdTimeout   = 4 * time.Second
	doctorDetectTimeout   = 10 * time.Second
	doctorManifestTimeout = 6 * time.Second
	doctorMeshTimeout     = 12 * time.Second
)

// doctorReleasesURL is where every install method's files live.
const doctorReleasesURL = "https://github.com/teraflock/flockd/releases/latest"

// cmd spells a tera command for a remedy, by full path when `tera` alone
// would not run this binary.
func (e *doctorEnv) cmd(args string) string {
	t := "tera"
	if !e.install.OnPath && e.install.TeraPath != "" {
		t = e.install.TeraPath
		if strings.ContainsAny(t, " \t") {
			t = `"` + t + `"`
		}
	}
	return "`" + t + " " + args + "`"
}

// restart is the remedy fragment for "stop and start the service".
func (e *doctorEnv) restart() string {
	return e.cmd("down") + ", then " + e.cmd("up")
}

// headline is the context line under the report title.
func (e *doctorEnv) headline() string {
	return fmt.Sprintf("tera %s, data in %s", e.teraVersion, e.dataDir)
}

func (e *doctorEnv) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e *doctorEnv) parent() context.Context {
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// daemonSnapshot is one read of the local API, shared by the checks that
// need it so they agree with each other and the daemon is asked once.
type daemonSnapshot struct {
	Status gen.Status
	// Err is nil when the daemon answered /status.
	Err error

	Models    []gen.ModelRow
	ModelsErr error

	Earnings    gen.Earnings
	EarningsErr error
}

func (s *daemonSnapshot) up() bool { return s.Err == nil }

// daemonState reads the local API once. It runs under the command's
// context, not the calling check's, because its result outlives that
// check; its own deadlines keep it well inside every caller's timeout.
func (e *doctorEnv) daemonState() *daemonSnapshot {
	e.daemonOnce.Do(func() {
		snap := &daemonSnapshot{}
		e.daemonSnap = snap
		defer func() {
			if r := recover(); r != nil {
				snap.Err = fmt.Errorf("reading the local API crashed: %v", r)
			}
		}()
		if e.daemon == nil {
			snap.Err = errors.New("no local API client")
			return
		}
		ctx, cancel := context.WithTimeout(e.parent(), doctorStatusTimeout)
		snap.Status, snap.Err = e.daemon.Status(ctx)
		cancel()
		if snap.Err != nil {
			return
		}
		ctx, cancel = context.WithTimeout(e.parent(), doctorAPITimeout)
		defer cancel()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					snap.ModelsErr = fmt.Errorf("listing models crashed: %v", r)
				}
			}()
			var ml gen.ModelList
			ml, snap.ModelsErr = e.daemon.Models(ctx)
			snap.Models = ml.Models
		}()
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					snap.EarningsErr = fmt.Errorf("reading earnings crashed: %v", r)
				}
			}()
			snap.Earnings, snap.EarningsErr = e.daemon.Earnings(ctx)
		}()
		wg.Wait()
	})
	return e.daemonSnap
}

// serviceState asks the service manager once.
func (e *doctorEnv) serviceState() (svc.Status, error) {
	e.svcOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				e.svcStatus, e.svcErr = svc.StatusUnknown, fmt.Errorf("asking the service manager crashed: %v", r)
			}
		}()
		if e.service == nil {
			e.svcStatus, e.svcErr = svc.StatusUnknown, svc.ErrUnsupported
			return
		}
		ctx, cancel := context.WithTimeout(e.parent(), doctorServiceTimeout)
		defer cancel()
		e.svcStatus, e.svcErr = e.service.Status(ctx)
	})
	return e.svcStatus, e.svcErr
}

// flockdVersion runs the installed flockd for its version, once.
func (e *doctorEnv) flockdVersion() (string, error) {
	e.flockdOnce.Do(func() {
		defer func() {
			if r := recover(); r != nil {
				e.flockdVer, e.flockdErr = "", fmt.Errorf("asking flockd for its version crashed: %v", r)
			}
		}()
		if e.install.FlockdPath == "" || e.install.FlockdErr != nil || e.flockd == nil {
			e.flockdErr = errors.New("flockd not found")
			return
		}
		ctx, cancel := context.WithTimeout(e.parent(), doctorFlockdTimeout)
		defer cancel()
		e.flockdVer, e.flockdErr = e.flockd.Version(ctx, e.install.FlockdPath)
	})
	return e.flockdVer, e.flockdErr
}

// ---- 1. install ----

// checkInstall: are tera and flockd both here, do they match, does the
// configuration load, and is tera reachable as a bare command.
func checkInstall(_ context.Context, env *doctorEnv) doctorResult {
	var f finding
	in := env.install
	teraDir := "its own directory"
	if in.TeraPath != "" {
		teraDir = filepath.Dir(in.TeraPath)
	}

	if in.FlockdErr != nil || in.FlockdPath == "" {
		f.raise(doctorFail,
			"Install flockd next to tera (in "+teraDir+") or on your PATH: reinstall Teraflock from "+doctorReleasesURL+".",
			"tera %s is at %s, but the flockd daemon was not found next to it or on your PATH.", env.teraVersion, in.TeraPath)
	} else {
		ver, err := env.flockdVersion()
		switch {
		case err != nil:
			f.raise(doctorWarn,
				"Run `"+in.FlockdPath+" -version` in a terminal to see why it does not start.",
				"tera %s is at %s. flockd is at %s but did not report its version (%v).", env.teraVersion, in.TeraPath, in.FlockdPath, err)
		case filepath.Dir(in.FlockdPath) == teraDir:
			f.note("tera %s and flockd %s are installed in %s.", env.teraVersion, ver, teraDir)
		default:
			f.note("tera %s is at %s; flockd %s is at %s (found on your PATH).", env.teraVersion, in.TeraPath, ver, in.FlockdPath)
		}
		if err == nil && !sameVersion(ver, env.teraVersion) {
			f.raise(doctorWarn,
				"Reinstall Teraflock so tera and flockd come from the same release ("+doctorReleasesURL+").",
				"tera and flockd are released together and these two versions differ.")
		}
	}

	if env.cfgErr != nil {
		f.raise(doctorFail,
			"Fix or remove "+env.cfgPath+".",
			"The configuration does not load, and flockd refuses to start with it: %v.", env.cfgErr)
	}

	switch {
	case in.OnPath:
	case in.TeraPath == "":
	default:
		line := "tera is not on your PATH, so run it by its full path: " + in.TeraPath + "."
		if env.home != "" && teraDir == filepath.Join(env.home, ".teraflock", "bin") {
			line += " The desktop app installs tera and flockd there, and that directory is not on PATH."
		}
		if in.OtherTera != "" {
			line += " Typing `tera` runs a different copy: " + in.OtherTera + "."
		}
		f.note("%s", line)
	}
	return f.result()
}

// ---- 2. service ----

// serviceName is what the per-user service is called on this OS.
func serviceName(goos string) string {
	switch goos {
	case "darwin":
		return "launchd service"
	case "linux":
		return "systemd user service"
	case "windows":
		return "logon task"
	}
	return "service"
}

// checkService: is the per-user service installed and running, and on
// Linux will it survive a logout.
func checkService(ctx context.Context, env *doctorEnv) doctorResult {
	name := serviceName(env.goos)
	st, err := env.serviceState()
	if errors.Is(err, svc.ErrUnsupported) {
		return doctorSkipped("Service management is not available on %s.", env.goos)
	}
	var f finding
	switch {
	case err != nil || st == svc.StatusUnknown:
		reason := "the service manager gave no usable answer"
		if err != nil {
			reason = err.Error()
		}
		f.raise(doctorWarn,
			"Run "+env.cmd("doctor")+" again; if it stays, "+env.cmd("up")+" reinstalls the service.",
			"Could not read the state of the %s: %s.", name, reason)
	case st == svc.StatusNotInstalled:
		if env.daemonState().up() {
			f.raise(doctorWarn,
				"Run "+env.cmd("up")+" to install the service, so the node starts by itself.",
				"The %s is not installed. A flockd that was started by hand is answering, and it will not come back after a restart or logout.", name)
		} else {
			f.raise(doctorFail,
				"Run "+env.cmd("up")+".",
				"The %s is not installed, so nothing starts flockd on this machine.", name)
		}
	case st == svc.StatusStopped:
		f.raise(doctorFail,
			"Run "+env.cmd("up")+".",
			"The %s is installed but not running.", name)
	default:
		f.note("The %s is installed and running.", name)
		checkLinger(ctx, env, &f)
	}
	return f.result()
}

// checkLinger adds the Linux lingering answer to the service finding.
func checkLinger(ctx context.Context, env *doctorEnv, f *finding) {
	if env.linger == nil {
		return
	}
	user, on, known, applies := env.linger.Linger(ctx)
	if !applies {
		return
	}
	who := user
	if who == "" {
		who = "$USER"
	}
	switch {
	case !known:
		f.note("Could not read whether lingering is enabled (`loginctl show-user %s --property=Linger`); without it the node stops when you log out.", who)
	case on:
		f.note("Lingering is enabled for %s, so the node keeps running after you log out.", who)
	default:
		f.raise(doctorWarn,
			"Run `sudo loginctl enable-linger "+who+"`.",
			"Lingering is off for %s: systemd stops the node when your last session ends (for example when you close SSH).", who)
	}
}

// ---- 3. daemon ----

// stateReading is how a status state reads to an operator.
type stateReading struct {
	status doctorStatus
	text   string
	remedy string
}

// readState explains the daemon's state. A node that holds back because
// its own policy says so (someone is at the keyboard, it is on battery, it
// is outside its schedule) is working as configured: that passes, with the
// reason and the command that changes the policy in the sentence. Only a
// state the operator did not choose is a warning.
func readState(env *doctorEnv, st gen.Status) stateReading {
	switch st.State {
	case "serving":
		return stateReading{doctorPass, "It is serving.", ""}
	case "starting":
		return stateReading{doctorWarn,
			"It is still fetching or loading its default model, so it is not serving yet.",
			"Wait for the load to finish; " + env.cmd("logs -f") + " follows it."}
	case "idle":
		return stateReading{doctorPass, "It is idle: no model is loaded right now, and one loads on the first request or placement.", ""}
	case "no-model":
		return stateReading{doctorPass, "It has no model to serve yet (see Models).", ""}
	case "yielded":
		return stateReading{doctorPass,
			"It is paused because this computer is in use: under the idle-only policy it serves when the keyboard and mouse have been quiet for a while. To serve while you work, run " + env.cmd("limits --serve always") + ".", ""}
	case "paused-battery":
		return stateReading{doctorPass,
			"It is paused because this computer is on battery power and serving on battery is off. Plug it in, or allow it with " + env.cmd("limits --serve-on-battery") + ".", ""}
	case "paused-thermal":
		return stateReading{doctorWarn,
			fmt.Sprintf("It is paused because the machine is too hot (%.0f°C); it resumes when it cools down.", st.TempCelsius),
			"Let the machine cool down. The threshold is " + env.cmd("limits --max-temp <°C>") + "."}
	case "outside-schedule":
		return stateReading{doctorPass,
			"It is paused because it is outside its serving schedule. Change the windows with " + env.cmd("limits --schedule <HH:MM-HH:MM>") + ", or serve at any time with " + env.cmd("limits --serve always") + ".", ""}
	}
	return stateReading{doctorPass, "Its state is " + st.State + ".", ""}
}

// logHint says where the daemon's own log is, for a daemon that does not
// answer (`tera logs` reads it through the API, so it cannot help then).
func (e *doctorEnv) logHint() string {
	if e.goos == "linux" {
		return "`journalctl --user -u flockd -n 50`"
	}
	return filepath.Join(e.dataDir, "flockd.log")
}

// lastBrokenLine is the most recent log line that shows a boot failure
// (logLooksBroken's rule), shortened for one line of output.
func lastBrokenLine(tail string) string {
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if logLooksBroken(lines[i]) {
			line := strings.TrimSpace(lines[i])
			if r := []rune(line); len(r) > 240 {
				line = string(r[:239]) + "…"
			}
			return line
		}
	}
	return ""
}

// checkDaemon: does the local API answer with the local token, and what
// does the daemon say it is doing.
func checkDaemon(ctx context.Context, env *doctorEnv) doctorResult {
	var f finding
	snap := env.daemonState()
	if !snap.up() {
		daemonDown(ctx, env, snap.Err, &f)
		return f.result()
	}
	st := snap.Status

	mode := ""
	if st.Standalone {
		mode = " (standalone)"
	}
	f.note("flockd %s%s answers on %s and has been up for %s.", st.Version, mode, env.apiBase, uptimeText(time.Duration(st.UptimeSeconds)*time.Second))
	if env.apiFromConfig && env.apiBase != client.DefaultBase {
		f.note("That address comes from local_api.listen; other tera commands need `--api %s`.", env.apiBase)
	}

	r := readState(env, st)
	f.raise(r.status, r.remedy, "%s", r.text)

	if st.Standalone {
		f.raise(doctorWarn,
			"To join the mesh, restart it without --standalone: "+env.restart()+".",
			"It runs standalone, with a built-in stand-in for the coordinator: it is not connected to the mesh and earns nothing.")
	}
	if ver, err := env.flockdVersion(); err == nil && st.Version != "" && !sameVersion(ver, st.Version) {
		switch {
		case !update.IsRelease(ver) || !update.IsRelease(st.Version):
			// A development build on either side: say it, judge nothing.
			f.note("The flockd next to this tera is %s, not the build that is running.", ver)
		case update.Compare(ver, st.Version) > 0:
			f.raise(doctorWarn,
				"Restart the node so it runs the new version: "+env.restart()+".",
				"The running daemon is still flockd %s, but flockd %s is installed: the node was updated without a restart.", st.Version, ver)
		default:
			f.raise(doctorWarn,
				"Use the tera that was installed with the running daemon (the desktop app keeps its own in ~/.teraflock/bin), and remove this older copy. Do not run "+env.cmd("up")+" from this copy: it would start the older flockd.",
				"This tera and the flockd next to it (%s) are older than the running daemon (%s): the node runs from another install.", ver, st.Version)
		}
	}
	return f.result()
}

// sameVersion compares two version strings, taking "v0.9.3" and "0.9.3"
// as equal.
func sameVersion(a, b string) bool {
	if update.IsRelease(a) && update.IsRelease(b) {
		return update.Compare(a, b) == 0
	}
	return a == b
}

// uptimeText renders an uptime: exact while it is short, in days after.
func uptimeText(d time.Duration) string {
	if d >= 48*time.Hour {
		return humanSpan(d)
	}
	return shortDuration(d)
}

// daemonDown explains a local API that did not answer /status: something
// answered but not usefully (wrong token, not flockd, an error), the
// daemon is too busy to answer, or nothing is listening at all.
func daemonDown(ctx context.Context, env *doctorEnv, err error, f *finding) {
	var apiErr *client.APIError
	var unreachable *client.UnreachableError
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
		source := env.tokenSource
		if source == "" {
			source = "the data directory " + env.dataDir
		}
		detail := fmt.Sprintf("A daemon answers on %s but rejects the local token (%s).", env.apiBase, source)
		if strings.HasPrefix(source, "no token found") {
			detail = fmt.Sprintf("A daemon answers on %s, but this machine has no local token to show it (%s).", env.apiBase, source)
		}
		f.raise(doctorFail,
			"Point tera at the data directory flockd uses: "+env.cmd("--data-dir <dir> doctor")+" (or set FLOCKD_DATA_DIR).",
			"%s The token is per install and lives in the daemon's data directory.", detail)
		return
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		f.raise(doctorFail,
			"Stop the other program, or move flockd to another port with local_api.listen in "+env.cfgPath+".",
			"Something answers on %s, but not like flockd does (HTTP 404): another program may be using the port.", env.apiBase)
		return
	case errors.As(err, &apiErr):
		f.raise(doctorFail,
			"Read the daemon's log for the cause: "+env.logHint()+".",
			"A daemon answers on %s but reports an error: %s.", env.apiBase, apiErr.Error())
		return
	case errors.Is(err, context.DeadlineExceeded):
		f.raise(doctorWarn,
			"Run "+env.cmd("doctor")+" again. If it stays this way, restart the node: "+env.restart()+".",
			"A daemon accepted the connection on %s but did not answer within %s: it is very busy or stuck.", env.apiBase, doctorStatusTimeout)
		return
	case !errors.As(err, &unreachable):
		f.raise(doctorFail,
			"Read the daemon's log for the cause: "+env.logHint()+".",
			"The local API on %s gave an answer tera cannot use: %v.", env.apiBase, err)
		return
	}

	// Nothing is listening. Whether that is "not started", "still
	// starting" or "crashing" is the service manager's and the log's to
	// say. The log line is quoted as the last error, not as the cause: the
	// tail may predate this stop.
	svcState, _ := env.serviceState()
	tail := ""
	if env.logs != nil {
		tail = env.logs.Tail(ctx)
	}
	lastErr := ""
	if line := lastBrokenLine(tail); line != "" {
		lastErr = " The last error in its log: " + line
	}
	switch {
	case svcState == svc.StatusRunning && lastErr != "":
		f.raise(doctorFail,
			"Read the daemon's log: "+env.logHint()+". After fixing the cause, run "+env.restart()+".",
			"The service is running but no daemon answers on %s: flockd is probably failing at start-up and being restarted.%s", env.apiBase, lastErr)
	case svcState == svc.StatusRunning:
		f.raise(doctorWarn,
			"Check again in a minute. If it stays this way, read the daemon's log: "+env.logHint()+".",
			"The service is running but no daemon answers on %s yet. The local API opens once the default model is loaded, which can take a minute for a large model.", env.apiBase)
	default:
		f.raise(doctorFail,
			"Run "+env.cmd("up")+".",
			"No daemon answers on %s: flockd is not running.%s", env.apiBase, lastErr)
	}
}

// ---- 4. enrollment ----

// certExpiry reads the client certificate's own NotAfter, falling back to
// the expiry recorded beside it.
func certExpiry(c *enroll.Credentials) time.Time {
	if block, _ := pem.Decode(c.ClientCertPEM); block != nil && block.Type == "CERTIFICATE" {
		if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
			return cert.NotAfter
		}
	}
	return c.CertExpiresAt
}

// humanSpan renders a positive duration for a sentence: "25 days",
// "1 day", "7 hours", "less than an hour".
func humanSpan(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= 24*time.Hour:
		return "1 day"
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	case d >= time.Hour:
		return "1 hour"
	}
	return "less than an hour"
}

const doctorDateLayout = "2 Jan 2006"

// checkEnrollment: is the node claimed, as which node, and how long is its
// client certificate good for.
func checkEnrollment(_ context.Context, env *doctorEnv) doctorResult {
	var f finding
	loginHint := " (it asks for a claim code from " + consoleConnectURL + ")"
	creds, err := env.enroll.Credentials()
	switch {
	case errors.Is(err, os.ErrNotExist):
		snap := env.daemonState()
		switch {
		case env.enroll.ClaimPending():
			f.raise(doctorFail,
				"Run "+env.cmd("up")+" so the daemon finishes enrolling. If it refuses the code (codes are single use and expire), get a new one with "+env.cmd("login")+".",
				"A claim code is stored, but the node is not enrolled yet: the daemon exchanges the code for a certificate when it starts.")
		case snap.up() && snap.Status.Standalone:
			f.raise(doctorWarn,
				"To join the mesh, run "+env.cmd("login")+loginHint+".",
				"This node is not enrolled. Standalone mode needs no account, but a node only earns on the mesh.")
		default:
			f.raise(doctorFail,
				"Run "+env.cmd("login")+loginHint+".",
				"This node is not enrolled, so the mesh sends it no work.")
		}
		return f.result()
	case err != nil:
		f.raise(doctorFail,
			"Claim the node again: "+env.cmd("login --force")+loginHint+".",
			"The enrollment file in %s cannot be read: %v.", env.dataDir, err)
		return f.result()
	}

	now := env.clock()
	expiry := certExpiry(creds)
	left := expiry.Sub(now)
	when := expiry.Local().Format(doctorDateLayout)
	switch {
	case expiry.IsZero():
		f.note("Enrolled, with node id %s. The certificate's expiry date could not be read.", creds.NodeID)
	case left <= 0:
		f.raise(doctorFail,
			"Claim the node again: "+env.cmd("login --force")+loginHint+".",
			"Enrolled, with node id %s, but its client certificate expired on %s, so the mesh refuses it.", creds.NodeID, when)
	case left <= enroll.RotationWindow:
		f.raise(doctorWarn,
			"Restart the node so it renews the certificate: "+env.restart()+". If the renewal keeps failing, claim the node again with "+env.cmd("login --force")+".",
			"Enrolled, with node id %s. Its client certificate expires in %s (%s). The daemon renews it when it starts during the last %d days, and that has not happened yet.",
			creds.NodeID, humanSpan(left), when, int(enroll.RotationWindow.Hours()/24))
	default:
		f.note("Enrolled, with node id %s. Its client certificate is valid until %s (%s left).", creds.NodeID, when, humanSpan(left))
	}

	// What the running daemon makes of the same files.
	if snap := env.daemonState(); snap.up() && !snap.Status.Standalone {
		st := snap.Status
		if st.CertRotationError != nil && *st.CertRotationError != "" {
			f.raise(doctorWarn,
				"Restart the node to retry: "+env.restart()+". If it keeps failing, claim the node again with "+env.cmd("login --force")+".",
				"The daemon's last attempt to renew the certificate failed: %s.", *st.CertRotationError)
		}
		switch {
		case !st.Enrolled:
			f.raise(doctorWarn,
				"Restart the node so it picks the enrollment up: "+env.restart()+".",
				"The running daemon was started before this enrollment and is not using it.")
		case st.NodeId != "" && st.NodeId != creds.NodeID:
			f.raise(doctorWarn,
				"Restart the node so it uses the current enrollment: "+env.restart()+".",
				"The running daemon is still enrolled with node id %s.", st.NodeId)
		}
	}
	return f.result()
}

// ---- 5. mesh ----

// meshTarget is what to dial and how to judge the server's certificate:
// the same roots and switches the daemon's own dial uses, and never the
// node's key or certificate.
type meshTarget struct {
	Addr string // tunnel.coordinator_addr, host:port
	// Plaintext: tunnel.insecure, a development coordinator without TLS.
	Plaintext          bool
	RootCAs            *x509.CertPool
	InsecureSkipVerify bool
	// MinTLS is the lowest TLS version to offer (0 = TLS 1.3, what the
	// daemon requires).
	MinTLS uint16
}

// Stages of a mesh probe, in order.
const (
	meshStageAddr = "address"
	meshStageDNS  = "dns"
	meshStageTCP  = "tcp"
	meshStageTLS  = "tls"
)

// meshProbe is how far one connection attempt got.
type meshProbe struct {
	Host, Port string
	// Stage is where it stopped ("" = everything completed) and Err why.
	Stage string
	Err   error
	IPs   []string
	// TLSVersion is the negotiated version ("TLS 1.3") after a completed
	// handshake.
	TLSVersion string
	// ServerVerified: the server's certificate chain passed verification
	// before anything went wrong.
	ServerVerified bool
	// CertRequested: the server asked for a client certificate. The probe
	// answers with none.
	CertRequested bool
	Elapsed       time.Duration
}

// alertsForMissingClientCert are the TLS alerts a server sends when it
// required a client certificate and got none: bad_certificate (42),
// certificate_unknown (46), unknown_ca (48), access_denied (49),
// handshake_failure (40, what OpenSSL-family servers send under TLS 1.2)
// and certificate_required (116, TLS 1.3).
var alertsForMissingClientCert = map[int]bool{40: true, 42: true, 46: true, 48: true, 49: true, 116: true}

// readMesh turns a probe into a verdict. The one subtle case: a server
// that requires a client certificate ends the handshake with an alert
// when it gets none. That is the mesh answering, and the probe presents no
// certificate on purpose, so it counts as reachable — but only when the
// server's own certificate had already verified and it had asked for
// ours; any other refused handshake is a failure.
func readMesh(env *doctorEnv, p meshProbe) (st doctorStatus, text, remedy string) {
	addr := p.Host
	if p.Port != "" {
		addr = p.Host + ":" + p.Port
	}
	ips := ""
	if len(p.IPs) > 0 && p.IPs[0] != p.Host {
		shown := p.IPs
		if len(shown) > 3 {
			shown = shown[:3]
		}
		ips = " (" + strings.Join(shown, ", ") + ")"
	}
	network := "Check this machine's internet connection, then run " + env.cmd("doctor") + " again."
	switch p.Stage {
	case meshStageAddr:
		return doctorFail,
			fmt.Sprintf("The coordinator address %q is not a host:port (%v).", p.Host, p.Err),
			"Fix tunnel.coordinator_addr in " + env.cfgPath + " (the default is " + config.Default().Tunnel.CoordinatorAddr + ")."
	case meshStageDNS:
		return doctorFail,
			fmt.Sprintf("Cannot look up %s: %s.", p.Host, meshErrText(p.Err)),
			network
	case meshStageTCP:
		what := fmt.Sprintf("%s resolves%s, but nothing answers on port %s", p.Host, ips, p.Port)
		if len(p.IPs) == 1 && p.IPs[0] == p.Host {
			what = "Nothing answers at " + addr // an IP address: there was no lookup
		}
		return doctorFail,
			fmt.Sprintf("%s: %s.", what, meshErrText(p.Err)),
			"Allow outbound TCP connections to " + addr + " (a firewall, VPN or proxy may be blocking them)."
	case meshStageTLS:
		code, alert, isAlert := remoteTLSAlert(p.Err)
		switch {
		case isAlert && p.ServerVerified && p.CertRequested && alertsForMissingClientCert[code]:
			return doctorPass,
				fmt.Sprintf("%s is reachable%s: DNS, TCP and its TLS certificate all check out. It then asked for the node's client certificate, which this check does not present, and closed the connection (%s), as expected.", addr, ips, alert),
				""
		case isAlert:
			return doctorFail,
				fmt.Sprintf("%s answers, but it refused the TLS handshake (%s).", addr, alert),
				"Run " + env.cmd("doctor") + " again in a few minutes; if it stays, something between this machine and the mesh is interfering with TLS (a proxy or security product)."
		case isCertError(p.Err):
			return doctorFail,
				fmt.Sprintf("%s answers, but the certificate it presents is not trusted: %s. Something between this machine and the mesh is intercepting the connection (a corporate proxy, a captive portal), or this machine's clock is wrong.", addr, meshErrText(p.Err)),
				"Check the date and time, and try a network without a proxy or captive portal. A self-hosted coordinator needs tunnel.ca_cert in " + env.cfgPath + "."
		}
		return doctorFail,
			fmt.Sprintf("%s answers on TCP, but the TLS handshake failed: %s.", addr, meshErrText(p.Err)),
			network
	}
	how := "DNS, TCP"
	switch {
	case p.TLSVersion != "" && p.CertRequested:
		how += " and the " + p.TLSVersion + " handshake succeed, and it asks for the node's client certificate as expected"
	case p.TLSVersion != "":
		how += " and the " + p.TLSVersion + " handshake succeed"
	default:
		how += " succeed (plaintext: tunnel.insecure is set, for a development coordinator only)"
	}
	return doctorPass, fmt.Sprintf("%s is reachable%s: %s (%d ms).", addr, ips, how, p.Elapsed.Milliseconds()), ""
}

// meshSessionGrace is how long a daemon may be up before a missing ledger
// snapshot means anything: the coordinator pushes one every few minutes,
// and the daemon calls a snapshot stale after two missed pushes.
const meshSessionGrace = 15 * time.Minute

// checkMesh: can this machine reach the coordinator, and does the running
// daemon look connected to it.
func checkMesh(ctx context.Context, env *doctorEnv) doctorResult {
	snap := env.daemonState()
	if env.cfg.Tunnel.Standalone || (snap.up() && snap.Status.Standalone) {
		return doctorSkipped("The node runs standalone, with a built-in stand-in for the coordinator, so it makes no connection to the mesh.")
	}
	var f finding
	target := meshTarget{
		Addr:               env.cfg.Tunnel.CoordinatorAddr,
		Plaintext:          env.cfg.Tunnel.Insecure,
		InsecureSkipVerify: env.cfg.Tunnel.InsecureSkipVerify,
	}
	if !target.Plaintext {
		// The roots the daemon trusts for this dial: the system's, the
		// mesh CA from enrollment, and an operator-pinned CA. A pool that
		// cannot be built falls back to the system roots.
		var meshCA []byte
		if creds, err := env.enroll.Credentials(); err == nil && creds != nil {
			meshCA = creds.CACertPEM
		}
		pinned, _ := env.cfg.Tunnel.CACertPEM()
		if pool, err := enroll.RootPool(meshCA, pinned); err == nil {
			target.RootCAs = pool
		}
	}
	mctx, cancel := context.WithTimeout(ctx, doctorMeshTimeout)
	defer cancel()
	st, text, remedy := readMesh(env, env.mesh.Probe(mctx, target))
	f.raise(st, remedy, "%s", text)

	// Reaching the coordinator from here is not the daemon's own session.
	// The status has no field for that, but the earnings do: the daemon
	// reports ledger figures only while the coordinator keeps pushing
	// them over the tunnel.
	if snap.up() && snap.Status.Enrolled && snap.EarningsErr == nil {
		uptime := time.Duration(snap.Status.UptimeSeconds) * time.Second
		switch {
		case snap.Earnings.Source == gen.Ledger:
			f.note("The running daemon has a live session: the coordinator is sending it account updates.")
		case uptime >= meshSessionGrace:
			f.raise(doctorWarn,
				"Look for tunnel errors in "+env.cmd("logs")+"; if they persist, restart the node: "+env.restart()+".",
				"The running daemon has had no account update from the coordinator for several minutes, which usually means its own session to the mesh is down.")
		default:
			f.note("The running daemon started %s ago and has not had its first account update from the coordinator yet.", shortDuration(uptime))
		}
	}
	return f.result()
}

// ---- 6. hardware and runtime ----

// gbOf renders megabytes as whole gigabytes for a hardware line.
func gbOf(mb uint64) string {
	return fmt.Sprintf("%d GB", int(math.Round(float64(mb)/1024)))
}

// hardwareLines describes the profile: CPU and memory, then each GPU.
func hardwareLines(p *typesv1.CapabilityProfile) []string {
	cpu := strings.TrimSpace(p.GetCpuModel())
	if cpu == "" {
		cpu = p.GetOs() + "/" + p.GetArch() + " CPU"
	}
	lines := []string{fmt.Sprintf("%s, %d cores, %s of memory.", cpu, p.GetCpuCores(), gbOf(p.GetRamTotalMb()))}
	gpus := 0
	for _, g := range p.GetGpus() {
		if g.GetVendor() == "none" || g.GetVendor() == "" {
			continue
		}
		gpus++
		var parts []string
		if a := g.GetAccel(); a != "" {
			parts = append(parts, a)
		} else {
			parts = append(parts, "no accelerated lane of its own")
		}
		switch {
		case g.GetUnifiedMemory():
			parts = append(parts, "shares the system memory")
		case g.GetVramMb() > 0:
			parts = append(parts, gbOf(g.GetVramMb())+" of video memory")
		}
		if d := g.GetDriverVersion(); d != "" {
			parts = append(parts, "driver "+d)
		}
		lines = append(lines, fmt.Sprintf("GPU: %s (%s).", g.GetModel(), strings.Join(parts, ", ")))
	}
	if gpus == 0 {
		lines = append(lines, "No usable GPU was detected, so models run on the CPU.")
	}
	return lines
}

func isCPULane(lane string) bool { return lane == "cpu" || lane == "cpu-avx2" }

// checkRuntime: what hardware is here, which accelerator lanes that gives,
// and whether the pinned runtime manifest has a build this machine can run.
func checkRuntime(ctx context.Context, env *doctorEnv) doctorResult {
	var f finding
	dctx, cancel := context.WithTimeout(ctx, doctorDetectTimeout)
	hw, err := env.hardware.Detect(dctx)
	// A probe cut off by the deadline degrades to "no GPU": that profile
	// must not be judged as if it were the machine.
	cutOff := dctx.Err() != nil
	cancel()
	if err != nil || hw == nil || cutOff {
		reason := fmt.Sprintf("it took longer than %s", doctorDetectTimeout)
		if err != nil {
			reason = err.Error()
		}
		f.raise(doctorWarn,
			"Run "+env.cmd("doctor")+" again.",
			"Hardware detection did not complete (%s), so the runtime could not be checked.", reason)
		return f.result()
	}
	for _, line := range hardwareLines(hw) {
		f.note("%s", line)
	}
	lanes := env.hardware.Lanes(hw)
	hasGPULane := false
	for _, l := range lanes {
		if !isCPULane(l) {
			hasGPULane = true
		}
	}

	nvidia := env.hardware.NVIDIA(ctx)
	anyCUDA := false
	for _, g := range nvidia {
		anyCUDA = anyCUDA || g.RunsCUDA
	}
	for _, g := range nvidia {
		capText := "the driver does not report its compute capability"
		if g.ComputeCap != "" {
			capText = "compute capability " + g.ComputeCap
		}
		f.note("NVIDIA %s: driver %s, %s.", g.Name, g.DriverVersion, capText)
		if g.Warning == "" {
			continue
		}
		remedy := "Nothing to change on this machine: the card is older than the CUDA runtime supports."
		if env.goos == "linux" && !anyCUDA && !hasGPULane {
			remedy = "Install the Vulkan loader and the NVIDIA Vulkan driver to serve on this GPU through the Vulkan runtime, then restart the node: " + env.restart() + "."
		}
		// The sentence detection logs for this card, minus its log-style
		// lead-in (the line above already names the card as NVIDIA).
		f.raise(doctorWarn, remedy, "%s.", upperFirst(strings.TrimPrefix(g.Warning, "nvidia gpu ")))
	}

	platform := env.goos + "/" + env.goarch
	if env.cfg.Runtime.Kind != "llamacpp" {
		f.note("The %s runtime is configured (runtime.kind), so no llama-server build is needed; it produces synthetic tokens, for testing only.", env.cfg.Runtime.Kind)
		return f.result()
	}
	f.note("Accelerator lanes, in order of preference: %s.", strings.Join(lanes, " > "))

	rctx, cancel := context.WithTimeout(ctx, doctorManifestTimeout)
	defer cancel()
	sel, err := env.runtime.Resolve(rctx, lanes...)
	snap := env.daemonState()
	live := ""
	if snap.up() && snap.Status.RuntimeAccel != nil {
		live = *snap.Status.RuntimeAccel
	}
	switch {
	case err == nil:
	case env.goos == "darwin" && env.goarch == "amd64":
		f.raise(doctorFail,
			"Run the node on an Apple Silicon Mac, or on a Linux or Windows machine.",
			"Intel Macs are not supported: no llama-server build is published for darwin/amd64, so this machine cannot load a model.")
		return f.result()
	case errors.Is(err, llamacpp.ErrNoBuild):
		f.raise(doctorFail,
			"Wait for a build to be published for this platform, or set runtime.llama_server_path in "+env.cfgPath+" to a llama-server you built.",
			"The pinned runtime manifest has no llama-server build for %s, so this machine cannot load a model.", platform)
		return f.result()
	case env.cfg.Runtime.LlamaServerPath != "":
		f.raise(doctorFail,
			"Fix runtime.llama_server_path in "+env.cfgPath+", or remove it to use the published build.",
			"The configured llama-server cannot be used: %v.", err)
		return f.result()
	case live != "":
		f.raise(doctorWarn, manifestRemedy(env, err),
			"The runtime manifest could not be checked (%v). The running daemon already has its %s build, so it keeps serving.", err, live)
		return f.result()
	default:
		f.raise(doctorFail, manifestRemedy(env, err),
			"The runtime manifest could not be checked, so it is not known whether a llama-server build exists for %s: %v.", platform, err)
		return f.result()
	}

	if sel.Accel == "" {
		f.note("The node runs your own llama-server at %s (runtime.llama_server_path).", sel.Path)
		return f.result()
	}
	have := "it is downloaded on the next start"
	if sel.Path != "" {
		have = "it is already downloaded"
	}
	f.note("The pinned runtime manifest has a build for %s: %s (%s); %s.", platform, sel.BuildID, sel.Accel, have)
	if isCPULane(sel.Accel) && hasGPULane {
		f.raise(doctorWarn,
			"Nothing to change on this machine: once a GPU build is published for "+platform+", the node uses it from its next restart.",
			"A GPU lane was detected, but there is %s yet, so the node runs the CPU build and serves more slowly.", sel.Reason)
	} else if sel.Reason != "" && sel.Reason != "preferred" {
		f.note("That lane was chosen because there is %s.", sel.Reason)
	}
	if live != "" && live != sel.Accel {
		f.note("The running daemon uses its %s build until it is restarted.", live)
	}
	return f.result()
}

// manifestRemedy is the fix for a manifest that could not be used: the
// network when the fetch itself failed, the runtime settings when the
// manifest arrived and was refused (wrong URL, signature policy).
func manifestRemedy(env *doctorEnv, err error) string {
	var netErr net.Error
	if errors.As(err, &netErr) || errors.Is(err, context.DeadlineExceeded) {
		return "Check this machine's internet connection, then run " + env.cmd("doctor") + " again."
	}
	return "The manifest was reached and refused, so this is not the network. If you set runtime.artifact_manifest_url, runtime.artifact_signing_key or runtime.require_signature in " + env.cfgPath + ", check them; otherwise run " + env.cmd("doctor") + " again later."
}

// upperFirst capitalises a leading ASCII letter.
func upperFirst(s string) string {
	if s != "" && s[0] >= 'a' && s[0] <= 'z' {
		return string(s[0]-('a'-'A')) + s[1:]
	}
	return s
}

// ---- 7. models ----

const (
	// doctorDiskFloor is the free space below which the model store gets
	// a warning: less than the smallest catalog models need.
	doctorDiskFloor = 5 << 30
	// doctorDiskHeadroom is what a download needs beyond the model's own
	// size.
	doctorDiskHeadroom = 512 << 20
)

// modelPercent is download progress of a row, when it reports one.
func modelPercent(m gen.ModelRow) string {
	if m.ReceivedBytes == nil || m.SizeBytes <= 0 {
		return ""
	}
	return fmt.Sprintf(" (%d%%)", int(float64(*m.ReceivedBytes)/float64(m.SizeBytes)*100))
}

// checkModels: which model the node serves by default, whether anything is
// ready or loaded, and whether the disk has room.
func checkModels(ctx context.Context, env *doctorEnv) doctorResult {
	var f finding
	snap := env.daemonState()

	def := config.NormalizeDefaultModel(env.cfg.Models.Default)
	dir := filepath.Join(env.dataDir, "models")
	if snap.up() {
		def = snap.Status.DefaultModel
		if snap.Status.Disk.Dir != "" {
			dir = snap.Status.Disk.Dir
		}
	}

	needDefault := false // the default model still has to be downloaded
	switch {
	case !snap.up():
	case snap.ModelsErr != nil:
		f.raise(doctorWarn,
			"Run "+env.cmd("doctor")+" again.",
			"The daemon did not list its models: %v.", snap.ModelsErr)
	default:
		needDefault = modelFindings(env, snap.Models, def, &f)
	}

	// Free space, measured here; the daemon's own figure is the fallback.
	free, freeKnown := int64(0), false
	if b, err := env.disk.FreeBytes(existingDir(dir)); err == nil {
		free, freeKnown = int64(min(b, uint64(math.MaxInt64))), true
	} else if snap.up() && snap.Status.Disk.Dir != "" {
		free, freeKnown = snap.Status.Disk.FreeBytes, true
	}

	fits := true
	if needDefault && freeKnown && def != "" {
		if size := catalogSize(ctx, env, def); size > 0 && free < size+doctorDiskHeadroom {
			fits = false
			f.raise(doctorFail,
				"Free up disk space on that volume, or start the node with a smaller default model: "+env.cmd("up --default-model <id>")+".",
				"The default model %s needs %s, but only %s is free in %s: it does not fit.", def, gb(size), gb(free), dir)
		}
	}
	switch {
	case !freeKnown:
		f.note("Free disk space in %s could not be read.", dir)
	case !fits:
	case free < doctorDiskFloor:
		f.raise(doctorWarn,
			"Free up disk space on that volume, or remove a model you do not need with "+env.cmd("models rm <id>")+".",
			"Only %s is free in %s, which is too little for another model.", gb(free), dir)
	default:
		f.note("%s is free in %s.", gb(free), dir)
	}

	if !snap.up() && f.status == "" {
		// Nothing about the models themselves could be checked, and the
		// disk is fine: that is a skip, with what is known from here.
		what := "No default model is configured."
		if def != "" {
			what = "The configured default model is " + def + "."
		}
		return doctorSkipped("The daemon is not answering, so its models cannot be listed. %s %s", what, strings.Join(f.lines, " "))
	}
	return f.result()
}

// existingDir is path, or its nearest ancestor that exists: free space is
// a property of the volume, and the model directory is only created by
// the daemon's first start.
func existingDir(path string) string {
	for {
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

// modelFindings reads the model list. It reports whether the default
// model still has to be fetched in full (not on disk, not on its way).
func modelFindings(env *doctorEnv, rows []gen.ModelRow, def string, f *finding) (needDefault bool) {
	var loaded []string
	ready := 0
	var defRow *gen.ModelRow
	for i, m := range rows {
		if m.Loaded {
			loaded = append(loaded, m.Id)
		}
		if m.State == "ready" || m.Loaded {
			ready++
		}
		if def != "" && m.Id == def {
			defRow = &rows[i]
		}
	}
	if def == "" {
		f.note("No default model is set: the node serves what the mesh places on it and what you load.")
	} else {
		f.note("The default model is %s.", def)
	}

	switch {
	case len(loaded) == 1:
		f.note("1 model is loaded: %s.", loaded[0])
	case len(loaded) > 1:
		f.note("%d models are loaded: %s.", len(loaded), strings.Join(loaded, ", "))
	case ready == 1:
		f.note("Nothing is loaded right now; the 1 model on disk loads on the first request or placement.")
	case ready > 1:
		f.note("Nothing is loaded right now; the %d models on disk load on the first request or placement.", ready)
	}

	switch {
	case def == "":
		if ready == 0 {
			f.raise(doctorWarn,
				"Download one with "+env.cmd("models pull <id>")+", or wait for the mesh to place one.",
				"There is no model on this node yet, so it has nothing to serve.")
		}
	case defRow == nil:
		needDefault = true
		if ready == 0 {
			f.raise(doctorWarn,
				"Run "+env.cmd("models pull "+def)+".",
				"The default model is not on this node yet, and no other model is, so it has nothing to serve.")
		} else {
			f.note("The default model itself is not on this node; requests that name no model need it.")
		}
	case defRow.State == "downloading" || defRow.State == "assigned":
		f.raise(doctorWarn,
			"Wait for the download; "+env.cmd("dashboard")+" shows its progress.",
			"The default model is still downloading%s.", modelPercent(*defRow))
	case defRow.State == "missing":
		needDefault = true
		f.raise(doctorWarn,
			"Download it again with "+env.cmd("models pull "+def)+".",
			"The default model is in the node's index but its file is gone from disk.")
	}
	return needDefault
}

// catalogSize asks the daemon's catalog how large a model is (0 = not
// known). The daemon answers from its cached copy of the catalog.
func catalogSize(ctx context.Context, env *doctorEnv, id string) int64 {
	cctx, cancel := context.WithTimeout(ctx, doctorAPITimeout)
	defer cancel()
	cat, err := env.daemon.Catalog(cctx, false)
	if err != nil {
		return 0
	}
	for _, m := range cat.Models {
		if m.Id == id {
			return m.SizeBytes
		}
	}
	return 0
}

// ---- 8. update ----

// updateRemedy says how to update, by how this copy was installed.
func updateRemedy(env *doctorEnv, u gen.Update) string {
	url := doctorReleasesURL
	if u.Url != nil && *u.Url != "" {
		url = *u.Url
	}
	then := " Then restart the node: " + env.restart() + "."
	if env.home != "" && filepath.Dir(env.install.TeraPath) == filepath.Join(env.home, ".teraflock", "bin") {
		return "Update the Teraflock desktop app: it installs the matching tera and flockd (" + url + ")."
	}
	switch env.goos {
	case "darwin":
		return "Run `brew upgrade teraflock/tap/tera`, or install the package from " + url + "." + then
	case "linux":
		return "Run `curl -fsSL https://teraflock.ai/install.sh | sh`, or install the package from " + url + "." + then
	}
	return "Install the new release from " + url + "." + then
}

// checkUpdate: is the running daemon current, by the version feed result
// it already carries in its status.
func checkUpdate(_ context.Context, env *doctorEnv) doctorResult {
	snap := env.daemonState()
	if !snap.up() {
		return doctorSkipped("The daemon is not answering, and the version feed is read through it.")
	}
	u := snap.Status.Update
	if u == nil {
		return doctorSkipped("The daemon has not checked the version feed yet (it checks shortly after it starts, then every hour).")
	}
	var f finding
	current := u.Current
	if current == "" {
		current = snap.Status.Version
	}
	switch {
	case u.BelowMinimum != nil && *u.BelowMinimum:
		floor := "the mesh minimum"
		if u.Minimum != nil && *u.Minimum != "" {
			floor = "the mesh minimum, " + *u.Minimum
		}
		f.raise(doctorFail, updateRemedy(env, *u),
			"The node runs flockd %s, which is below %s: the mesh sends it no work until it is updated to %s or newer.", current, floor, u.Latest)
	case u.Available:
		f.raise(doctorWarn, updateRemedy(env, *u),
			"flockd %s is available; the node runs %s.", u.Latest, current)
	default:
		f.note("The node runs flockd %s, and the version feed has nothing newer.", current)
	}
	if !u.CheckedAt.IsZero() {
		if age := env.clock().Sub(u.CheckedAt); age > 6*time.Hour {
			f.note("The feed was last read %s ago.", humanSpan(age))
		}
	}
	return f.result()
}

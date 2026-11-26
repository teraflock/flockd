package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/enroll"
	"github.com/teraflock/flockd/internal/hardware"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/client/clienttest"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/runtime/llamacpp"
	"github.com/teraflock/flockd/internal/svc"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// ---- fakes: no network, no service manager, no daemon ----

type stubDaemon struct {
	status      gen.Status
	statusErr   error
	models      []gen.ModelRow
	modelsErr   error
	earnings    gen.Earnings
	earningsErr error
	catalog     []gen.CatalogEntry
	catalogErr  error
	statusCalls atomic.Int32
}

func (d *stubDaemon) Status(context.Context) (gen.Status, error) {
	d.statusCalls.Add(1)
	return d.status, d.statusErr
}
func (d *stubDaemon) Models(context.Context) (gen.ModelList, error) {
	return gen.ModelList{Models: d.models}, d.modelsErr
}
func (d *stubDaemon) Earnings(context.Context) (gen.Earnings, error) {
	return d.earnings, d.earningsErr
}
func (d *stubDaemon) Catalog(context.Context, bool) (gen.CatalogList, error) {
	return gen.CatalogList{Models: d.catalog}, d.catalogErr
}

type stubService struct {
	st  svc.Status
	err error
}

func (s stubService) Status(context.Context) (svc.Status, error) { return s.st, s.err }

type stubLinger struct {
	user               string
	on, known, applies bool
}

func (l stubLinger) Linger(context.Context) (string, bool, bool, bool) {
	return l.user, l.on, l.known, l.applies
}

type stubLogs string

func (l stubLogs) Tail(context.Context) string { return string(l) }

type stubEnroll struct {
	creds   *enroll.Credentials
	err     error
	pending bool
}

func (e stubEnroll) Credentials() (*enroll.Credentials, error) { return e.creds, e.err }
func (e stubEnroll) ClaimPending() bool                        { return e.pending }

type stubMesh struct {
	probe meshProbe
	got   *meshTarget
}

func (m stubMesh) Probe(_ context.Context, t meshTarget) meshProbe {
	if m.got != nil {
		*m.got = t
	}
	return m.probe
}

type stubHardware struct {
	profile *typesv1.CapabilityProfile
	err     error
	lanes   []string
	nvidia  []hardware.NVIDIAGPU
	// outlast makes Detect return only after its context expired, the way
	// a probe cut off by the deadline does.
	outlast bool
}

func (h stubHardware) Detect(ctx context.Context) (*typesv1.CapabilityProfile, error) {
	if h.outlast {
		<-ctx.Done()
	}
	return h.profile, h.err
}
func (h stubHardware) Lanes(*typesv1.CapabilityProfile) []string   { return h.lanes }
func (h stubHardware) NVIDIA(context.Context) []hardware.NVIDIAGPU { return h.nvidia }
func (r stubRuntime) Resolve(_ context.Context, lanes ...string) (llamacpp.Selection, error) {
	if r.gotLanes != nil {
		*r.gotLanes = lanes
	}
	return r.sel, r.err
}

type stubRuntime struct {
	sel      llamacpp.Selection
	err      error
	gotLanes *[]string
}

type stubDisk struct {
	free uint64
	err  error
}

func (d stubDisk) FreeBytes(string) (uint64, error) { return d.free, d.err }

type stubFlockd struct {
	ver string
	err error
}

func (f stubFlockd) Version(context.Context, string) (string, error) { return f.ver, f.err }

// fakeAlert stands in for crypto/tls's unexported alert type: a uint8
// whose Error is "tls: <description>".
type fakeAlert uint8

func (a fakeAlert) Error() string {
	return "tls: " + map[fakeAlert]string{
		40: "handshake failure", 42: "bad certificate", 46: "unknown certificate", 48: "unknown certificate authority",
		49: "access denied", 70: "protocol version not supported", 80: "internal error", 116: "certificate required",
	}[a]
}

func remoteAlert(code uint8) error {
	return &net.OpError{Op: "remote error", Err: fakeAlert(code)}
}

var doctorNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func boolp(b bool) *bool { return &b }

// healthyEnv is a node where every check passes: an enrolled Apple
// Silicon Mac serving its default model, installed in /usr/local/bin.
func healthyEnv(t *testing.T) (*doctorEnv, *stubDaemon) {
	t.Helper()
	dataDir := t.TempDir()
	d := &stubDaemon{
		status: gen.Status{
			NodeId: "node-abc", Version: "1.2.0", Enrolled: true, State: "serving",
			UptimeSeconds: 3*3600 + 120, DefaultModel: "llama-3.2-3b-instruct-q4_k_m", ModelsLoaded: 1,
			RuntimeAccel: strp("metal"),
			Disk:         gen.Disk{Dir: filepath.Join(dataDir, "models"), FreeBytes: 200 << 30},
			Update:       &gen.Update{Current: "1.2.0", Latest: "1.2.0", CheckedAt: doctorNow.Add(-10 * time.Minute)},
		},
		models:   []gen.ModelRow{{Id: "llama-3.2-3b-instruct-q4_k_m", State: "ready", Loaded: true, Default: true, SizeBytes: 2 << 30}},
		earnings: gen.Earnings{Source: gen.Ledger},
		catalog: []gen.CatalogEntry{
			{Id: "llama-3.2-3b-instruct-q4_k_m", SizeBytes: 2 << 30},
			{Id: "big-70b", SizeBytes: 40 << 30},
		},
	}
	cfg := config.Default()
	cfg.DataDir = dataDir
	env := &doctorEnv{
		ctx:         context.Background(),
		now:         func() time.Time { return doctorNow },
		goos:        "darwin",
		goarch:      "arm64",
		home:        "/Users/ada",
		teraVersion: "1.2.0",
		cfg:         cfg,
		cfgPath:     "/Users/ada/.teraflock/config.toml",
		dataDir:     dataDir,
		apiBase:     "http://127.0.0.1:7777",
		install: installFacts{
			TeraPath: "/usr/local/bin/tera", OnPath: true, FlockdPath: "/usr/local/bin/flockd",
		},
		flockd:  stubFlockd{ver: "1.2.0"},
		service: stubService{st: svc.StatusRunning},
		linger:  stubLinger{},
		daemon:  d,
		logs:    stubLogs(""),
		enroll: stubEnroll{creds: &enroll.Credentials{
			NodeID: "node-abc", CertExpiresAt: doctorNow.Add(21 * 24 * time.Hour),
		}},
		mesh: stubMesh{probe: meshProbe{
			Host: "tunnel.teraflock.ai", Port: "443", IPs: []string{"203.0.113.7"},
			TLSVersion: "TLS 1.3", ServerVerified: true, CertRequested: true, Elapsed: 120 * time.Millisecond,
		}},
		hardware: stubHardware{
			profile: &typesv1.CapabilityProfile{
				Os: "darwin", Arch: "arm64", CpuModel: "Apple M3 Max", CpuCores: 16, RamTotalMb: 65536,
				Gpus: []*typesv1.GpuInfo{{Vendor: "apple", Model: "Apple M3 Max", Accel: "metal", UnifiedMemory: true, VramMb: 65536}},
			},
			lanes: []string{"metal", "cpu"},
		},
		runtime: stubRuntime{sel: llamacpp.Selection{BuildID: "llamacpp-b100-1", Accel: "metal", Reason: "preferred", Path: "/x/llama-server"}},
		disk:    stubDisk{free: 200 << 30},
	}
	return env, d
}

// doctorCase is one row of a check's table: start from the healthy node,
// change one thing, and say what the check must report.
type doctorCase struct {
	name   string
	mutate func(env *doctorEnv, d *stubDaemon)
	want   doctorStatus
	detail []string // substrings of Detail
	remedy []string // substrings of Remedy
	absent []string // substrings that must not appear in Detail
}

func runDoctorCases(t *testing.T, check func(context.Context, *doctorEnv) doctorResult, cases []doctorCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, d := healthyEnv(t)
			if tc.mutate != nil {
				tc.mutate(env, d)
			}
			got := finishDoctorResult(doctorCheck{id: "x", title: "X"}, check(context.Background(), env))
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s\ndetail: %s\nremedy: %s", got.Status, tc.want, got.Detail, got.Remedy)
			}
			for _, s := range tc.detail {
				if !strings.Contains(got.Detail, s) {
					t.Errorf("detail misses %q:\n%s", s, got.Detail)
				}
			}
			for _, s := range tc.absent {
				if strings.Contains(got.Detail, s) {
					t.Errorf("detail must not say %q:\n%s", s, got.Detail)
				}
			}
			for _, s := range tc.remedy {
				if !strings.Contains(got.Remedy, s) {
					t.Errorf("remedy misses %q:\n%s", s, got.Remedy)
				}
			}
			if (got.Status == doctorWarn || got.Status == doctorFail) && got.Remedy == "" {
				t.Errorf("a %s must come with a fix:\n%s", got.Status, got.Detail)
			}
			if got.Detail == "" {
				t.Error("no detail")
			}
		})
	}
}

func daemonDownErr() error {
	return &client.UnreachableError{Base: "http://127.0.0.1:7777", Err: errors.New("connection refused")}
}

func TestCheckInstall(t *testing.T) {
	// The check prints the directory with the host's separator, so the
	// expectation is built the same way (this test runs on Windows too).
	installDir := filepath.Dir("/usr/local/bin/tera")
	runDoctorCases(t, checkInstall, []doctorCase{
		{name: "both binaries together on PATH", want: doctorPass,
			detail: []string{"tera 1.2.0 and flockd 1.2.0 are installed in " + installDir + "."},
			absent: []string{"PATH"}},
		{name: "v prefix is the same version", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.flockd = stubFlockd{ver: "v1.2.0"} }},
		{name: "flockd found on PATH elsewhere", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.install.FlockdPath = "/opt/bin/flockd" },
			detail: []string{"flockd 1.2.0 is at /opt/bin/flockd (found on your PATH)"}},
		{name: "desktop app install is not on PATH", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.install = installFacts{TeraPath: "/Users/ada/.teraflock/bin/tera", FlockdPath: "/Users/ada/.teraflock/bin/flockd"}
			},
			detail: []string{"tera is not on your PATH, so run it by its full path: /Users/ada/.teraflock/bin/tera.",
				"The desktop app installs tera and flockd there"}},
		{name: "not on PATH, another tera shadows it", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.install = installFacts{TeraPath: "/home/ada/dl/tera", FlockdPath: "/home/ada/dl/flockd", OtherTera: "/usr/bin/tera"}
			},
			detail: []string{"run it by its full path: /home/ada/dl/tera.", "Typing `tera` runs a different copy: /usr/bin/tera."},
			absent: []string{"desktop app"}},
		{name: "flockd missing", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.install.FlockdPath, env.install.FlockdErr = "", errors.New("not found")
			},
			detail: []string{"the flockd daemon was not found"},
			remedy: []string{"Install flockd next to tera (in " + installDir + ")", doctorReleasesURL}},
		{name: "flockd does not start", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.flockd = stubFlockd{err: errors.New("killed")} },
			detail: []string{"did not report its version (killed)"},
			remedy: []string{"`/usr/local/bin/flockd -version`"}},
		{name: "versions differ", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.flockd = stubFlockd{ver: "1.1.0"} },
			detail: []string{"tera 1.2.0", "flockd 1.1.0", "these two versions differ"},
			remedy: []string{"same release"}},
		{name: "configuration does not load", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.cfgErr = errors.New("config: invalid governor.serve_policy \"x\"")
			},
			detail: []string{"The configuration does not load", "serve_policy"},
			remedy: []string{"Fix or remove /Users/ada/.teraflock/config.toml"}},
	})
}

func TestCheckService(t *testing.T) {
	runDoctorCases(t, checkService, []doctorCase{
		{name: "launchd running", want: doctorPass,
			detail: []string{"The launchd service is installed and running."},
			absent: []string{"ingering"}},
		{name: "windows task running", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.goos = "windows" },
			detail: []string{"The logon task is installed and running."}},
		{name: "linux, lingering on", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.linger = "linux", stubLinger{user: "ada", on: true, known: true, applies: true}
			},
			detail: []string{"The systemd user service is installed and running.", "Lingering is enabled for ada"}},
		{name: "linux, lingering off", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.linger = "linux", stubLinger{user: "ada", on: false, known: true, applies: true}
			},
			detail: []string{"Lingering is off for ada", "stops the node when your last session ends"},
			remedy: []string{"`sudo loginctl enable-linger ada`"}},
		{name: "linux, lingering unreadable", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.linger = "linux", stubLinger{user: "ada", applies: true}
			},
			detail: []string{"Could not read whether lingering is enabled", "loginctl show-user ada --property=Linger"}},
		{name: "installed but stopped", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.service = stubService{st: svc.StatusStopped} },
			detail: []string{"installed but not running"}, remedy: []string{"`tera up`"}},
		{name: "stopped on linux does not nag about lingering", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.service = "linux", stubService{st: svc.StatusStopped}
				env.linger = stubLinger{user: "ada", known: true, applies: true}
			},
			absent: []string{"ingering"}, remedy: []string{"`tera up`"}},
		{name: "not installed, nothing answering", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.service = stubService{st: svc.StatusNotInstalled}
				d.statusErr = daemonDownErr()
			},
			detail: []string{"is not installed, so nothing starts flockd"}, remedy: []string{"`tera up`"}},
		{name: "not installed, flockd started by hand", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.service = stubService{st: svc.StatusNotInstalled} },
			detail: []string{"started by hand", "will not come back after a restart"}, remedy: []string{"`tera up`"}},
		{name: "service manager error", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.service = stubService{st: svc.StatusUnknown, err: errors.New("svc: launchctl list: exit status 1")}
			},
			detail: []string{"Could not read the state of the launchd service", "launchctl list"}},
		{name: "unknown without an error", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.service = stubService{st: svc.StatusUnknown} },
			detail: []string{"no usable answer"}},
		{name: "unsupported platform", want: doctorSkip,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.service = "plan9", stubService{st: svc.StatusUnknown, err: svc.ErrUnsupported}
			},
			detail: []string{"not available on plan9"}},
		{name: "full-path tera in the remedy when not on PATH", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.install = installFacts{TeraPath: "/Users/ada/.teraflock/bin/tera", FlockdPath: "/Users/ada/.teraflock/bin/flockd"}
				env.service = stubService{st: svc.StatusStopped}
			},
			remedy: []string{"`/Users/ada/.teraflock/bin/tera up`"}},
	})
}

func TestCheckDaemon(t *testing.T) {
	state := func(s string) func(*doctorEnv, *stubDaemon) {
		return func(_ *doctorEnv, d *stubDaemon) { d.status.State = s }
	}
	runDoctorCases(t, checkDaemon, []doctorCase{
		{name: "serving", want: doctorPass,
			detail: []string{"flockd 1.2.0 answers on http://127.0.0.1:7777 and has been up for 3h02m.", "It is serving."}},
		{name: "idle", want: doctorPass, mutate: state("idle"), detail: []string{"It is idle"}},
		{name: "long uptime reads in days", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.UptimeSeconds = 102*3600 + 60 },
			detail: []string{"has been up for 4 days."}},
		{name: "starting", want: doctorWarn, mutate: state("starting"),
			detail: []string{"still fetching or loading its default model"}, remedy: []string{"`tera logs -f`"}},
		{name: "no model is the models check's to judge", want: doctorPass, mutate: state("no-model"),
			detail: []string{"no model to serve yet (see Models)"}},
		{name: "yielded to the operator is the policy working", want: doctorPass, mutate: state("yielded"),
			detail: []string{"paused because this computer is in use", "`tera limits --serve always`"}},
		{name: "on battery", want: doctorPass, mutate: state("paused-battery"),
			detail: []string{"on battery power", "`tera limits --serve-on-battery`"}},
		{name: "too hot", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.State, d.status.TempCelsius = "paused-thermal", 96 },
			detail: []string{"too hot (96°C)"}, remedy: []string{"cool down", "--max-temp"}},
		{name: "outside schedule", want: doctorPass, mutate: state("outside-schedule"),
			detail: []string{"outside its serving schedule", "--schedule"}},
		{name: "a state this tera does not know", want: doctorPass, mutate: state("defragmenting"),
			detail: []string{"Its state is defragmenting."}},
		{name: "standalone earns nothing", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.Standalone = true },
			detail: []string{"flockd 1.2.0 (standalone)", "not connected to the mesh and earns nothing"},
			remedy: []string{"without --standalone", "`tera down`, then `tera up`"}},
		{name: "updated on disk, not restarted", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.flockd = stubFlockd{ver: "1.3.0"} },
			detail: []string{"still flockd 1.2.0, but flockd 1.3.0 is installed", "without a restart"},
			remedy: []string{"`tera down`, then `tera up`"}},
		{name: "an older install must not be told to restart", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.flockd = stubFlockd{ver: "1.1.0"} },
			detail: []string{"(1.1.0) are older than the running daemon (1.2.0)", "another install"},
			remedy: []string{"Use the tera that was installed with the running daemon", "Do not run `tera up`"}},
		{name: "a development build next to tera is only noted", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.flockd = stubFlockd{ver: "8092857-dev"} },
			detail: []string{"The flockd next to this tera is 8092857-dev, not the build that is running."}},
		{name: "address taken from local_api.listen", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.apiBase, env.apiFromConfig = "http://127.0.0.1:9000", true
			},
			detail: []string{"answers on http://127.0.0.1:9000", "other tera commands need `--api http://127.0.0.1:9000`"}},
		{name: "nothing answering, service not running", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr, env.service = daemonDownErr(), stubService{st: svc.StatusStopped}
			},
			detail: []string{"No daemon answers on http://127.0.0.1:7777: flockd is not running."},
			absent: []string{"last error"},
			remedy: []string{"`tera up`"}},
		{name: "stopped node, old error in the log is quoted, not blamed", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr, env.service = daemonDownErr(), stubService{st: svc.StatusStopped}
				env.logs = stubLogs("time=1 level=ERROR msg=\"request failed\"\ntime=2 level=INFO msg=shutdown\n")
			},
			detail: []string{"flockd is not running. The last error in its log: time=1 level=ERROR msg=\"request failed\""},
			remedy: []string{"`tera up`"}},
		{name: "service up, API not yet: still loading", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = daemonDownErr() },
			detail: []string{"The service is running but no daemon answers", "once the default model is loaded"},
			remedy: []string{"Check again in a minute", "flockd.log"}},
		{name: "service up, log shows the crash", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr = daemonDownErr()
				env.logs = stubLogs("time=1 level=INFO msg=boot\nflockd: llamacpp: manifest x has no build for darwin/amd64 accel=cpu-avx2\n")
			},
			detail: []string{"probably failing at start-up and being restarted",
				"The last error in its log: flockd: llamacpp: manifest x has no build for darwin/amd64"},
			remedy: []string{"flockd.log", "`tera down`, then `tera up`"}},
		{name: "linux points at journalctl", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.goos, d.statusErr = "linux", daemonDownErr()
				env.logs = stubLogs("time=1 level=ERROR msg=\"runtime fetch failed\"")
			},
			remedy: []string{"`journalctl --user -u flockd -n 50`"}},
		{name: "token rejected", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr = &client.APIError{Status: 401, Message: "unauthorized"}
				env.tokenSource = "/Users/ada/.teraflock/local_api_token"
			},
			detail: []string{"rejects the local token (/Users/ada/.teraflock/local_api_token)"},
			remedy: []string{"--data-dir <dir> doctor", "FLOCKD_DATA_DIR"}},
		{name: "token from the environment rejected", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr, env.tokenSource = &client.APIError{Status: 401}, "$TERA_TOKEN"
			},
			detail: []string{"rejects the local token ($TERA_TOKEN)"}},
		{name: "no token file at all", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr, env.tokenSource = &client.APIError{Status: 401}, "no token found at /data/local_api_token"
			},
			detail: []string{"has no local token to show it (no token found at /data/local_api_token)"}},
		{name: "another program on the port", want: doctorFail,
			mutate: func(_ *doctorEnv, d *stubDaemon) {
				d.statusErr = &client.APIError{Status: 404, Message: "404 page not found"}
			},
			detail: []string{"not like flockd does (HTTP 404)", "another program may be using the port"},
			remedy: []string{"local_api.listen"}},
		{name: "daemon answers with an error", want: doctorFail,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = &client.APIError{Status: 500, Message: "boom"} },
			detail: []string{"reports an error: daemon error (500): boom"}},
		{name: "daemon accepts but does not answer", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = context.DeadlineExceeded },
			detail: []string{"did not answer within 3s", "very busy or stuck"},
			remedy: []string{"`tera down`, then `tera up`"}},
		{name: "an answer that is not the API's", want: doctorFail,
			mutate: func(_ *doctorEnv, d *stubDaemon) {
				d.statusErr = errors.New("daemon answered 200 without the expected JSON body")
			},
			detail: []string{"gave an answer tera cannot use: daemon answered 200 without the expected JSON body"}},
	})
}

// testCertPEM is a self-signed certificate that expires at notAfter.
func testCertPEM(t *testing.T, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "node"},
		NotBefore: notAfter.Add(-30 * 24 * time.Hour), NotAfter: notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestCheckEnrollment(t *testing.T) {
	expiresIn := func(d time.Duration) func(*doctorEnv, *stubDaemon) {
		return func(env *doctorEnv, _ *stubDaemon) {
			env.enroll = stubEnroll{creds: &enroll.Credentials{NodeID: "node-abc", CertExpiresAt: doctorNow.Add(d)}}
		}
	}
	day := 24 * time.Hour
	runDoctorCases(t, checkEnrollment, []doctorCase{
		{name: "enrolled, certificate fresh", want: doctorPass,
			detail: []string{"Enrolled, with node id node-abc.", "valid until 16 Dec 2026 (21 days left)"}},
		{name: "one second outside the rotation window", want: doctorPass, mutate: expiresIn(7*day + time.Second)},
		{name: "exactly at the rotation window", want: doctorWarn, mutate: expiresIn(7 * day),
			detail: []string{"expires in 7 days", "renews it when it starts during the last 7 days"},
			remedy: []string{"`tera down`, then `tera up`", "`tera login --force`"}},
		{name: "hours left", want: doctorWarn, mutate: expiresIn(5 * time.Hour), detail: []string{"expires in 5 hours"}},
		{name: "expired", want: doctorFail, mutate: expiresIn(-time.Hour),
			detail: []string{"client certificate expired on 25 Nov 2026", "the mesh refuses it"},
			remedy: []string{"`tera login --force`", "https://app.teraflock.com/connect"}},
		{name: "the certificate's own date wins over the recorded one", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.enroll = stubEnroll{creds: &enroll.Credentials{
					NodeID: "node-abc", CertExpiresAt: doctorNow.Add(20 * day),
					ClientCertPEM: testCertPEM(t, doctorNow.Add(-2*day)),
				}}
			},
			detail: []string{"expired on 23 Nov 2026"}},
		{name: "expiry unknown", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.enroll = stubEnroll{creds: &enroll.Credentials{NodeID: "node-abc"}}
			},
			detail: []string{"expiry date could not be read"}},
		{name: "not enrolled", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.enroll = stubEnroll{err: os.ErrNotExist}
				d.status.Enrolled = false
			},
			detail: []string{"This node is not enrolled, so the mesh sends it no work."},
			remedy: []string{"Run `tera login`", "asks for a claim code from https://app.teraflock.com/connect"}},
		{name: "not enrolled, daemon down", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.enroll = stubEnroll{err: fmt.Errorf("open creds: %w", os.ErrNotExist)}
				d.statusErr = daemonDownErr()
			},
			remedy: []string{"`tera login`"}},
		{name: "not enrolled, standalone", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.enroll = stubEnroll{err: os.ErrNotExist}
				d.status.Enrolled, d.status.Standalone = false, true
			},
			detail: []string{"Standalone mode needs no account"}, remedy: []string{"`tera login`"}},
		{name: "claim code waiting for the daemon", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.enroll = stubEnroll{err: os.ErrNotExist, pending: true} },
			detail: []string{"A claim code is stored, but the node is not enrolled yet"},
			remedy: []string{"`tera up`", "`tera login`"}},
		{name: "credentials file corrupt", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.enroll = stubEnroll{err: errors.New("enroll: credentials file corrupt")}
			},
			detail: []string{"cannot be read: enroll: credentials file corrupt"}, remedy: []string{"`tera login --force`"}},
		{name: "rotation failed", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.CertRotationError = strp("cert rotation dial: timeout") },
			detail: []string{"last attempt to renew the certificate failed: cert rotation dial: timeout"},
			remedy: []string{"`tera down`, then `tera up`"}},
		{name: "daemon started before the enrollment", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.Enrolled = false },
			detail: []string{"started before this enrollment"}, remedy: []string{"`tera down`, then `tera up`"}},
		{name: "daemon still on the previous identity", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.NodeId = "node-old" },
			detail: []string{"still enrolled with node id node-old"}},
		{name: "daemon down: the files alone pass", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = daemonDownErr() },
			detail: []string{"Enrolled, with node id node-abc."}},
	})
}

func TestCheckMesh(t *testing.T) {
	probe := func(p meshProbe) func(*doctorEnv, *stubDaemon) {
		return func(env *doctorEnv, _ *stubDaemon) {
			if p.Host == "" {
				p.Host, p.Port = "tunnel.teraflock.ai", "443"
			}
			env.mesh = stubMesh{probe: p}
		}
	}
	resolved := []string{"203.0.113.7"}
	runDoctorCases(t, checkMesh, []doctorCase{
		{name: "reachable, live session", want: doctorPass,
			detail: []string{"tunnel.teraflock.ai:443 is reachable (203.0.113.7)", "the TLS 1.3 handshake succeed",
				"asks for the node's client certificate as expected (120 ms)", "has a live session"}},
		{name: "server does not ask for a certificate", want: doctorPass,
			mutate: probe(meshProbe{IPs: resolved, TLSVersion: "TLS 1.3", ServerVerified: true}),
			absent: []string{"client certificate"}},
		// The case the brief calls out: mTLS-only servers end the handshake
		// when no client certificate comes. Reachable all the same.
		{name: "TLS 1.2 server demands a client certificate", want: doctorPass,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: remoteAlert(42), ServerVerified: true, CertRequested: true}),
			detail: []string{"is reachable", "asked for the node's client certificate, which this check does not present", "(bad certificate), as expected"}},
		{name: "OpenSSL-style handshake failure for the same reason", want: doctorPass,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: remoteAlert(40), ServerVerified: true, CertRequested: true})},
		{name: "TLS 1.3 certificate required", want: doctorPass,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: fmt.Errorf("read: %w", remoteAlert(116)), ServerVerified: true, CertRequested: true}),
			detail: []string{"(certificate required)"}},
		// The same alerts are a failure when the server never proved who it
		// is, or never asked for a certificate.
		{name: "alert before the server was verified", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: remoteAlert(40)}),
			detail: []string{"refused the TLS handshake (handshake failure)"}},
		{name: "alert without a certificate request", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: remoteAlert(42), ServerVerified: true}),
			detail: []string{"refused the TLS handshake (bad certificate)"}},
		{name: "an unrelated alert after the request", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: remoteAlert(80), ServerVerified: true, CertRequested: true}),
			detail: []string{"refused the TLS handshake (internal error)"}},
		{name: "TLS 1.2-only middlebox", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: remoteAlert(70)}),
			detail: []string{"protocol version not supported"}},
		{name: "intercepted TLS", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: x509.UnknownAuthorityError{}}),
			detail: []string{"the certificate it presents is not trusted", "intercepting the connection"},
			remedy: []string{"date and time", "tunnel.ca_cert"}},
		{name: "wrong host on the certificate", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: x509.HostnameError{Certificate: &x509.Certificate{}, Host: "tunnel.teraflock.ai"}}),
			detail: []string{"not trusted"}},
		{name: "handshake timed out", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTLS, Err: context.DeadlineExceeded}),
			detail: []string{"the TLS handshake failed: no answer in time"}},
		{name: "DNS: no such host", want: doctorFail,
			mutate: probe(meshProbe{Stage: meshStageDNS, Err: &net.DNSError{Err: "no such host", Name: "tunnel.teraflock.ai", IsNotFound: true}}),
			detail: []string{"Cannot look up tunnel.teraflock.ai: the name does not exist."},
			remedy: []string{"internet connection"}},
		{name: "DNS: timeout", want: doctorFail,
			mutate: probe(meshProbe{Stage: meshStageDNS, Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}),
			detail: []string{"the DNS server did not answer in time"}},
		{name: "TCP refused", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTCP, Err: &net.OpError{Op: "dial", Err: errors.New("connect: connection refused")}}),
			detail: []string{"resolves (203.0.113.7), but nothing answers on port 443: connect: connection refused"},
			remedy: []string{"outbound TCP connections to tunnel.teraflock.ai:443"}},
		{name: "TCP to an IP address", want: doctorFail,
			mutate: probe(meshProbe{Host: "10.0.0.9", Port: "8443", IPs: []string{"10.0.0.9"}, Stage: meshStageTCP, Err: context.DeadlineExceeded}),
			detail: []string{"Nothing answers at 10.0.0.9:8443: no answer in time."}, absent: []string{"resolves"}},
		{name: "TCP silently dropped", want: doctorFail,
			mutate: probe(meshProbe{IPs: resolved, Stage: meshStageTCP, Err: context.DeadlineExceeded}),
			detail: []string{"no answer in time"}, remedy: []string{"firewall"}},
		{name: "address is not host:port", want: doctorFail,
			mutate: probe(meshProbe{Host: "tunnel.teraflock.ai", Stage: meshStageAddr, Err: errors.New("missing port in address")}),
			detail: []string{"is not a host:port"}, remedy: []string{"tunnel.coordinator_addr", "tunnel.teraflock.ai:443"}},
		{name: "plaintext development coordinator", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.cfg.Tunnel.Insecure = true
				env.mesh = stubMesh{probe: meshProbe{Host: "localhost", Port: "9443", IPs: []string{"127.0.0.1"}}}
			},
			detail: []string{"localhost:9443 is reachable", "plaintext: tunnel.insecure is set"}},
		{name: "reachable, but the daemon's session is down", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.earnings = gen.Earnings{Source: gen.Estimated} },
			detail: []string{"is reachable", "no account update from the coordinator", "its own session to the mesh is down"},
			remedy: []string{"`tera logs`"}},
		{name: "daemon just started: no verdict on the session yet", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) {
				d.earnings, d.status.UptimeSeconds = gen.Earnings{Source: gen.Estimated}, 90
			},
			detail: []string{"started 1m30s ago and has not had its first account update"}},
		{name: "daemon down: reachability alone", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = daemonDownErr() },
			absent: []string{"session"}},
		{name: "not enrolled: reachability alone", want: doctorPass,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.enroll, d.status.Enrolled = stubEnroll{err: os.ErrNotExist}, false
				d.earnings = gen.Earnings{Source: gen.Estimated}
			},
			absent: []string{"session", "account update"}},
		{name: "standalone daemon", want: doctorSkip,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.Standalone = true },
			detail: []string{"runs standalone"}},
		{name: "standalone by configuration", want: doctorSkip,
			mutate: func(env *doctorEnv, d *stubDaemon) { env.cfg.Tunnel.Standalone, d.statusErr = true, daemonDownErr() }},
	})
}

// The mesh check dials the configured address with the daemon's trust
// settings and nothing that identifies the node.
func TestCheckMeshTarget(t *testing.T) {
	env, _ := healthyEnv(t)
	var got meshTarget
	env.mesh = stubMesh{probe: meshProbe{Host: "coord.example", Port: "8443", TLSVersion: "TLS 1.3"}, got: &got}
	env.cfg.Tunnel.CoordinatorAddr = "coord.example:8443"
	env.cfg.Tunnel.InsecureSkipVerify = true
	ca := testCertPEM(t, doctorNow.Add(time.Hour))
	env.enroll = stubEnroll{creds: &enroll.Credentials{NodeID: "n", CertExpiresAt: doctorNow.Add(time.Hour), CACertPEM: ca}}
	checkMesh(context.Background(), env)
	if got.Addr != "coord.example:8443" || got.Plaintext || !got.InsecureSkipVerify || got.MinTLS != 0 {
		t.Fatalf("target = %+v", got)
	}
	if got.RootCAs == nil {
		t.Fatal("the mesh CA from enrollment must be among the trusted roots")
	}
	block, _ := pem.Decode(ca)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: got.RootCAs, CurrentTime: doctorNow}); err != nil {
		t.Fatalf("mesh CA is not trusted by the probe's pool: %v", err)
	}
}

func TestCheckRuntime(t *testing.T) {
	nvidiaBox := func(env *doctorEnv, lanes []string, sel llamacpp.Selection, cards ...hardware.NVIDIAGPU) {
		env.goos, env.goarch = "linux", "amd64"
		gpus := make([]*typesv1.GpuInfo, 0, len(cards))
		for _, c := range cards {
			accel := "cuda12"
			if c.Warning != "" && !strings.Contains(c.Warning, "stays on the CUDA runtime") {
				accel = ""
			}
			gpus = append(gpus, &typesv1.GpuInfo{Vendor: "nvidia", Model: c.Name, VramMb: c.VRAMMB, DriverVersion: c.DriverVersion, Accel: accel})
		}
		env.hardware = stubHardware{
			profile: &typesv1.CapabilityProfile{Os: "linux", Arch: "amd64", CpuModel: "AMD Ryzen 9 7950X", CpuCores: 32, RamTotalMb: 64 * 1024, Gpus: gpus},
			lanes:   lanes, nvidia: cards,
		}
		env.runtime = stubRuntime{sel: sel}
	}
	tooOld := "nvidia gpu too old for the CUDA runtime: GeForce GTX 1080 has compute capability 6.1, the CUDA runtime needs 7.5 or newer (RTX 20-series / GTX 16-series or newer)"
	var lanesAsked []string
	runDoctorCases(t, checkRuntime, []doctorCase{
		{name: "apple silicon, build cached", want: doctorPass,
			detail: []string{"Apple M3 Max, 16 cores, 64 GB of memory.", "GPU: Apple M3 Max (metal, shares the system memory).",
				"Accelerator lanes, in order of preference: metal > cpu.",
				"has a build for darwin/arm64: llamacpp-b100-1 (metal); it is already downloaded."}},
		{name: "build not downloaded yet", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.runtime = stubRuntime{sel: llamacpp.Selection{BuildID: "llamacpp-b100-1", Accel: "metal", Reason: "preferred"}, gotLanes: &lanesAsked}
			},
			detail: []string{"it is downloaded on the next start"}},
		{name: "nvidia, modern card", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				nvidiaBox(env, []string{"cuda12", "vulkan", "cpu-avx2"}, llamacpp.Selection{BuildID: "b1", Accel: "cuda12", Reason: "preferred"},
					hardware.NVIDIAGPU{Name: "GeForce RTX 4090", DriverVersion: "550.54.14", VRAMMB: 24564, ComputeCap: "8.9", RunsCUDA: true})
			},
			detail: []string{"GPU: GeForce RTX 4090 (cuda12, 24 GB of video memory, driver 550.54.14).",
				"NVIDIA GeForce RTX 4090: driver 550.54.14, compute capability 8.9.",
				"cuda12 > vulkan > cpu-avx2", "b1 (cuda12)"}},
		{name: "nvidia, old driver reports no capability", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				nvidiaBox(env, []string{"cuda12", "cpu-avx2"}, llamacpp.Selection{BuildID: "b1", Accel: "cuda12", Reason: "preferred"},
					hardware.NVIDIAGPU{Name: "GeForce RTX 2070", DriverVersion: "470.82", VRAMMB: 8192, RunsCUDA: true})
			},
			detail: []string{"driver 470.82, the driver does not report its compute capability."}},
		{name: "nvidia too old, falls to vulkan", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				nvidiaBox(env, []string{"vulkan", "cpu-avx2"}, llamacpp.Selection{BuildID: "b1", Accel: "vulkan", Reason: "preferred"},
					hardware.NVIDIAGPU{Name: "GeForce GTX 1080", DriverVersion: "535.1", VRAMMB: 8192, ComputeCap: "6.1", Warning: tooOld + "; falling back to the Vulkan runtime"})
			},
			detail: []string{"Too old for the CUDA runtime: GeForce GTX 1080 has compute capability 6.1",
				"falling back to the Vulkan runtime.", "GPU: GeForce GTX 1080 (no accelerated lane of its own", "b1 (vulkan)"},
			remedy: []string{"Nothing to change on this machine"}},
		{name: "nvidia too old, linux without vulkan", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				nvidiaBox(env, []string{"cpu-avx2"}, llamacpp.Selection{BuildID: "b1", Accel: "cpu-avx2", Reason: "preferred"},
					hardware.NVIDIAGPU{Name: "GeForce GTX 1080", DriverVersion: "535.1", VRAMMB: 8192, ComputeCap: "6.1", Warning: tooOld + "; falling back to the CPU runtime"})
			},
			detail: []string{"Too old for the CUDA runtime", "b1 (cpu-avx2)"},
			remedy: []string{"Install the Vulkan loader and the NVIDIA Vulkan driver", "`tera down`, then `tera up`"}},
		{name: "mixed nvidia box stays on cuda", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				nvidiaBox(env, []string{"cuda12", "cpu-avx2"}, llamacpp.Selection{BuildID: "b1", Accel: "cuda12", Reason: "preferred"},
					hardware.NVIDIAGPU{Name: "GeForce RTX 3090", DriverVersion: "550.1", VRAMMB: 24576, ComputeCap: "8.6", RunsCUDA: true},
					hardware.NVIDIAGPU{Name: "GeForce GTX 1080", DriverVersion: "550.1", VRAMMB: 8192, ComputeCap: "6.1",
						Warning: tooOld + "; the node stays on the CUDA runtime for its other NVIDIA GPU, and this card cannot serve on it"})
			},
			detail: []string{"stays on the CUDA runtime for its other NVIDIA GPU", "b1 (cuda12)"},
			remedy: []string{"Nothing to change on this machine"}},
		{name: "gpu present, only a cpu build published", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				nvidiaBox(env, []string{"cuda12", "cpu-avx2"}, llamacpp.Selection{BuildID: "b1", Accel: "cpu-avx2", Reason: "no cuda12 artifact for windows/amd64"},
					hardware.NVIDIAGPU{Name: "GeForce RTX 4070", DriverVersion: "560.1", VRAMMB: 12282, ComputeCap: "8.9", RunsCUDA: true})
				env.goos = "windows"
			},
			detail: []string{"there is no cuda12 artifact for windows/amd64 yet, so the node runs the CPU build"},
			remedy: []string{"once a GPU build is published for windows/amd64, the node uses it from its next restart"}},
		{name: "amd on vulkan while no rocm build exists", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.goarch = "linux", "amd64"
				env.hardware = stubHardware{
					profile: &typesv1.CapabilityProfile{CpuModel: "Ryzen", CpuCores: 16, RamTotalMb: 32768,
						Gpus: []*typesv1.GpuInfo{{Vendor: "amd", Model: "Radeon RX 7900 XT", Accel: "rocm", VramMb: 20480}}},
					lanes: []string{"rocm", "vulkan", "cpu-avx2"},
				}
				env.runtime = stubRuntime{sel: llamacpp.Selection{BuildID: "b1", Accel: "vulkan", Reason: "no rocm artifact for linux/amd64"}}
			},
			detail: []string{"b1 (vulkan)", "That lane was chosen because there is no rocm artifact for linux/amd64."}},
		{name: "cpu only box", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.goarch = "linux", "amd64"
				env.hardware = stubHardware{
					profile: &typesv1.CapabilityProfile{Os: "linux", Arch: "amd64", CpuCores: 8, RamTotalMb: 16384,
						Gpus: []*typesv1.GpuInfo{{Vendor: "none", Model: "cpu", Accel: "cpu-avx2"}}},
					lanes: []string{"cpu-avx2"},
				}
				env.runtime = stubRuntime{sel: llamacpp.Selection{BuildID: "b1", Accel: "cpu-avx2", Reason: "preferred"}}
			},
			detail: []string{"linux/amd64 CPU, 8 cores, 16 GB of memory.", "No usable GPU was detected", "b1 (cpu-avx2)"}},
		{name: "running daemon still on another lane", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.RuntimeAccel = strp("cpu") },
			detail: []string{"The running daemon uses its cpu build until it is restarted."}},
		{name: "intel mac", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goarch = "amd64"
				env.runtime = stubRuntime{err: fmt.Errorf("wrapped: %w", llamacpp.ErrNoBuild)}
			},
			detail: []string{"Intel Macs are not supported: no llama-server build is published for darwin/amd64"},
			remedy: []string{"Apple Silicon Mac"}},
		{name: "intel mac, offline: still the clear answer", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goarch = "amd64"
				env.runtime = stubRuntime{err: errors.New("llamacpp: fetch manifest: dial tcp: no route to host")}
			},
			detail: []string{"Intel Macs are not supported"}},
		{name: "intel mac with its own llama-server", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goarch = "amd64"
				env.runtime = stubRuntime{sel: llamacpp.Selection{Path: "/opt/llama-server", BuildID: "local-binary", Reason: "configured llama_server_path"}}
			},
			detail: []string{"your own llama-server at /opt/llama-server"}},
		{name: "no build for this platform", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.goos, env.goarch = "linux", "riscv64"
				env.runtime = stubRuntime{err: llamacpp.ErrNoBuild}
			},
			detail: []string{"has no llama-server build for linux/riscv64"},
			remedy: []string{"runtime.llama_server_path"}},
		{name: "configured llama-server is missing", want: doctorFail,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.cfg.Runtime.LlamaServerPath = "/gone/llama-server"
				env.runtime = stubRuntime{err: errors.New("llamacpp: configured llama_server_path: stat /gone/llama-server: no such file or directory")}
			},
			detail: []string{"The configured llama-server cannot be used"}, remedy: []string{"runtime.llama_server_path"}},
		{name: "manifest unreachable, daemon already serving", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.runtime = stubRuntime{err: fmt.Errorf("llamacpp: fetch manifest: %w", context.DeadlineExceeded)}
			},
			detail: []string{"could not be checked", "already has its metal build, so it keeps serving"},
			remedy: []string{"internet connection"}},
		{name: "manifest unreachable, no daemon", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr = daemonDownErr()
				env.runtime = stubRuntime{err: errors.New("llamacpp: fetch manifest: timeout")}
			},
			detail: []string{"not known whether a llama-server build exists for darwin/arm64"}},
		{name: "manifest refused by the signature policy is not a network problem", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				d.statusErr = daemonDownErr()
				env.runtime = stubRuntime{err: errors.New("llamacpp: manifest at https://x/manifest.json has no signature; refusing (runtime.require_signature=true)")}
			},
			detail: []string{"has no signature"},
			remedy: []string{"reached and refused", "runtime.require_signature"}},
		{name: "mock runtime needs no build", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.cfg.Runtime.Kind = "mock"
				env.runtime = stubRuntime{err: errors.New("must not be asked")}
			},
			detail: []string{"The mock runtime is configured", "Apple M3 Max"}},
		{name: "detection error", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) {
				env.hardware = stubHardware{err: errors.New("hardware: platform detection: boom")}
			},
			detail: []string{"Hardware detection did not complete (hardware: platform detection: boom)"},
			remedy: []string{"`tera doctor`"}},
	})
	if strings.Join(lanesAsked, ">") != "metal>cpu" {
		t.Errorf("the runtime was resolved for lanes %v, want the detected chain metal > cpu", lanesAsked)
	}
}

// A detection that was cut off by its deadline returns a degraded profile
// (no GPU). It must be reported as incomplete, not judged: on a Mac that
// profile would otherwise read as "no build for this machine".
func TestCheckRuntimeDoesNotJudgeACutOffDetection(t *testing.T) {
	env, _ := healthyEnv(t)
	env.hardware = stubHardware{
		profile: &typesv1.CapabilityProfile{Gpus: []*typesv1.GpuInfo{{Vendor: "none", Accel: "cpu"}}},
		lanes:   []string{"cpu"}, outlast: true,
	}
	env.runtime = stubRuntime{err: llamacpp.ErrNoBuild}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	got := checkRuntime(ctx, env)
	if got.Status != doctorWarn || !strings.Contains(got.Detail, "Hardware detection did not complete") {
		t.Fatalf("got %+v", got)
	}
}

func TestCheckModels(t *testing.T) {
	const def = "llama-3.2-3b-instruct-q4_k_m"
	noModels := func(_ *doctorEnv, d *stubDaemon) {
		d.models, d.status.ModelsLoaded, d.status.State = nil, 0, "no-model"
	}
	runDoctorCases(t, checkModels, []doctorCase{
		{name: "default loaded, room on disk", want: doctorPass,
			detail: []string{"The default model is " + def + ".", "1 model is loaded: " + def + ".", "200.0GB is free in"}},
		{name: "several loaded", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) {
				d.models = append(d.models, gen.ModelRow{Id: "laya-q8_0", State: "ready", Loaded: true})
			},
			detail: []string{"2 models are loaded: " + def + ", laya-q8_0."}},
		{name: "on disk, not loaded", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.models[0].Loaded = false },
			detail: []string{"Nothing is loaded right now; the 1 model on disk loads on the first request or placement."}},
		{name: "default still downloading", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) {
				got := int64(1 << 30)
				d.models = []gen.ModelRow{{Id: def, State: "downloading", SizeBytes: 2 << 30, ReceivedBytes: &got}}
			},
			detail: []string{"The default model is still downloading (50%)."}, remedy: []string{"`tera dashboard`"}},
		{name: "nothing installed", want: doctorWarn, mutate: noModels,
			detail: []string{"The default model is not on this node yet, and no other model is"},
			remedy: []string{"`tera models pull " + def + "`"}},
		{name: "default missing but another model serves", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.models[0].Id = "laya-q8_0" },
			detail: []string{"1 model is loaded: laya-q8_0.", "The default model itself is not on this node"}},
		{name: "default file deleted from disk", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.models[0].State, d.models[0].Loaded = "missing", false },
			detail: []string{"its file is gone from disk"}, remedy: []string{"`tera models pull " + def + "`"}},
		{name: "no default, mesh-placed model ready", want: doctorPass,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.DefaultModel = "" },
			detail: []string{"No default model is set"}},
		{name: "no default and no model", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) { noModels(env, d); d.status.DefaultModel = "" },
			detail: []string{"There is no model on this node yet"}, remedy: []string{"`tera models pull <id>`"}},
		{name: "default model does not fit on the disk", want: doctorFail,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				noModels(env, d)
				d.status.DefaultModel = "big-70b"
				env.disk = stubDisk{free: 30 << 30}
			},
			detail: []string{"The default model big-70b needs 40.0GB, but only 30.0GB is free", "it does not fit"},
			remedy: []string{"Free up disk space", "`tera up --default-model <id>`"}},
		{name: "default model fits exactly with headroom", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				noModels(env, d)
				d.status.DefaultModel = "big-70b"
				env.disk = stubDisk{free: 40<<30 + doctorDiskHeadroom}
			},
			absent: []string{"does not fit"}},
		{name: "catalog unavailable: size unknown, no verdict on fit", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				noModels(env, d)
				d.catalogErr = errors.New("upstream")
				env.disk = stubDisk{free: 6 << 30}
			},
			absent: []string{"does not fit"}, detail: []string{"6.0GB is free"}},
		{name: "disk nearly full while serving", want: doctorWarn,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.disk = stubDisk{free: 3 << 30} },
			detail: []string{"1 model is loaded", "Only 3.0GB is free", "too little for another model"},
			remedy: []string{"Free up disk space", "`tera models rm <id>`"}},
		{name: "free space falls back to the daemon's figure", want: doctorPass,
			mutate: func(env *doctorEnv, _ *stubDaemon) { env.disk = stubDisk{err: errors.New("statfs")} },
			detail: []string{"200.0GB is free"}},
		{name: "model list failed", want: doctorWarn,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.modelsErr = errors.New("daemon error (500): index locked") },
			detail: []string{"did not list its models: daemon error (500): index locked"}},
		{name: "daemon down, disk fine", want: doctorSkip,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = daemonDownErr() },
			detail: []string{"The daemon is not answering, so its models cannot be listed.", "The configured default model is " + def + ".", "200.0GB is free"}},
		{name: "daemon down, no default configured", want: doctorSkip,
			mutate: func(env *doctorEnv, d *stubDaemon) { d.statusErr, env.cfg.Models.Default = daemonDownErr(), "none" },
			detail: []string{"No default model is configured."}},
		{name: "daemon down, disk nearly full", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) { d.statusErr, env.disk = daemonDownErr(), stubDisk{free: 1 << 30} },
			detail: []string{"Only 1.0GB is free"}},
	})
}

func TestCheckUpdate(t *testing.T) {
	upd := func(u gen.Update) func(*doctorEnv, *stubDaemon) {
		return func(_ *doctorEnv, d *stubDaemon) { d.status.Update = &u }
	}
	runDoctorCases(t, checkUpdate, []doctorCase{
		{name: "current", want: doctorPass,
			detail: []string{"The node runs flockd 1.2.0, and the version feed has nothing newer."}, absent: []string{"last read"}},
		{name: "stale feed read is mentioned", want: doctorPass,
			mutate: upd(gen.Update{Current: "1.2.0", Latest: "1.2.0", CheckedAt: doctorNow.Add(-30 * time.Hour)}),
			detail: []string{"The feed was last read 1 day ago."}},
		{name: "update available, macOS", want: doctorWarn,
			mutate: upd(gen.Update{Available: true, Current: "1.2.0", Latest: "1.3.0", Url: strp("https://example.test/r/1.3.0")}),
			detail: []string{"flockd 1.3.0 is available; the node runs 1.2.0."},
			remedy: []string{"`brew upgrade teraflock/tap/tera`", "https://example.test/r/1.3.0", "`tera down`, then `tera up`"}},
		{name: "update available, linux", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.goos = "linux"
				d.status.Update = &gen.Update{Available: true, Current: "1.2.0", Latest: "1.3.0"}
			},
			remedy: []string{"install.sh", doctorReleasesURL}},
		{name: "update available, windows", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.goos = "windows"
				d.status.Update = &gen.Update{Available: true, Current: "1.2.0", Latest: "1.3.0"}
			},
			remedy: []string{"Install the new release from " + doctorReleasesURL}},
		{name: "update available, desktop app install", want: doctorWarn,
			mutate: func(env *doctorEnv, d *stubDaemon) {
				env.install = installFacts{TeraPath: "/Users/ada/.teraflock/bin/tera", FlockdPath: "/Users/ada/.teraflock/bin/flockd"}
				d.status.Update = &gen.Update{Available: true, Current: "1.2.0", Latest: "1.3.0"}
			},
			remedy: []string{"Update the Teraflock desktop app"}},
		{name: "below the mesh minimum", want: doctorFail,
			mutate: upd(gen.Update{Available: true, BelowMinimum: boolp(true), Minimum: strp("1.2.5"), Current: "1.2.0", Latest: "1.3.0"}),
			detail: []string{"flockd 1.2.0, which is below the mesh minimum, 1.2.5", "sends it no work until it is updated to 1.3.0 or newer"},
			remedy: []string{"brew upgrade"}},
		{name: "below minimum, floor not given", want: doctorFail,
			mutate: upd(gen.Update{BelowMinimum: boolp(true), Current: "1.2.0", Latest: "1.3.0"}),
			detail: []string{"below the mesh minimum:"}},
		{name: "no feed result yet", want: doctorSkip,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.status.Update = nil },
			detail: []string{"has not checked the version feed yet"}},
		{name: "daemon down", want: doctorSkip,
			mutate: func(_ *doctorEnv, d *stubDaemon) { d.statusErr = daemonDownErr() },
			detail: []string{"The daemon is not answering"}},
	})
}

// The whole list on a healthy node: every check passes, the daemon was
// asked for its status once however many checks needed it.
func TestDoctorHealthyNode(t *testing.T) {
	env, d := healthyEnv(t)
	results := runDoctor(context.Background(), env, doctorChecks())
	for _, r := range results {
		if r.Status != doctorPass {
			t.Errorf("%s = %s: %s (fix: %s)", r.ID, r.Status, r.Detail, r.Remedy)
		}
	}
	if s, clean := doctorSummary(results); !clean || s != "Everything looks good" {
		t.Errorf("summary = %q", s)
	}
	if n := d.statusCalls.Load(); n != 1 {
		t.Errorf("the daemon's status was read %d times, want once", n)
	}
}

// Nothing works: no network, no daemon, no service, nothing enrolled.
// Every check still answers, with a fix or a reason, and the command
// exits 1.
func TestDoctorDeadNode(t *testing.T) {
	env, d := healthyEnv(t)
	d.statusErr = daemonDownErr()
	env.service = stubService{st: svc.StatusNotInstalled}
	env.enroll = stubEnroll{err: os.ErrNotExist}
	env.mesh = stubMesh{probe: meshProbe{Host: "tunnel.teraflock.ai", Port: "443", Stage: meshStageDNS, Err: &net.DNSError{IsNotFound: true}}}
	env.runtime = stubRuntime{err: errors.New("llamacpp: fetch manifest: no route to host")}
	results := runDoctor(context.Background(), env, doctorChecks())
	want := map[string]doctorStatus{
		"install": doctorPass, "service": doctorFail, "daemon": doctorFail, "enrollment": doctorFail,
		"mesh": doctorFail, "runtime": doctorFail, "models": doctorSkip, "update": doctorSkip,
	}
	for _, r := range results {
		if r.Status != want[r.ID] {
			t.Errorf("%s = %s, want %s: %s", r.ID, r.Status, want[r.ID], r.Detail)
		}
		if r.Detail == "" || (r.Status == doctorFail && r.Remedy == "") {
			t.Errorf("%s says nothing useful: %+v", r.ID, r)
		}
	}
	if !doctorFailed(results) {
		t.Error("a dead node must exit 1")
	}
	if s, _ := doctorSummary(results); s != "5 checks need attention" {
		t.Errorf("summary = %q", s)
	}
}

// A fake that panics inside the probe several checks share: each of them
// reports, none takes the run down.
type panickyDaemon struct{ stubDaemon }

func (*panickyDaemon) Status(context.Context) (gen.Status, error) { panic("status exploded") }

func TestDoctorSurvivesAPanickingSharedProbe(t *testing.T) {
	env, _ := healthyEnv(t)
	env.daemon = &panickyDaemon{}
	results := runDoctor(context.Background(), env, doctorChecks())
	if len(results) != len(doctorChecks()) {
		t.Fatalf("%d results", len(results))
	}
	byID := map[string]doctorResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	if byID["install"].Status != doctorPass || byID["runtime"].Status == "" {
		t.Errorf("unrelated checks were disturbed: %+v", byID["install"])
	}
	if d := byID["daemon"]; d.Status == doctorPass || d.Detail == "" {
		t.Errorf("daemon check = %+v", d)
	}
}

// The checks against the real local API contract (the generated router,
// the mock runtime), through the real client: only the machine probes are
// fakes.
func TestDoctorAgainstTheLocalAPI(t *testing.T) {
	srv := clienttest.New(t, clienttest.Options{})
	env, _ := healthyEnv(t)
	cl, err := client.New(client.Options{API: srv.URL, DataDir: env.dataDir, Token: srv.Token})
	if err != nil {
		t.Fatal(err)
	}
	env.daemon, env.apiBase = cl, srv.URL
	env.flockd = stubFlockd{ver: "test"}
	env.teraVersion = "test"
	env.enroll = stubEnroll{err: os.ErrNotExist}
	env.cfg.Runtime.Kind = "mock"

	byID := map[string]doctorResult{}
	for _, r := range runDoctor(context.Background(), env, doctorChecks()) {
		byID[r.ID] = r
	}
	if d := byID["daemon"]; d.Status != doctorWarn || !strings.Contains(d.Detail, "flockd test (standalone) answers on "+srv.URL) ||
		!strings.Contains(d.Detail, "It is serving.") {
		t.Errorf("daemon = %+v", d)
	}
	if m := byID["models"]; m.Status != doctorPass || !strings.Contains(m.Detail, "1 model is loaded: "+clienttest.Model) {
		t.Errorf("models = %+v", m)
	}
	if e := byID["enrollment"]; e.Status != doctorWarn || !strings.Contains(e.Detail, "Standalone mode needs no account") {
		t.Errorf("enrollment = %+v", e)
	}
	if m := byID["mesh"]; m.Status != doctorSkip {
		t.Errorf("mesh = %+v", m)
	}
	if u := byID["update"]; u.Status != doctorSkip {
		t.Errorf("update = %+v", u)
	}

	// A wrong token is the daemon check's to explain.
	bad, err := client.New(client.Options{API: srv.URL, DataDir: env.dataDir, Token: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	env2, _ := healthyEnv(t)
	env2.daemon, env2.apiBase = bad, srv.URL
	if d := checkDaemon(context.Background(), env2); d.Status != doctorFail || !strings.Contains(d.Detail, "rejects the local token") {
		t.Errorf("wrong token = %+v", d)
	}

	// And nothing listening at all.
	gone, err := client.New(client.Options{API: "http://127.0.0.1:1", DataDir: env.dataDir, Token: "x"})
	if err != nil {
		t.Fatal(err)
	}
	env3, _ := healthyEnv(t)
	env3.daemon, env3.apiBase = gone, "http://127.0.0.1:1"
	env3.service = stubService{st: svc.StatusStopped}
	if d := checkDaemon(context.Background(), env3); d.Status != doctorFail || !strings.Contains(d.Detail, "flockd is not running") {
		t.Errorf("nothing listening = %+v", d)
	}
}

func TestDoctorCmdSpelling(t *testing.T) {
	for _, tc := range []struct {
		in   installFacts
		want string
	}{
		{installFacts{TeraPath: "/usr/local/bin/tera", OnPath: true}, "`tera up`"},
		{installFacts{TeraPath: "/Users/ada/.teraflock/bin/tera"}, "`/Users/ada/.teraflock/bin/tera up`"},
		{installFacts{TeraPath: `C:\Users\Ada Byron\.teraflock\bin\tera.exe`}, "`\"C:\\Users\\Ada Byron\\.teraflock\\bin\\tera.exe\" up`"},
		{installFacts{}, "`tera up`"},
	} {
		env := &doctorEnv{install: tc.in}
		if got := env.cmd("up"); got != tc.want {
			t.Errorf("cmd with %+v = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestHumanSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * 24 * time.Hour: "30 days", 49 * time.Hour: "2 days", 47 * time.Hour: "1 day", 24 * time.Hour: "1 day",
		23 * time.Hour: "23 hours", 90 * time.Minute: "1 hour", 10 * time.Minute: "less than an hour",
	} {
		if got := humanSpan(d); got != want {
			t.Errorf("humanSpan(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestLastBrokenLine(t *testing.T) {
	tail := "level=INFO msg=a\nlevel=ERROR msg=first\nlevel=INFO msg=b\nflockd: second\nlevel=INFO msg=c\n"
	if got := lastBrokenLine(tail); got != "flockd: second" {
		t.Errorf("got %q", got)
	}
	if got := lastBrokenLine("level=INFO msg=fine\n"); got != "" {
		t.Errorf("healthy log = %q", got)
	}
	if got := lastBrokenLine("flockd: " + strings.Repeat("x", 500)); len([]rune(got)) != 240 {
		t.Errorf("long line is %d runes, want 240", len([]rune(got)))
	}
}

func TestExistingDir(t *testing.T) {
	dir := t.TempDir()
	if got := existingDir(dir); got != dir {
		t.Errorf("existing dir = %q", got)
	}
	if got := existingDir(filepath.Join(dir, "models", "deeper")); got != dir {
		t.Errorf("missing descendant resolves to %q, want %q", got, dir)
	}
	// A path with no existing ancestor but the root still terminates.
	if got := existingDir(filepath.Join(string(filepath.Separator), "no-such-dir-for-doctor", "x")); got == "" {
		t.Error("existingDir returned nothing")
	}
}

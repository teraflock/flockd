package main

// The production side of `tera doctor`: the implementations of the small
// interfaces in doctor_checks.go. Everything here only reads.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/enroll"
	"github.com/teraflock/flockd/internal/hardware"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/runtime/llamacpp"
	"github.com/teraflock/flockd/internal/svc"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// newDoctorEnv wires the checks to this machine.
func newDoctorEnv(cmd *cobra.Command) *doctorEnv {
	// Hardware detection and the runtime fetcher log through slog (the
	// too-old NVIDIA card, an unsigned manifest). The doctor reports those
	// itself, in its own lines, so the log copies would only be noise
	// above the report.
	quiet := slog.New(slog.DiscardHandler)
	slog.SetDefault(quiet)

	cfg, cfgErr := config.Load("")
	if cfgErr != nil {
		cfg = config.Default()
	}
	// --data-dir names the daemon's data directory: everything below reads
	// from it, the runtime cache included.
	dd := flagDataDir
	if dd == "" {
		dd = cfg.DataDir
	}
	cfg.DataDir = dd
	home, _ := os.UserHomeDir()

	// --api wins; otherwise a daemon moved with local_api.listen is found
	// where the configuration says it is.
	base, fromConfig := strings.TrimSuffix(flagAPI, "/"), false
	if f := cmd.Flag("api"); f == nil || !f.Changed {
		if b := listenBase(cfg.LocalAPI.Listen); b != "" {
			base, fromConfig = b, true
		}
	}
	if base == "" {
		base = client.DefaultBase
	}

	env := &doctorEnv{
		now:           time.Now,
		goos:          runtime.GOOS,
		goarch:        runtime.GOARCH,
		home:          home,
		teraVersion:   version,
		cfg:           cfg,
		cfgErr:        cfgErr,
		cfgPath:       filepath.Join(config.Default().DataDir, "config.toml"),
		dataDir:       dd,
		apiBase:       base,
		apiFromConfig: fromConfig,
		install:       locateInstall(),
		flockd:        execFlockd{},
		service:       svc.NewManager(),
		linger:        systemLinger{},
		logs:          fileLogs{path: filepath.Join(dd, "flockd.log")},
		enroll:        dirEnroll{dir: dd},
		mesh:          netMesh{},
		hardware:      hostHardware{dataDir: dd},
		runtime:       manifestRuntime{cfg: cfg, log: quiet},
		disk:          hostDisk{},
	}
	// client.New cannot fail for a well-formed base URL; with a malformed
	// one the daemon check reports that no client exists.
	if cl, err := client.New(client.Options{API: base, DataDir: dd, Token: flagToken}); err == nil {
		env.daemon, env.tokenSource = cl, cl.TokenSource
	}
	return env
}

// listenBase turns local_api.listen ("127.0.0.1:7777", ":7777") into the
// URL a client on this machine dials. "" when it is not a host:port.
func listenBase(listen string) string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || port == "" {
		return ""
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// locateInstall finds this binary, whether a bare `tera` runs it, and the
// flockd that `tera up` would install (findFlockd).
func locateInstall() installFacts {
	var in installFacts
	if self, err := os.Executable(); err == nil {
		in.TeraPath = self
	}
	if p, err := exec.LookPath("tera"); err == nil {
		if sameFile(p, in.TeraPath) {
			in.OnPath = true
		} else {
			in.OtherTera = p
		}
	}
	in.FlockdPath, in.FlockdErr = findFlockd()
	return in
}

// sameFile reports whether two paths name one file, following symlinks
// (Homebrew links its binaries into the PATH directory).
func sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// execFlockd runs `flockd -version`, which prints "flockd <version>" and
// exits without touching the data directory.
type execFlockd struct{}

func (execFlockd) Version(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "-version").Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("no answer within %s", doctorFlockdTimeout)
		}
		return "", err
	}
	return parseFlockdVersion(string(out))
}

// parseFlockdVersion reads the first line of `flockd -version`.
func parseFlockdVersion(out string) (string, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[0] != "flockd" {
		return "", fmt.Errorf("unexpected output %q", strings.TrimSpace(line))
	}
	return fields[1], nil
}

// systemLinger reads systemd lingering through internal/svc.
type systemLinger struct{}

func (systemLinger) Linger(ctx context.Context) (user string, on, known, applies bool) {
	return svc.Linger(ctx)
}

// fileLogs is the daemon's own log, read where `tera up` reads it.
type fileLogs struct{ path string }

func (l fileLogs) Tail(ctx context.Context) string { return tailFlockdLogs(ctx, l.path) }

// dirEnroll reads enrollment state from the data directory. It never
// opens the node key and never returns the claim code.
type dirEnroll struct{ dir string }

func (d dirEnroll) Credentials() (*enroll.Credentials, error) {
	return enroll.LoadCredentials(d.dir)
}

func (d dirEnroll) ClaimPending() bool {
	fi, err := os.Stat(enroll.ClaimCodePath(d.dir))
	return err == nil && fi.Size() > 0
}

// hostHardware is the daemon's own detection.
type hostHardware struct{ dataDir string }

func (h hostHardware) Detect(ctx context.Context) (*typesv1.CapabilityProfile, error) {
	return hardware.Detect(ctx, h.dataDir)
}

func (hostHardware) Lanes(p *typesv1.CapabilityProfile) []string {
	return hardware.AccelPreference(p)
}

func (hostHardware) NVIDIA(ctx context.Context) []hardware.NVIDIAGPU {
	return hardware.ProbeNVIDIA(ctx)
}

// manifestRuntime resolves the pinned build with the fetcher `tera up`
// preflights with; Resolve downloads the manifest and nothing else.
type manifestRuntime struct {
	cfg config.Config
	log *slog.Logger
}

func (m manifestRuntime) Resolve(ctx context.Context, lanes ...string) (llamacpp.Selection, error) {
	f, err := runtimeFetcher(m.cfg)
	if err != nil {
		return llamacpp.Selection{}, err
	}
	f.Log = m.log
	f.HTTPClient = &http.Client{Timeout: doctorManifestTimeout}
	return f.Resolve(ctx, lanes...)
}

// hostDisk measures free space the way the daemon's status does.
type hostDisk struct{}

func (hostDisk) FreeBytes(path string) (uint64, error) { return hardware.DiskFreeBytes(path) }

// netMesh is the real connection attempt: resolve, connect, and (unless
// the coordinator is a plaintext development one) a TLS handshake that
// verifies the server exactly as the daemon would and presents no client
// certificate. The node key is never loaded.
type netMesh struct{}

func (netMesh) Probe(ctx context.Context, t meshTarget) meshProbe {
	start := time.Now()
	p := meshProbe{Host: t.Addr}
	done := func(stage string, err error) meshProbe {
		p.Stage, p.Err, p.Elapsed = stage, err, time.Since(start)
		return p
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(t.Addr))
	if err != nil {
		return done(meshStageAddr, err)
	}
	if host == "" || port == "" {
		return done(meshStageAddr, errors.New("the host or the port is empty"))
	}
	p.Host, p.Port = host, port

	if ip := net.ParseIP(host); ip != nil {
		p.IPs = []string{ip.String()}
	} else {
		dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		addrs, err := net.DefaultResolver.LookupIPAddr(dctx, host)
		cancel()
		if err != nil {
			return done(meshStageDNS, err)
		}
		if len(addrs) == 0 {
			return done(meshStageDNS, errors.New("the name has no address"))
		}
		for _, a := range addrs {
			p.IPs = append(p.IPs, a.IP.String())
		}
	}

	tctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	conn, err := (&net.Dialer{}).DialContext(tctx, "tcp", net.JoinHostPort(host, port))
	cancel()
	if err != nil {
		return done(meshStageTCP, err)
	}
	defer func() { _ = conn.Close() }()
	if t.Plaintext {
		return done("", nil)
	}

	minTLS := t.MinTLS
	if minTLS == 0 {
		minTLS = tls.VersionTLS13
	}
	tc := tls.Client(conn, &tls.Config{
		ServerName:         host,
		MinVersion:         minTLS,
		RootCAs:            t.RootCAs,
		InsecureSkipVerify: t.InsecureSkipVerify, //nolint:gosec // mirrors the operator's own tunnel.insecure_skip_verify
		// The tunnel is gRPC, so the daemon offers h2; offer the same.
		NextProtos: []string{"h2"},
		// Runs once the server's chain has verified (or verification is
		// switched off), before the client's own flight.
		VerifyConnection: func(tls.ConnectionState) error {
			p.ServerVerified = true
			return nil
		},
		// Called when the server asks for a client certificate. An empty
		// one sends none: this check proves the path, not the identity.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			p.CertRequested = true
			return &tls.Certificate{}, nil
		},
	})
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		return done(meshStageTLS, err)
	}
	p.TLSVersion = tls.VersionName(tc.ConnectionState().Version)
	return done("", nil)
}

// remoteTLSAlert reports the TLS alert the peer sent, when err is one:
// its number and its text. crypto/tls returns those as a *net.OpError
// with Op "remote error" around an unexported uint8 alert type, so the
// number is read by kind.
func remoteTLSAlert(err error) (code int, text string, ok bool) {
	var oe *net.OpError
	if !errors.As(err, &oe) || oe.Op != "remote error" || oe.Err == nil {
		return 0, "", false
	}
	text = strings.TrimPrefix(oe.Err.Error(), "tls: ")
	if v := reflect.ValueOf(oe.Err); v.Kind() == reflect.Uint8 {
		return int(v.Uint()), text, true
	}
	// A future crypto/tls that changes the type: fall back to the text.
	for c, name := range map[int]string{
		40: "handshake failure", 42: "bad certificate", 46: "unknown certificate",
		48: "unknown certificate authority", 49: "access denied", 116: "certificate required",
	} {
		if text == name {
			return c, text, true
		}
	}
	return -1, text, true
}

// isCertError reports whether err is this side refusing the server's
// certificate (unknown issuer, wrong name, expired).
func isCertError(err error) bool {
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return true
	}
	var ua x509.UnknownAuthorityError
	var hn x509.HostnameError
	var inv x509.CertificateInvalidError
	return errors.As(err, &ua) || errors.As(err, &hn) || errors.As(err, &inv)
}

// meshErrText renders a network error as a phrase for a sentence.
func meshErrText(err error) string {
	if err == nil {
		return "unknown error"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return "the name does not exist"
		case dns.IsTimeout:
			return "the DNS server did not answer in time"
		}
		return "the DNS lookup failed (" + dns.Err + ")"
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return "no answer in time"
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err.Error()
	}
	if errors.Is(err, io.EOF) {
		return "the server closed the connection"
	}
	return err.Error()
}

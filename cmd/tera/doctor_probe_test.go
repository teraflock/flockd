package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/enroll"
)

// tlsServer is a loopback TLS server (httptest's own certificate, valid
// for 127.0.0.1) configured by tune; it returns its address and a pool
// that trusts it.
func tlsServer(t *testing.T, tune func(*tls.Config)) (addr string, roots *x509.CertPool) {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{}
	if tune != nil {
		tune(srv.TLS)
	}
	// A refused handshake is the point of several cases; keep the server's
	// complaint about it out of the test log.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	roots = x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	return srv.Listener.Addr().String(), roots
}

func requireClientCert(c *tls.Config) {
	c.ClientAuth = tls.RequireAndVerifyClientCert
	c.ClientCAs = x509.NewCertPool() // trusts nobody: no certificate can satisfy it
}

func probeCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// The real handshake against real crypto/tls servers, and the verdict the
// mesh check draws from each. The first two are the case to get right: a
// server that insists on a client certificate proves the path is open even
// though this check presents none.
func TestNetMeshProbeTLS(t *testing.T) {
	env := &doctorEnv{cfgPath: "/c/config.toml", install: installFacts{OnPath: true}}

	t.Run("mTLS server, TLS 1.3: the client side of the handshake completes", func(t *testing.T) {
		addr, roots := tlsServer(t, requireClientCert)
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: addr, RootCAs: roots})
		if p.Stage != "" || p.Err != nil || !p.ServerVerified || !p.CertRequested || p.TLSVersion != "TLS 1.3" {
			t.Fatalf("probe = %+v", p)
		}
		if st, text, _ := readMesh(env, p); st != doctorPass {
			t.Fatalf("verdict = %s: %s", st, text)
		}
	})

	t.Run("mTLS server, TLS 1.2: the server ends the handshake with an alert", func(t *testing.T) {
		addr, roots := tlsServer(t, func(c *tls.Config) {
			requireClientCert(c)
			c.MaxVersion = tls.VersionTLS12
		})
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: addr, RootCAs: roots, MinTLS: tls.VersionTLS12})
		if p.Stage != meshStageTLS || p.Err == nil {
			t.Fatalf("expected a refused handshake, got %+v", p)
		}
		code, text, ok := remoteTLSAlert(p.Err)
		if !ok || !alertsForMissingClientCert[code] {
			t.Fatalf("the refusal is not a client-certificate alert: code %d %q ok=%v (%v)", code, text, ok, p.Err)
		}
		if !p.ServerVerified || !p.CertRequested {
			t.Fatalf("the server was verified and asked for a certificate before refusing: %+v", p)
		}
		st, detail, _ := readMesh(env, p)
		if st != doctorPass {
			t.Fatalf("verdict = %s: %s", st, detail)
		}
	})

	t.Run("no client certificate asked", func(t *testing.T) {
		addr, roots := tlsServer(t, nil)
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: addr, RootCAs: roots})
		if p.Stage != "" || !p.ServerVerified || p.CertRequested {
			t.Fatalf("probe = %+v", p)
		}
		if st, _, _ := readMesh(env, p); st != doctorPass {
			t.Fatalf("verdict = %s", st)
		}
	})

	t.Run("untrusted server certificate (interception) fails", func(t *testing.T) {
		addr, _ := tlsServer(t, requireClientCert)
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: addr, RootCAs: x509.NewCertPool()})
		if p.Stage != meshStageTLS || p.ServerVerified || !isCertError(p.Err) {
			t.Fatalf("probe = %+v", p)
		}
		if st, text, _ := readMesh(env, p); st != doctorFail {
			t.Fatalf("verdict = %s: %s", st, text)
		}
	})

	t.Run("insecure_skip_verify accepts it, as the daemon would", func(t *testing.T) {
		addr, _ := tlsServer(t, nil)
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: addr, RootCAs: x509.NewCertPool(), InsecureSkipVerify: true})
		if p.Stage != "" {
			t.Fatalf("probe = %+v", p)
		}
	})

	t.Run("a TLS 1.2-only server is refused: the daemon needs TLS 1.3", func(t *testing.T) {
		addr, roots := tlsServer(t, func(c *tls.Config) { c.MaxVersion = tls.VersionTLS12 })
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: addr, RootCAs: roots})
		if p.Stage != meshStageTLS {
			t.Fatalf("probe = %+v", p)
		}
		if st, _, _ := readMesh(env, p); st != doctorFail {
			t.Fatalf("verdict = %s", st)
		}
	})

	t.Run("a server that is not TLS at all", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ln.Close() })
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
				_ = c.Close()
			}
		}()
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: ln.Addr().String()})
		if p.Stage != meshStageTLS || p.ServerVerified {
			t.Fatalf("probe = %+v", p)
		}
		if st, _, _ := readMesh(env, p); st != doctorFail {
			t.Fatalf("verdict = %s", st)
		}
	})
}

func TestNetMeshProbeTCPAndAddress(t *testing.T) {
	env := &doctorEnv{cfgPath: "/c/config.toml"}

	// A port that was just closed: connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()
	p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: closed})
	if p.Stage != meshStageTCP || p.Err == nil || len(p.IPs) != 1 || p.IPs[0] != "127.0.0.1" {
		t.Fatalf("closed port = %+v", p)
	}
	if st, _, remedy := readMesh(env, p); st != doctorFail || remedy == "" {
		t.Fatalf("closed port verdict = %s", st)
	}

	// Plaintext (tunnel.insecure): DNS and TCP only, no TLS attempted.
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	p = netMesh{}.Probe(probeCtx(t), meshTarget{Addr: open.Addr().String(), Plaintext: true})
	if p.Stage != "" || p.TLSVersion != "" {
		t.Fatalf("plaintext = %+v", p)
	}

	for _, bad := range []string{"", "tunnel.teraflock.ai", ":443", "tunnel.teraflock.ai:", "a:b:c"} {
		p := netMesh{}.Probe(probeCtx(t), meshTarget{Addr: bad})
		if p.Stage != meshStageAddr || p.Err == nil {
			t.Errorf("address %q = %+v, want the address stage", bad, p)
		}
		if st, _, _ := readMesh(env, p); st != doctorFail {
			t.Errorf("address %q verdict = %s", bad, st)
		}
	}
}

// A dead context stops the probe at once instead of waiting on the
// network.
func TestNetMeshProbeHonoursItsContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Accepts and then says nothing: a handshake against it would hang.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	p := netMesh{}.Probe(ctx, meshTarget{Addr: ln.Addr().String()})
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("probe took %s past a 150ms deadline", d)
	}
	if p.Stage != meshStageTLS || p.Err == nil {
		t.Fatalf("probe = %+v", p)
	}
}

func TestRemoteTLSAlert(t *testing.T) {
	if code, text, ok := remoteTLSAlert(remoteAlert(116)); !ok || code != 116 || text != "certificate required" {
		t.Errorf("alert 116 = %d %q %v", code, text, ok)
	}
	for _, err := range []error{nil, context.DeadlineExceeded, &net.OpError{Op: "dial", Err: fakeAlert(42)}, &net.OpError{Op: "remote error"}} {
		if _, _, ok := remoteTLSAlert(err); ok {
			t.Errorf("%v was read as a TLS alert", err)
		}
	}
}

func TestParseFlockdVersion(t *testing.T) {
	for in, want := range map[string]string{
		"flockd 0.9.3\n":           "0.9.3",
		"flockd 8092857-dev":       "8092857-dev",
		"flockd v1.0.0\nextra\n":   "v1.0.0",
		"  flockd   1.2.3  \r\n":   "1.2.3",
		"":                         "",
		"tera 1.0.0":               "",
		"flockd":                   "",
		"flockd 1.0.0 (commit ab)": "",
		"usage: flockd [flags]":    "",
	} {
		got, err := parseFlockdVersion(in)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("parseFlockdVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestListenBase(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:7777": "http://127.0.0.1:7777",
		":9000":          "http://127.0.0.1:9000",
		"0.0.0.0:9000":   "http://127.0.0.1:9000",
		"[::]:9000":      "http://127.0.0.1:9000",
		"[::1]:7777":     "http://[::1]:7777",
		"localhost:7777": "http://localhost:7777",
		"192.168.1.5:80": "http://192.168.1.5:80",
		"":               "",
		"7777":           "",
		"127.0.0.1:":     "",
	} {
		if got := listenBase(in); got != want {
			t.Errorf("listenBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameFile(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "tera")
	if err := os.WriteFile(a, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "other")
	if err := os.WriteFile(b, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !sameFile(a, a) || sameFile(a, b) || sameFile(a, "") || sameFile(a, filepath.Join(dir, "missing")) {
		t.Fatal("sameFile on plain files")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(a, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	if !sameFile(link, a) {
		t.Fatal("a symlink to the binary is the same file")
	}
}

// The enrollment reader reports state without handing out what is in the
// files: a pending claim code is a yes/no.
func TestDirEnroll(t *testing.T) {
	dir := t.TempDir()
	e := dirEnroll{dir: dir}
	if _, err := e.Credentials(); !os.IsNotExist(err) {
		t.Fatalf("no credentials: err = %v", err)
	}
	if e.ClaimPending() {
		t.Fatal("no claim code is stored")
	}
	if err := enroll.SaveClaimCode(dir, "ABCD-EFGH-1234"); err != nil {
		t.Fatal(err)
	}
	if !e.ClaimPending() {
		t.Fatal("a stored claim code is pending")
	}
	if err := os.WriteFile(filepath.Join(dir, "node_creds.pem"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Credentials(); err == nil || os.IsNotExist(err) {
		t.Fatalf("corrupt credentials: err = %v", err)
	}
}

func TestFileLogsTail(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("on Linux the tail is journalctl's, not a file")
	}
	path := filepath.Join(t.TempDir(), "flockd.log")
	if got := (fileLogs{path: path}).Tail(context.Background()); got != "" {
		t.Errorf("missing log file = %q", got)
	}
	if err := os.WriteFile(path, []byte("a\nflockd: boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := (fileLogs{path: path}).Tail(context.Background()); lastBrokenLine(got) != "flockd: boom" {
		t.Errorf("tail = %q", got)
	}
}

package enroll

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hivegrid/hived/internal/tunnel/fakecoord"
	tunnelv1 "github.com/hivegrid/proto/gen/go/hive/tunnel/v1"
	typesv1 "github.com/hivegrid/proto/gen/go/hive/types/v1"
)

func TestIdentityPersistence(t *testing.T) {
	dir := t.TempDir()
	id1, err := LoadOrGenerateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := LoadOrGenerateIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id1.Fingerprint() != id2.Fingerprint() {
		t.Fatal("identity not stable across loads")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(dir, keyFile))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("key perms = %o, want 600", fi.Mode().Perm())
		}
	}
}

func TestCSRIsValid(t *testing.T) {
	id, err := LoadOrGenerateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	csr, err := id.CSR()
	if err != nil {
		t.Fatal(err)
	}
	if len(csr) == 0 {
		t.Fatal("empty CSR")
	}
}

func TestEnrollAgainstFakeCoordinator(t *testing.T) {
	coord, err := fakecoord.New()
	if err != nil {
		t.Fatal(err)
	}
	defer coord.Stop()

	conn, err := coord.Dialer().Dial(context.Background(), coord.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := tunnelv1.NewTunnelServiceClient(conn)

	dir := t.TempDir()
	id, _ := LoadOrGenerateIdentity(dir)
	cap := &typesv1.CapabilityProfile{Os: "darwin", Arch: "arm64"}

	creds, err := Enroll(context.Background(), client, id, "claim-123", cap, dir)
	if err != nil {
		t.Fatal(err)
	}
	if creds.NodeID == "" || len(creds.ClientCertPEM) == 0 || len(creds.CACertPEM) == 0 {
		t.Fatalf("creds = %+v", creds)
	}
	if string(creds.CoordinatorPubKey) != string(coord.PubKey()) {
		t.Fatal("coordinator pubkey not pinned")
	}

	// Round-trip through disk.
	loaded, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.NodeID != creds.NodeID {
		t.Errorf("node id = %q vs %q", loaded.NodeID, creds.NodeID)
	}
	if string(loaded.CoordinatorPubKey) != string(creds.CoordinatorPubKey) {
		t.Error("pubkey lost in round trip")
	}
	if loaded.CertExpiresAt.IsZero() {
		t.Error("expiry lost in round trip")
	}
	if !Enrolled(dir) {
		t.Error("Enrolled() = false after enroll")
	}
}

func TestLoginFlowCallback(t *testing.T) {
	f := &LoginFlow{
		LoginURL: "https://hivegrid.dev/claim",
		OpenBrowser: func(u string) error {
			// Simulate the human+webapp: hit the loopback callback with the
			// same state and a claim code.
			go func() {
				parsed := mustParse(t, u)
				redirect := parsed.Query().Get("redirect_uri")
				state := parsed.Query().Get("state")
				if parsed.Query().Get("code_challenge_method") != "S256" {
					t.Error("missing S256 challenge method")
				}
				time.Sleep(20 * time.Millisecond)
				resp, err := http.Get(redirect + "?claim_code=cc-42&state=" + state)
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, openURL, err := f.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.ClaimCode != "cc-42" {
		t.Errorf("claim code = %q", res.ClaimCode)
	}
	if openURL == "" {
		t.Error("open URL empty")
	}
}

func TestLoginFlowRejectsStateMismatch(t *testing.T) {
	f := &LoginFlow{
		LoginURL: "https://hivegrid.dev/claim",
		OpenBrowser: func(u string) error {
			go func() {
				parsed := mustParse(t, u)
				redirect := parsed.Query().Get("redirect_uri")
				time.Sleep(20 * time.Millisecond)
				resp, err := http.Get(redirect + "?claim_code=cc&state=wrong")
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := f.Run(ctx); err == nil {
		t.Fatal("state mismatch must fail the flow")
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

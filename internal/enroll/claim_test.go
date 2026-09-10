package enroll

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClaimCodeRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if _, err := ReadClaimCode(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadClaimCode on empty dir = %v, want os.ErrNotExist", err)
	}

	if err := SaveClaimCode(dir, "claim-abc123"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadClaimCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "claim-abc123" {
		t.Errorf("code = %q, want claim-abc123", got)
	}

	// A claim code grants enrollment against the operator's account, so it
	// must not be world-readable.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(ClaimCodePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("claim code perms = %o, want 600", fi.Mode().Perm())
		}
	}

	// The browser flow's verifier rides beside the code and is cleared with it.
	if v, err := ReadClaimVerifier(dir); err != nil || v != "" {
		t.Fatalf("ReadClaimVerifier before save = %q, %v", v, err)
	}
	if err := SaveClaimVerifier(dir, "v-secret\n"); err != nil {
		t.Fatal(err)
	}
	if v, err := ReadClaimVerifier(dir); err != nil || v != "v-secret" {
		t.Fatalf("ReadClaimVerifier = %q, %v", v, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(ClaimVerifierPath(dir)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("verifier perms = %v, %v; want 600", fi, err)
		}
	}
	// --claim-code after an abandoned browser flow: no verifier, and the
	// stale one must go so the daemon does not send the wrong proof.
	if err := SaveClaimVerifier(dir, ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := ReadClaimVerifier(dir); v != "" {
		t.Errorf("stale verifier survived an empty save: %q", v)
	}
	if err := SaveClaimVerifier(dir, "v-again"); err != nil {
		t.Fatal(err)
	}

	if err := ClearClaimCode(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClaimCode(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadClaimCode after clear = %v, want os.ErrNotExist", err)
	}
	if v, err := ReadClaimVerifier(dir); err != nil || v != "" {
		t.Errorf("verifier survived ClearClaimCode: %q, %v", v, err)
	}
	// Clearing an already-clear code is not an error — the daemon calls it
	// unconditionally after a successful enrollment.
	if err := ClearClaimCode(dir); err != nil {
		t.Errorf("second ClearClaimCode: %v", err)
	}
}

func TestClaimCodeTrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	// `tera login` writes a trailing newline; operators pasting a code into
	// the file by hand tend to add more.
	if err := os.WriteFile(ClaimCodePath(dir), []byte("  claim-xyz \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadClaimCode(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "claim-xyz" {
		t.Errorf("code = %q, want claim-xyz", got)
	}
}

func TestClaimCodeRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := SaveClaimCode(dir, "   "); err == nil {
		t.Error("SaveClaimCode(whitespace) = nil, want error")
	}
	if _, err := os.Stat(ClaimCodePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Error("empty claim code should not create a file")
	}

	if err := os.WriteFile(ClaimCodePath(dir), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClaimCode(dir); err == nil {
		t.Error("ReadClaimCode(empty file) = nil, want error")
	}
}

func TestSaveClaimCodeCreatesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "teraflock")
	if err := SaveClaimCode(dir, "claim-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClaimCode(dir); err != nil {
		t.Fatal(err)
	}
}

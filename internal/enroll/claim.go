package enroll

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// claimFile holds the one-shot claim code captured by `tera login` until the
// daemon exchanges it for credentials. It is deliberately a plain file in the
// data dir: `tera` and `flockd` are separate processes (often separate user
// sessions once the service is installed), so the filesystem is the handoff.
const claimFile = "claim_code"

// verifierFile holds the PKCE code_verifier that goes with a browser-flow
// claim code. It is a separate file, not a second line in claim_code, so a
// daemon that predates it (flockd <= 0.5.x reads the whole file as the
// code) still parses the handoff; it simply enrols without the verifier
// and the coordinator tells it why that was refused.
const verifierFile = "claim_verifier"

// ClaimCodePath is the handoff location for a pending claim code.
func ClaimCodePath(dataDir string) string {
	return filepath.Join(dataDir, claimFile)
}

// SaveClaimCode persists a claim code for the daemon to consume on its next
// start (0600 — a claim code grants enrollment to the operator's account).
func SaveClaimCode(dataDir, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("enroll: empty claim code")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("enroll: mkdir data dir: %w", err)
	}
	if err := os.WriteFile(ClaimCodePath(dataDir), []byte(code+"\n"), 0o600); err != nil {
		return fmt.Errorf("enroll: write claim code: %w", err)
	}
	return nil
}

// ClaimVerifierPath is the handoff location for the code's PKCE verifier.
func ClaimVerifierPath(dataDir string) string {
	return filepath.Join(dataDir, verifierFile)
}

// SaveClaimVerifier persists the PKCE verifier beside a pending claim code
// (0600 — with the code it is the whole credential). An empty verifier
// removes any stale one so a `--claim-code` login after an abandoned
// browser flow does not send the wrong proof.
func SaveClaimVerifier(dataDir, verifier string) error {
	verifier = strings.TrimSpace(verifier)
	if verifier == "" {
		return clearFile(ClaimVerifierPath(dataDir), "clear claim verifier")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("enroll: mkdir data dir: %w", err)
	}
	if err := os.WriteFile(ClaimVerifierPath(dataDir), []byte(verifier+"\n"), 0o600); err != nil {
		return fmt.Errorf("enroll: write claim verifier: %w", err)
	}
	return nil
}

// ReadClaimVerifier returns the verifier stored beside the claim code, or ""
// when there is none (codes from the claim page's --claim-code path carry
// no PKCE binding).
func ReadClaimVerifier(dataDir string) (string, error) {
	raw, err := os.ReadFile(ClaimVerifierPath(dataDir))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("enroll: read claim verifier: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// ReadClaimCode returns a pending claim code, or os.ErrNotExist when the node
// has nothing to enroll with.
func ReadClaimCode(dataDir string) (string, error) {
	raw, err := os.ReadFile(ClaimCodePath(dataDir))
	if err != nil {
		return "", err // callers test with errors.Is(err, os.ErrNotExist)
	}
	code := strings.TrimSpace(string(raw))
	if code == "" {
		return "", fmt.Errorf("enroll: claim code file is empty")
	}
	return code, nil
}

// ClearClaimCode removes a consumed claim code. Claim codes are one-shot at
// the coordinator, so keeping a spent one around would only produce confusing
// retry failures on every restart.
func ClearClaimCode(dataDir string) error {
	if err := clearFile(ClaimCodePath(dataDir), "clear claim code"); err != nil {
		return err
	}
	return clearFile(ClaimVerifierPath(dataDir), "clear claim verifier")
}

func clearFile(path, what string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("enroll: %s: %w", what, err)
	}
	return nil
}

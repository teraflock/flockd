package enroll

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// claimFile holds the one-shot claim code captured by `hive login` until the
// daemon exchanges it for credentials. It is deliberately a plain file in the
// data dir: `hive` and `hived` are separate processes (often separate user
// sessions once the service is installed), so the filesystem is the handoff.
const claimFile = "claim_code"

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
	err := os.Remove(ClaimCodePath(dataDir))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("enroll: clear claim code: %w", err)
	}
	return nil
}

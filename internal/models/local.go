package models

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// LocalArtifactPath reports whether an artifact_url points at a file already
// on this machine, and returns its filesystem path. Both `file:///abs/path`
// and a bare absolute path are accepted — operators writing a local catalog
// by hand reach for the latter.
//
// This is how a node serves an existing GGUF collection (LM Studio, ollama,
// hand-built quants) without re-downloading gigabytes it already has.
func LocalArtifactPath(artifactURL string) (string, bool) {
	if artifactURL == "" {
		return "", false
	}
	if strings.HasPrefix(artifactURL, "file://") {
		u, err := url.Parse(artifactURL)
		if err != nil {
			return "", false
		}
		p := u.Path
		if p == "" {
			return "", false
		}
		// file://~/x is not a real URL but people write it anyway.
		return expandHome(p), true
	}
	if filepath.IsAbs(artifactURL) || strings.HasPrefix(artifactURL, "~/") {
		return expandHome(artifactURL), true
	}
	return "", false
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~/"))
}

// ensureLocal validates a locally-supplied artifact and returns its path
// unchanged. The file is never copied, moved, or evicted.
func (m *Manager) ensureLocal(path string, spec *typesv1.ModelSpec) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("models: local artifact for %s: %w", spec.GetId(), err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("models: local artifact for %s is a directory: %s", spec.GetId(), path)
	}

	switch want := spec.GetSha256(); {
	case isRealSHA(want):
		// Hash pinning is the whole basis of the fingerprint trust story
		// (SPEC §6), so when a manifest states a hash we hold the file to
		// it — even a local one, where a stale or truncated download is the
		// likeliest failure.
		start := time.Now()
		m.Log.Info("verifying local model artifact", "model", spec.GetId(),
			"path", path, "size_mb", fi.Size()/(1024*1024))
		if err := verifySHA(path, want); err != nil {
			return "", err
		}
		m.Log.Info("local model artifact verified", "model", spec.GetId(), "took", time.Since(start).Round(time.Millisecond))
	default:
		// No usable hash: serve it, but say plainly that the integrity
		// guarantee is off for this model rather than implying it holds.
		m.Log.Warn("local model artifact has no sha256: serving unverified",
			"model", spec.GetId(), "path", path,
			"note", "hash pinning is disabled for this model; the mesh will not schedule it for verified tiers")
	}
	return path, nil
}

// isRealSHA reports whether a manifest carries an actual digest rather than
// the `TODO-verify` placeholder the models repo allows on drafts.
func isRealSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

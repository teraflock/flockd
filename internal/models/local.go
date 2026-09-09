package models

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// LocalArtifactPath reports whether an artifact_url points at a file already
// on this machine, and returns its filesystem path. Accepted forms:
//
//	file:///models/a.gguf      /models/a.gguf      ~/models/a.gguf     (everywhere)
//	file:///C:/models/a.gguf   C:\models\a.gguf    C:/models/a.gguf    (Windows)
//	file://C:\models\a.gguf    \\nas\share\a.gguf  file://nas/share/a.gguf
//
// Operators writing a local catalog by hand reach for the bare path. On
// Windows a rooted POSIX path (/models/a.gguf) is accepted too and, as
// with any Windows API, resolves against the current drive; a catalog
// written on one platform is usable on the other. Percent-escapes in a
// file:// URL are decoded (file:///models/my%20model.gguf).
//
// This is how a node serves an existing GGUF collection (LM Studio, ollama,
// hand-built quants) without re-downloading gigabytes it already has.
func LocalArtifactPath(artifactURL string) (string, bool) {
	p, ok := localArtifactSlashPath(artifactURL, runtime.GOOS == "windows")
	if !ok {
		return "", false
	}
	return filepath.Clean(filepath.FromSlash(expandHome(p))), true
}

// LocalArtifactURL is the inverse: the file:// URL for a filesystem path,
// in the form every platform's LocalArtifactPath accepts (file:///C:/x on
// Windows, file:///x elsewhere, file://nas/share/x for a UNC path). Build
// artifact_url values with this rather than "file://" + path: that yields
// file://C:\x on Windows, which LocalArtifactPath tolerates but nothing
// else does.
func LocalArtifactURL(path string) string {
	p := filepath.ToSlash(path)
	if host, rest, ok := strings.Cut(strings.TrimPrefix(p, "//"), "/"); ok && strings.HasPrefix(p, "//") {
		return (&url.URL{Scheme: "file", Host: host, Path: "/" + rest}).String()
	}
	return (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(p, "/")}).String()
}

// localArtifactSlashPath is the platform-independent core: it returns the
// path in slash form with the drive letter or UNC host kept ("C:/x",
// "//nas/share/x"), so the Windows rules are testable on every OS. The
// windows flag says whether backslashes are separators and drive letters
// and UNC hosts are meaningful.
func localArtifactSlashPath(s string, windows bool) (string, bool) {
	if s == "" {
		return "", false
	}
	if windows {
		// Backslashes are path separators, not URL characters: file://C:\x
		// is what "file://" + filepath.Join(...) produces there.
		s = strings.ReplaceAll(s, `\`, "/")
	}
	if len(s) >= len("file://") && strings.EqualFold(s[:len("file://")], "file://") {
		return fileURLPath(s[len("file://"):], windows)
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") || (windows && isDrivePath(s)) {
		return s, true
	}
	return "", false
}

// fileURLPath resolves what follows "file://": an optional authority and a
// path. RFC 8089 gives file:///p and file://localhost/p; Windows adds
// file:///C:/p (leading slash before the drive), the sloppy file://C:/p,
// and file://host/share/p for a UNC path. file://~/p is not a URL at all
// but people write it, so it means ~/p.
func fileURLPath(rest string, windows bool) (string, bool) {
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	if dec, err := url.PathUnescape(path); err == nil {
		path = dec
	}
	switch {
	case authority == "" || strings.EqualFold(authority, "localhost"):
		if windows && isDrivePath(strings.TrimPrefix(path, "/")) {
			path = strings.TrimPrefix(path, "/")
		}
		return path, path != ""
	case authority == "~":
		return "~" + path, path != ""
	case windows && isDrivePath(authority+path):
		return authority + path, true
	case windows && path != "":
		return "//" + authority + path, true
	}
	// A file on some other host is not a local artifact.
	return "", false
}

// isDrivePath reports whether p starts with a drive letter and a separator
// ("C:/x" in slash form). "C:x" is drive-relative and not accepted.
func isDrivePath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' &&
		(('a' <= p[0] && p[0] <= 'z') || ('A' <= p[0] && p[0] <= 'Z'))
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

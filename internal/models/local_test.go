package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// The platform-independent core, exercised with both rule sets on every
// OS so the Windows behaviour is covered by the macOS/Linux test runs too.
func TestLocalArtifactSlashPath(t *testing.T) {
	cases := []struct {
		in      string
		windows bool
		want    string
		ok      bool
	}{
		// POSIX rules.
		{"file:///models/a.gguf", false, "/models/a.gguf", true},
		{"file://localhost/models/a.gguf", false, "/models/a.gguf", true},
		{"FILE:///models/a.gguf", false, "/models/a.gguf", true},
		{"file:///models/my%20model.gguf", false, "/models/my model.gguf", true},
		{"file://~/models/a.gguf", false, "~/models/a.gguf", true},
		{"/models/a.gguf", false, "/models/a.gguf", true},
		{"~/models/a.gguf", false, "~/models/a.gguf", true},
		{"file://nas/share/a.gguf", false, "", false}, // some other host
		{`C:\models\a.gguf`, false, "", false},        // just a relative name on POSIX
		{"file://", false, "", false},
		{"file:///", false, "/", true}, // the root; ensureLocal rejects directories
		{"https://huggingface.co/x/y.gguf", false, "", false},
		{"http://example.com/y.gguf", false, "", false},
		{"", false, "", false},
		{"relative/path.gguf", false, "", false},
		// Windows rules: drive letters, UNC, backslashes, and the POSIX
		// forms too (rooted paths resolve against the current drive).
		{"file:///C:/models/a.gguf", true, "C:/models/a.gguf", true},
		{"file://localhost/C:/models/a.gguf", true, "C:/models/a.gguf", true},
		{"file://C:/models/a.gguf", true, "C:/models/a.gguf", true},
		{`file://C:\models\a.gguf`, true, "C:/models/a.gguf", true},
		{`file:///C:\models\my%20model.gguf`, true, "C:/models/my model.gguf", true},
		{`C:\models\a.gguf`, true, "C:/models/a.gguf", true},
		{"c:/models/a.gguf", true, "c:/models/a.gguf", true},
		{`\\nas\share\a.gguf`, true, "//nas/share/a.gguf", true},
		{"file://nas/share/a.gguf", true, "//nas/share/a.gguf", true},
		{`file:////nas/share/a.gguf`, true, "//nas/share/a.gguf", true},
		{"file:///models/a.gguf", true, "/models/a.gguf", true},
		{"/models/a.gguf", true, "/models/a.gguf", true},
		{`~\models\a.gguf`, true, "~/models/a.gguf", true},
		{"file://~/models/a.gguf", true, "~/models/a.gguf", true},
		{"C:models/a.gguf", true, "", false}, // drive-relative
		{"file://nas", true, "", false},      // host, no path
		{"https://huggingface.co/x/y.gguf", true, "", false},
		{"relative/path.gguf", true, "", false},
		{`relative\path.gguf`, true, "", false},
	}
	for _, c := range cases {
		got, ok := localArtifactSlashPath(c.in, c.windows)
		if ok != c.ok || got != c.want {
			t.Errorf("localArtifactSlashPath(%q, windows=%v) = (%q, %v), want (%q, %v)", c.in, c.windows, got, ok, c.want, c.ok)
		}
	}
}

// The native wrapper on whichever OS the test runs: results are clean
// paths in the OS's separator.
func TestLocalArtifactPath(t *testing.T) {
	type tc struct {
		in       string
		wantPath string
		wantOK   bool
	}
	home, _ := os.UserHomeDir()
	abs := filepath.FromSlash("/models/a.gguf")
	cases := []tc{
		{"file:///models/a.gguf", abs, true},
		{"/models/a.gguf", abs, true},
		{"~/models/a.gguf", filepath.Join(home, "models", "a.gguf"), true},
		{"file://~/models/a.gguf", filepath.Join(home, "models", "a.gguf"), true},
		{"file:///models/my%20model.gguf", filepath.FromSlash("/models/my model.gguf"), true},
		{"https://huggingface.co/x/y.gguf", "", false},
		{"http://example.com/y.gguf", "", false},
		{"", "", false},
		{"relative/path.gguf", "", false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			tc{"file:///C:/models/a.gguf", `C:\models\a.gguf`, true},
			tc{`file://C:\models\a.gguf`, `C:\models\a.gguf`, true},
			tc{`C:\models\a.gguf`, `C:\models\a.gguf`, true},
			tc{`\\nas\share\a.gguf`, `\\nas\share\a.gguf`, true},
		)
	}
	for _, c := range cases {
		got, ok := LocalArtifactPath(c.in)
		if ok != c.wantOK || (ok && got != c.wantPath) {
			t.Errorf("LocalArtifactPath(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.wantPath, c.wantOK)
		}
	}
}

// LocalArtifactURL produces the canonical form, and the two functions
// round-trip on every platform — including through spaces.
func TestLocalArtifactURLRoundTrip(t *testing.T) {
	if got, want := LocalArtifactURL(filepath.FromSlash("/models/my model.gguf")), "file:///models/my%20model.gguf"; got != want {
		t.Errorf("LocalArtifactURL = %q, want %q", got, want)
	}
	if runtime.GOOS == "windows" {
		if got, want := LocalArtifactURL(`C:\models\a.gguf`), "file:///C:/models/a.gguf"; got != want {
			t.Errorf("drive: %q, want %q", got, want)
		}
		if got, want := LocalArtifactURL(`\\nas\share\a.gguf`), "file://nas/share/a.gguf"; got != want {
			t.Errorf("UNC: %q, want %q", got, want)
		}
	}
	for _, p := range []string{
		filepath.Join(t.TempDir(), "plain.gguf"),
		filepath.Join(t.TempDir(), "with space.gguf"),
		filepath.Join(t.TempDir(), "100%.gguf"),
	} {
		got, ok := LocalArtifactPath(LocalArtifactURL(p))
		if !ok || got != p {
			t.Errorf("round trip %q -> %q -> (%q, %v)", p, LocalArtifactURL(p), got, ok)
		}
	}
}

func writeGGUF(t *testing.T, dir, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	_, _ = io.WriteString(h, content)
	return path, hex.EncodeToString(h.Sum(nil))
}

func localManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), 1024, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A local artifact is served from where it already lives — copying a 20GB
// GGUF into the cache to satisfy bookkeeping would be absurd.
func TestEnsureLocalServesInPlaceWithoutCopying(t *testing.T) {
	src := t.TempDir()
	path, sum := writeGGUF(t, src, "porkchop.gguf", "pretend weights")
	m := localManager(t)

	// Both the canonical URL and the naive "file://" + path (which on
	// Windows is file://C:\... — a footgun, but one we defuse) resolve.
	for _, u := range []string{LocalArtifactURL(path), "file://" + path} {
		got, err := m.Ensure(context.Background(), &typesv1.ModelSpec{
			Id: "porkchop", ArtifactUrl: u, Sha256: sum,
		})
		if err != nil {
			t.Fatalf("%s: %v", u, err)
		}
		if got != path {
			t.Errorf("%s: Ensure returned %q, want the original path %q", u, got, path)
		}
	}
	if _, err := os.Stat(filepath.Join(m.Dir, "porkchop.gguf")); !os.IsNotExist(err) {
		t.Error("local artifact was copied into the cache dir")
	}
}

func TestEnsureLocalRejectsBadHash(t *testing.T) {
	src := t.TempDir()
	path, _ := writeGGUF(t, src, "m.gguf", "real weights")
	m := localManager(t)

	_, err := m.Ensure(context.Background(), &typesv1.ModelSpec{
		Id: "m", ArtifactUrl: LocalArtifactURL(path),
		Sha256: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("Ensure accepted a local file whose hash does not match the manifest")
	}
}

// Operators' own models have no published digest; serving them is allowed,
// but only because the manifest says so explicitly.
func TestEnsureLocalAllowsPlaceholderHash(t *testing.T) {
	src := t.TempDir()
	path, _ := writeGGUF(t, src, "mine.gguf", "weights")
	m := localManager(t)

	for _, sha := range []string{"", "TODO-verify"} {
		got, err := m.Ensure(context.Background(), &typesv1.ModelSpec{
			Id: "mine", ArtifactUrl: path, Sha256: sha,
		})
		if err != nil {
			t.Fatalf("sha %q: %v", sha, err)
		}
		if got != path {
			t.Errorf("sha %q: got %q, want %q", sha, got, path)
		}
	}
}

func TestEnsureLocalMissingFile(t *testing.T) {
	m := localManager(t)
	_, err := m.Ensure(context.Background(), &typesv1.ModelSpec{
		Id: "ghost", ArtifactUrl: "file:///nope/missing.gguf",
	})
	if err == nil {
		t.Fatal("Ensure accepted a nonexistent local artifact")
	}
}

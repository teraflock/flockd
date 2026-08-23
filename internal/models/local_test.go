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

func TestLocalArtifactPath(t *testing.T) {
	// POSIX-absolute paths like "/models/a.gguf" aren't absolute on Windows
	// (needs a drive letter) — filepath.IsAbs returns false, so the function
	// correctly rejects them. Windows operators write "C:\models\a.gguf" or
	// "file:///C:/models/a.gguf" instead, but that's a separate coverage
	// gap tracked as a follow-up.
	if runtime.GOOS == "windows" {
		t.Skip("Windows-specific path semantics are a follow-up (see #TODO)")
	}
	home, _ := os.UserHomeDir()
	cases := []struct {
		in       string
		wantPath string
		wantOK   bool
	}{
		{"file:///models/a.gguf", "/models/a.gguf", true},
		{"/models/a.gguf", "/models/a.gguf", true},
		{"~/models/a.gguf", filepath.Join(home, "models/a.gguf"), true},
		{"https://huggingface.co/x/y.gguf", "", false},
		{"http://example.com/y.gguf", "", false},
		{"", "", false},
		{"relative/path.gguf", "", false},
	}
	for _, c := range cases {
		got, ok := LocalArtifactPath(c.in)
		if ok != c.wantOK || (ok && got != c.wantPath) {
			t.Errorf("LocalArtifactPath(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.wantPath, c.wantOK)
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
	// See TestLocalArtifactPath's skip: file://<windows-path> URL parsing is
	// a separate follow-up.
	if runtime.GOOS == "windows" {
		t.Skip("Windows-specific file:// URL semantics are a follow-up (see #TODO)")
	}
	src := t.TempDir()
	path, sum := writeGGUF(t, src, "porkchop.gguf", "pretend weights")
	m := localManager(t)

	got, err := m.Ensure(context.Background(), &typesv1.ModelSpec{
		Id: "porkchop", ArtifactUrl: "file://" + path, Sha256: sum,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Errorf("Ensure returned %q, want the original path %q", got, path)
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
		Id: "m", ArtifactUrl: "file://" + path,
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

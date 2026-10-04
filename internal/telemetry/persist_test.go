package telemetry

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// run simulates one daemon run: restore, count, and (optionally) the
// graceful final save.
func newRun(t *testing.T, dir string) (*Stats, *Persister) {
	t.Helper()
	s := NewStats()
	p := &Persister{Stats: s, DataDir: dir, Log: quiet()}
	p.Restore()
	return s, p
}

func TestLifetimeCountersSurviveRestarts(t *testing.T) {
	dir := t.TempDir()

	// First run: nothing on disk.
	s1, p1 := newRun(t, dir)
	if snap := s1.Snapshot(); snap.TotalTokens != 0 || snap.LifetimeSince.IsZero() {
		t.Fatalf("fresh node = %+v", snap)
	}
	s1.RecordTokens(100)
	s1.RecordRequest(100, 5500)
	since := s1.Snapshot().LifetimeSince
	p1.Save() // graceful shutdown

	// Second run continues the lifetime figures; session figures restart.
	s2, p2 := newRun(t, dir)
	snap := s2.Snapshot()
	if snap.TotalTokens != 100 || snap.TotalRequests != 1 || snap.EarnedMicrocred != 5500 {
		t.Fatalf("restored lifetime = %+v", snap)
	}
	if snap.SessionTokens != 0 || snap.SessionRequests != 0 || snap.SessionEarnedMicrocred != 0 {
		t.Fatalf("session counters did not restart: %+v", snap)
	}
	if !snap.LifetimeSince.Equal(since) {
		t.Fatalf("lifetime_since moved: %v -> %v", since, snap.LifetimeSince)
	}
	// Rates are rolling windows: a restart starts them empty, whatever
	// the lifetime totals say.
	if snap.TokensPerSec1m != 0 || snap.RequestsPerMin != 0 {
		t.Fatalf("rates after a restart = %+v", snap)
	}
	s2.RecordTokens(40)
	s2.RecordRequest(40, 2200)
	snap = s2.Snapshot()
	if snap.TotalTokens != 140 || snap.SessionTokens != 40 || snap.TotalRequests != 2 || snap.SessionRequests != 1 ||
		snap.EarnedMicrocred != 7700 || snap.SessionEarnedMicrocred != 2200 {
		t.Fatalf("second run = %+v", snap)
	}
	if snap.TokensPerSec1m != 40.0/60 || snap.RequestsPerMin != 1 {
		t.Fatalf("rates count this run only: %+v", snap)
	}
	p2.Save()

	// Crash safety: the third run saves once, counts more, and is killed
	// before the next write. Only what came after the last save is lost.
	s3, p3 := newRun(t, dir)
	s3.RecordTokens(10)
	p3.Save()
	s3.RecordTokens(7) // never saved: kill -9 here
	s4, _ := newRun(t, dir)
	if got := s4.Snapshot().TotalTokens; got != 150 {
		t.Fatalf("after a crash: total_tokens = %d, want 150 (the 7 since the last save are lost, nothing else)", got)
	}

	// The file holds counters and timestamps only.
	raw, err := os.ReadFile(filepath.Join(dir, StatsFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version":1`, `"since":`, `"saved_at":`, `"total_tokens":150`, `"total_requests":2`, `"earned_microcredits":7700`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("stats.json lacks %s: %s", key, raw)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, StatsFile)); fi.Mode().Perm()&0o077 != 0 && os.Getenv("OS") != "Windows_NT" {
		t.Fatalf("stats.json mode = %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(dir, StatsFile+".tmp")); err == nil {
		t.Fatal("temp file left behind")
	}
}

// A torn or foreign file never fails the start: it is set aside and
// counting starts from zero; a half-written temp file is ignored.
func TestCorruptCountersFile(t *testing.T) {
	for name, content := range map[string]string{
		"truncated": `{"version":1,"total_tok`,
		"garbage":   "\x00\x00not json",
		"version":   `{"version":99,"total_tokens":5}`,
		"negative":  `{"version":1,"total_tokens":-5}`,
		"empty":     ``,
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, StatsFile)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s, p := newRun(t, dir)
		if snap := s.Snapshot(); snap.TotalTokens != 0 {
			t.Fatalf("%s: restored %d tokens from a bad file", name, snap.TotalTokens)
		}
		if _, err := os.Stat(path + ".bad"); err != nil {
			t.Fatalf("%s: bad file not set aside: %v", name, err)
		}
		s.RecordTokens(3)
		p.Save()
		if c, ok, err := LoadCounters(dir); err != nil || !ok || c.Tokens != 3 {
			t.Fatalf("%s: after recovery = %+v %v %v", name, c, ok, err)
		}
	}
	// A crash between writing the temp file and the rename leaves the old
	// file intact and a stray .tmp, which the next save replaces.
	dir := t.TempDir()
	s, p := newRun(t, dir)
	s.RecordTokens(50)
	p.Save()
	if err := os.WriteFile(filepath.Join(dir, StatsFile+".tmp"), []byte(`{"version":1,"total_tok`), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, p2 := newRun(t, dir)
	if got := s2.Snapshot().TotalTokens; got != 50 {
		t.Fatalf("with a torn temp file: total_tokens = %d, want 50", got)
	}
	s2.RecordTokens(1)
	p2.Save()
	if c, _, _ := LoadCounters(dir); c.Tokens != 51 {
		t.Fatalf("save over a stray temp file = %+v", c)
	}
}

// Run writes on the timer while the counters move, not when they do not,
// and once more when the daemon shuts down.
func TestPersisterRun(t *testing.T) {
	dir := t.TempDir()
	s, p := newRun(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx, 5*time.Millisecond); close(done) }()

	path := filepath.Join(dir, StatsFile)
	s.RecordTokens(9)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, ok, _ := LoadCounters(dir); ok && c.Tokens == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timer never saved the counters")
		}
		time.Sleep(5 * time.Millisecond)
	}
	fi1, _ := os.Stat(path)
	time.Sleep(40 * time.Millisecond)
	if fi2, _ := os.Stat(path); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("unchanged counters were rewritten")
	}
	// Counted just before shutdown: the final save catches it.
	s.RecordTokens(1)
	cancel()
	<-done
	if c, _, _ := LoadCounters(dir); c.Tokens != 10 {
		t.Fatalf("after shutdown = %+v, want 10 tokens", c)
	}
}

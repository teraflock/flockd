package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Lifetime counters survive a daemon restart in <data_dir>/stats.json:
//
//	{"version":1,"since":"2026-10-04T12:00:00Z","saved_at":"…",
//	 "total_tokens":123456,"total_requests":789,"earned_microcredits":6790080}
//
// Counters only — nothing about any request. The file is rewritten whole
// (temp file + rename, so a crash never leaves half a file) every
// SaveInterval while the counters move and once more on a graceful
// shutdown; a crash or power loss costs at most the last interval. A
// missing file starts from zero; an unreadable one is set aside as
// stats.json.bad and counting starts from zero, never failing the start.
// Saving runs on its own goroutine and reads the counters with one short
// lock: the request path never waits on the disk.

// StatsFile is the file name inside the data dir.
const StatsFile = "stats.json"

// SaveInterval is how often moving counters are written.
const SaveInterval = 30 * time.Second

const statsVersion = 1

type statsFile struct {
	Version int       `json:"version"`
	Since   time.Time `json:"since"`
	SavedAt time.Time `json:"saved_at"`
	Counters
}

// LoadCounters reads the persisted lifetime counters. ok is false when
// there are none (first run, or a file that could not be used, which is
// then moved aside); err describes an unusable file and is for logging.
func LoadCounters(dataDir string) (c Counters, ok bool, err error) {
	path := filepath.Join(dataDir, StatsFile)
	raw, rerr := os.ReadFile(path)
	if errors.Is(rerr, os.ErrNotExist) {
		return Counters{}, false, nil
	}
	if rerr != nil {
		return Counters{}, false, fmt.Errorf("telemetry: read %s: %w", path, rerr)
	}
	var f statsFile
	if jerr := json.Unmarshal(raw, &f); jerr != nil || f.Version != statsVersion ||
		f.Tokens < 0 || f.Requests < 0 || f.EarnedMicrocred < 0 {
		_ = os.Rename(path, path+".bad")
		if jerr == nil {
			jerr = fmt.Errorf("unexpected contents (version %d)", f.Version)
		}
		return Counters{}, false, fmt.Errorf("telemetry: %s is not usable, set aside as %s.bad: %w", path, StatsFile, jerr)
	}
	c = f.Counters
	c.Since = f.Since
	return c, true, nil
}

// SaveCounters writes the counters atomically.
func SaveCounters(dataDir string, c Counters, now time.Time) error {
	raw, err := json.Marshal(statsFile{Version: statsVersion, Since: c.Since, SavedAt: now, Counters: c})
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, StatsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("telemetry: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("telemetry: finalize %s: %w", path, err)
	}
	return nil
}

// Persister keeps a Stats' lifetime counters on disk.
type Persister struct {
	Stats   *Stats
	DataDir string
	Log     *slog.Logger

	last Counters // what the file holds
}

// Restore loads the persisted counters into Stats. Never fails the
// caller: an unusable file is logged and counting starts from zero.
func (p *Persister) Restore() {
	c, ok, err := LoadCounters(p.DataDir)
	if err != nil {
		p.log().Warn("lifetime counters not restored; starting from zero", "err", err)
	}
	if ok {
		p.Stats.Restore(c)
		p.last = c
		p.log().Info("lifetime counters restored", "total_tokens", c.Tokens, "total_requests", c.Requests, "since", c.Since)
	}
}

// Save writes the counters if they changed since the last write.
func (p *Persister) Save() {
	c := p.Stats.Lifetime()
	if c == p.last {
		return
	}
	if err := SaveCounters(p.DataDir, c, time.Now()); err != nil {
		p.log().Warn("lifetime counters not saved", "err", err)
		return
	}
	p.last = c
}

// Run saves every interval (<= 0: SaveInterval) until ctx ends, then once
// more: the graceful-shutdown write.
func (p *Persister) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = SaveInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			p.Save()
			return
		case <-t.C:
			p.Save()
		}
	}
}

func (p *Persister) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

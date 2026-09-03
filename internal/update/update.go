// Package update checks the mesh's version feed for a newer flockd
// release. It never installs anything: it tells the operator (status,
// SSE, CLI, TUI, desktop banner) and points at the release page. Brew
// users run `brew upgrade --cask tera`; signed self-update is gated on
// plan 01.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/events"
)

// DefaultFeedURL is the control plane's public version feed.
const DefaultFeedURL = "https://api.teraflock.ai/v1/versions"

// Feed is the version feed document:
//
//	{"flockd":{"latest":"0.4.0","minimum":"0.3.0","url":"…"},
//	 "desktop":{"latest":"…","url":"…"}}
type Feed struct {
	Flockd struct {
		Latest  string `json:"latest"`
		Minimum string `json:"minimum"`
		URL     string `json:"url"`
	} `json:"flockd"`
	Desktop struct {
		Latest string `json:"latest"`
		URL    string `json:"url"`
	} `json:"desktop"`
}

// Result is one completed check.
type Result struct {
	// Available is true when Latest is a newer release than Current.
	// Always false for dev builds (never nag a developer's own binary).
	Available    bool
	Current      string
	Latest       string
	Minimum      string
	BelowMinimum bool
	URL          string
	CheckedAt    time.Time
}

// ErrFeedUnavailable wraps feed fetch failures (404 while the endpoint is
// being built, connection errors). The result is "unknown", not an error
// the operator sees.
var ErrFeedUnavailable = errors.New("update: version feed unavailable")

// Checker polls the feed.
type Checker struct {
	FeedURL string
	// Current is the running build version (main.version).
	Current string
	Client  *http.Client
	Log     *slog.Logger
	// Events receives `update_available` once per newly discovered
	// version. May be nil.
	Events *events.Hub
	// Activity gets an update_available row once per version. May be nil.
	Activity *activity.Ring
	// InitialDelay before the first check (lets the daemon finish booting)
	// and Interval between checks. Zero = 30s / 1h.
	InitialDelay time.Duration
	Interval     time.Duration

	mu        sync.Mutex
	last      *Result
	announced string
	warned    bool
}

// Check fetches the feed now and records the result.
func (c *Checker) Check(ctx context.Context) (Result, error) {
	url := c.FeedURL
	if url == "" {
		url = DefaultFeedURL
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrFeedUnavailable, err)
	}
	req.Header.Set("User-Agent", "flockd/"+c.Current)
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrFeedUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%w: status %s", ErrFeedUnavailable, resp.Status)
	}
	var feed Feed
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&feed); err != nil {
		return Result{}, fmt.Errorf("%w: decode: %v", ErrFeedUnavailable, err)
	}
	res := Evaluate(c.Current, feed, time.Now())
	c.record(res)
	return res, nil
}

// Evaluate compares a feed against the running version.
func Evaluate(current string, feed Feed, now time.Time) Result {
	res := Result{
		Current:   current,
		Latest:    feed.Flockd.Latest,
		Minimum:   feed.Flockd.Minimum,
		URL:       feed.Flockd.URL,
		CheckedAt: now,
	}
	if !IsRelease(current) {
		return res // dev build: report what is out there, never nag
	}
	if IsRelease(res.Latest) && Compare(res.Latest, current) > 0 {
		res.Available = true
	}
	if IsRelease(res.Minimum) && Compare(current, res.Minimum) < 0 {
		res.BelowMinimum = true
	}
	return res
}

func (c *Checker) record(res Result) {
	c.mu.Lock()
	c.last = &res
	announce := res.Available && c.announced != res.Latest
	if announce {
		c.announced = res.Latest
	}
	c.mu.Unlock()
	if !announce {
		return
	}
	c.log().Info("flockd update available", "current", res.Current, "latest", res.Latest, "url", res.URL, "below_minimum", res.BelowMinimum)
	c.Events.Publish("update_available", res.Public())
	msg := fmt.Sprintf("flockd %s is available (running %s)", res.Latest, res.Current)
	if res.BelowMinimum {
		msg += " — below the mesh minimum; the coordinator drains this node until it is updated"
	}
	c.Activity.Record(activity.KindUpdateAvailable, activity.ActorDaemon, "", msg, res.URL)
}

// Last returns the most recent successful check.
func (c *Checker) Last() (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return Result{}, false
	}
	return *c.last, true
}

// Run checks after InitialDelay and then every Interval until ctx ends.
// Feed failures are logged once at info and then at debug: the endpoint
// may simply not exist yet.
func (c *Checker) Run(ctx context.Context) {
	delay, interval := c.InitialDelay, c.Interval
	if delay <= 0 {
		delay = 30 * time.Second
	}
	if interval <= 0 {
		interval = time.Hour
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	for {
		if _, err := c.Check(ctx); err != nil && ctx.Err() == nil {
			c.mu.Lock()
			warned := c.warned
			c.warned = true
			c.mu.Unlock()
			if warned {
				c.log().Debug("version check skipped", "err", err)
			} else {
				c.log().Info("version check unavailable; will keep trying hourly", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (c *Checker) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Public is the wire shape shared by /api/v1/status.update, the
// update/check route and the SSE event (mirrors the Update schema).
type Public struct {
	Available    bool      `json:"available"`
	Current      string    `json:"current"`
	Latest       string    `json:"latest"`
	Minimum      string    `json:"minimum,omitempty"`
	BelowMinimum bool      `json:"below_minimum,omitempty"`
	URL          string    `json:"url,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
}

// Public converts a Result for the wire.
func (r Result) Public() Public {
	return Public{Available: r.Available, Current: r.Current, Latest: r.Latest, Minimum: r.Minimum,
		BelowMinimum: r.BelowMinimum, URL: r.URL, CheckedAt: r.CheckedAt}
}

// ---- semver ----

// IsRelease reports whether v is a plain release version (x.y.z with an
// optional v prefix). Anything else — "dev", "ad2a146-dev", "0.4.0-rc1" —
// is a development build.
func IsRelease(v string) bool {
	_, ok := parse(v)
	return ok
}

// Compare orders two release versions (-1, 0, 1). Non-release inputs sort
// below every release.
func Compare(a, b string) int {
	pa, oka := parse(a)
	pb, okb := parse(b)
	switch {
	case !oka && !okb:
		return 0
	case !oka:
		return -1
	case !okb:
		return 1
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if v == "" {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

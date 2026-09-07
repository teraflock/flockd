package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The write half of the models API (plan 10 Phase A): pull, load, unload,
// default. Every one goes through the daemon — the CLI never downloads or
// touches the model store itself (flockd#42).

func cmdModelsPull() *cobra.Command {
	var noWait bool
	c := &cobra.Command{
		Use:   "pull <model-id>",
		Short: "Download a catalog model (resumes; progress bar until ready)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPull(cmd, args[0], noWait)
		},
	}
	c.Flags().BoolVar(&noWait, "no-wait", false, "start the download and return immediately")
	return c
}

func cmdModelsLoad() *cobra.Command {
	return &cobra.Command{
		Use:   "load <model-id>",
		Short: "Load a model into the serving runtime (downloads first if needed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), styleDim.Render("loading "+args[0]+" … (llama-server startup takes a few seconds)"))
			// Synchronous on the daemon side and legitimately slow;
			// ctrl-c cancels our wait, not the load.
			if err := cl.postLong(cmd.Context(), "/api/v1/models/"+args[0]+"/load", nil, nil); err != nil {
				return err
			}
			return reportModel(cmd, cl, "loaded", args[0])
		},
	}
}

func cmdModelsUnload() *cobra.Command {
	return &cobra.Command{
		Use:   "unload <model-id>",
		Short: "Unload a model from the runtime (the file stays cached)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			if err := cl.post("/api/v1/models/"+args[0]+"/unload", nil, nil); err != nil {
				return err
			}
			return reportModel(cmd, cl, "unloaded", args[0])
		},
	}
}

func cmdModelsDefault() *cobra.Command {
	return &cobra.Command{
		Use:   "default <model-id>",
		Short: "Serve this model when a request names none (must be loaded)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := client()
			if err != nil {
				return err
			}
			if err := cl.post("/api/v1/models/"+args[0]+"/default", nil, nil); err != nil {
				return err
			}
			return reportModel(cmd, cl, "default is now", args[0])
		},
	}
}

// reportModel prints the success line and the model's row from
// GET /api/v1/models, so the operator sees the resulting state.
func reportModel(cmd *cobra.Command, cl *apiClient, verb, id string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, styleOK.Render("✓"), verb, id)
	row, ok, err := cl.findModel(id)
	if err != nil || !ok {
		return err
	}
	fmt.Fprintln(out, modelsHeader())
	fmt.Fprintln(out, modelRowLine(row))
	return nil
}

func modelsHeader() string {
	return fmt.Sprintf("%-40s %-12s %-10s %-6s %-9s %s", "MODEL", "STATE", "SIZE", "PIN", "ORIGIN", "LOADED")
}

// modelRowLine renders one `tera models list` row.
func modelRowLine(m modelRow) string {
	size := "—"
	if m.SizeBytes > 0 {
		size = fmt.Sprintf("%.1fGB", float64(m.SizeBytes)/1e9)
	}
	pin, loaded := "", ""
	if m.Pinned {
		pin = "📌"
	}
	if m.Loaded {
		loaded = styleOK.Render("●")
		if m.LoadedMB != nil {
			loaded += fmt.Sprintf(" %.1fGB", float64(*m.LoadedMB)/1024)
		}
		if m.IdleSince != nil {
			loaded += styleDim.Render(" idle " + time.Since(*m.IdleSince).Round(time.Second).String())
		}
	}
	name := m.ID
	if m.Default {
		name += styleDim.Render(" (default)")
	}
	state := m.State
	switch {
	case state == "missing":
		state = styleWarn.Render(state)
	case state == "downloading" && m.ReceivedBytes != nil && m.SizeBytes > 0:
		state = fmt.Sprintf("%s %d%%", state, *m.ReceivedBytes*100/m.SizeBytes)
	}
	return fmt.Sprintf("%-40s %-12s %-10s %-6s %-9s %s", name, state, size, pin, m.Origin, loaded)
}

// ---- pull ----

// pullMsg is what the event stream and the fallback poll feed the
// progress loop.
type pullMsg struct {
	received, total int64
	done            bool
	err             error
	streamLost      error
}

func runPull(cmd *cobra.Command, id string, noWait bool) error {
	cl, err := client()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var ds struct {
		State string `json:"state"`
	}
	if err := cl.post("/api/v1/models/"+id+"/download", nil, &ds); err != nil {
		return err
	}
	if ds.State == "ready" {
		fmt.Fprintln(out, styleOK.Render("✓"), id, "already downloaded")
		return nil
	}
	if noWait {
		fmt.Fprintln(out, styleOK.Render("✓"), "download started:", id)
		fmt.Fprintln(out, styleDim.Render("  watch it with `tera models list` or `tera logs -f`"))
		return nil
	}
	fmt.Fprintln(out, "pulling", id, "…")
	return waitForPull(cmd.Context(), cl, out, id, isTerminal(os.Stdout))
}

// waitForPull follows model_progress until the daemon reports the download
// complete (models_changed/downloaded) or failed (activity/download_failed).
// A 2s poll of the model list runs alongside so a completion that raced the
// stream open — or a stream the daemon cannot serve — still ends the wait.
func waitForPull(parent context.Context, cl *apiClient, out io.Writer, id string, tty bool) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	msgs := make(chan pullMsg, 16)
	go func() {
		err := cl.follow(ctx, false, func(ev sseEvent) error {
			if m, ok := pullMsgFromEvent(ev, id); ok {
				msgs <- m
			}
			return nil
		})
		if err != nil && ctx.Err() == nil {
			msgs <- pullMsg{streamLost: err}
		}
	}()

	bar := newProgress(out, tty)
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	idlePolls := 0
	for {
		select {
		case <-ctx.Done():
			bar.finish()
			return ctx.Err()
		case m := <-msgs:
			switch {
			case m.err != nil:
				bar.finish()
				return m.err
			case m.done:
				bar.complete()
				fmt.Fprintln(out, styleOK.Render("✓"), id, "ready")
				return nil
			case m.streamLost != nil:
				fmt.Fprintln(out, styleDim.Render("  event stream unavailable ("+m.streamLost.Error()+"); polling"))
			default:
				bar.update(m.received, m.total)
			}
		case <-poll.C:
			row, ok, err := cl.findModel(id)
			if err != nil {
				continue
			}
			switch {
			case ok && row.State == "ready":
				bar.complete()
				fmt.Fprintln(out, styleOK.Render("✓"), id, "ready")
				return nil
			case ok && row.ReceivedBytes != nil:
				idlePolls = 0
				bar.update(*row.ReceivedBytes, row.SizeBytes)
			default:
				// No progress and not ready: the daemon is between
				// states, or the download stopped and the failure event
				// was missed. Give it a few polls before giving up.
				idlePolls++
				if idlePolls >= 5 {
					bar.finish()
					return fmt.Errorf("download of %s is not running and the model is not ready — check `tera logs` (the .partial file is kept, `tera models pull` resumes)", id)
				}
			}
		}
	}
}

// pullMsgFromEvent maps the daemon's events onto the pull loop. Unrelated
// events (other models, status snapshots) return ok=false.
func pullMsgFromEvent(ev sseEvent, id string) (pullMsg, bool) {
	switch ev.Name {
	case "model_progress":
		var p struct {
			Model    string `json:"model"`
			Received int64  `json:"received_bytes"`
			Total    int64  `json:"total_bytes"`
		}
		if json.Unmarshal(ev.Data, &p) != nil || p.Model != id {
			return pullMsg{}, false
		}
		return pullMsg{received: p.Received, total: p.Total}, true
	case "models_changed":
		var c struct {
			Model  string `json:"model"`
			Change string `json:"change"`
		}
		if json.Unmarshal(ev.Data, &c) != nil || c.Model != id || c.Change != "downloaded" {
			return pullMsg{}, false
		}
		return pullMsg{done: true}, true
	case "activity":
		var a struct {
			Kind   string `json:"kind"`
			Model  string `json:"model"`
			Detail string `json:"detail"`
		}
		if json.Unmarshal(ev.Data, &a) != nil || a.Model != id || a.Kind != "download_failed" {
			return pullMsg{}, false
		}
		reason := a.Detail
		if reason == "" {
			reason = "see `tera logs`"
		}
		return pullMsg{err: fmt.Errorf("download of %s failed: %s", id, reason)}, true
	}
	return pullMsg{}, false
}

// ---- progress bar ----

// progress renders download progress: a live bar on a TTY, one line per
// ~10% (or 5s) when piped so logs stay readable.
type progress struct {
	w        io.Writer
	tty      bool
	start    time.Time
	last     time.Time
	lastB    int64
	rate     float64 // bytes/s, smoothed
	lastDraw time.Time
	lastDec  int // last decile printed when piped (-1 = none yet)
	drawn    bool
	received int64
	total    int64
}

func newProgress(w io.Writer, tty bool) *progress {
	now := time.Now()
	return &progress{w: w, tty: tty, start: now, last: now, lastDec: -1}
}

func (p *progress) update(received, total int64) {
	now := time.Now()
	if dt := now.Sub(p.last).Seconds(); dt > 0 && received >= p.lastB {
		inst := float64(received-p.lastB) / dt
		if p.rate == 0 {
			p.rate = inst
		} else {
			p.rate = 0.7*p.rate + 0.3*inst
		}
	}
	p.last, p.lastB = now, received
	p.received = received
	if total > 0 {
		p.total = total
	}
	pct := -1
	if p.total > 0 {
		pct = int(received * 100 / p.total)
	}
	if p.tty {
		if now.Sub(p.lastDraw) < 100*time.Millisecond && pct != 100 {
			return
		}
		p.lastDraw = now
		fmt.Fprint(p.w, "\r"+p.line(pct)+"\x1b[K")
		p.drawn = true
		return
	}
	// Piped: a line per 10% step, or every 5s when the total is unknown.
	if (pct >= 0 && pct/10 != p.lastDec) || (pct < 0 && now.Sub(p.lastDraw) >= 5*time.Second) {
		p.lastDraw = now
		p.lastDec = pct / 10
		fmt.Fprintln(p.w, p.line(pct))
	}
}

func (p *progress) line(pct int) string {
	var b strings.Builder
	b.WriteString("  ")
	if pct >= 0 {
		const width = 24
		filled := pct * width / 100
		b.WriteString("[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "] ")
		b.WriteString(fmt.Sprintf("%3d%%  %s / %s", pct, humanBytes(p.received), humanBytes(p.total)))
	} else {
		b.WriteString(humanBytes(p.received) + " received")
	}
	if p.rate > 0 {
		b.WriteString(fmt.Sprintf("  %s/s", humanBytes(int64(p.rate))))
		if p.total > 0 && p.received < p.total {
			eta := time.Duration(float64(p.total-p.received)/p.rate) * time.Second
			b.WriteString("  ETA " + eta.Round(time.Second).String())
		}
	}
	return b.String()
}

// complete draws the 100% state and ends the bar's line.
func (p *progress) complete() {
	if p.total > 0 {
		p.received = p.total
	}
	if p.tty {
		fmt.Fprint(p.w, "\r"+p.line(100)+"\x1b[K\n")
		return
	}
	if p.lastDec != 10 && p.total > 0 {
		fmt.Fprintln(p.w, p.line(100))
	}
}

// finish ends a partially drawn TTY line so the next print starts clean.
func (p *progress) finish() {
	if p.tty && p.drawn {
		fmt.Fprintln(p.w)
	}
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%dB", b)
}

// isTerminal reports whether f is a character device (a TTY), without
// pulling in x/term for one check.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

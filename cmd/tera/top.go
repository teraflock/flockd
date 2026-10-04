package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/teraflock/flockd/internal/localapi/gen"
)

// nowLine is the one-line "what is it doing right now" summary shown by
// `tera status` and at the top of `tera top`:
//
//	laya-q8_0: 2 decision in flight · llama-3.2-3b-instruct-q4_k_m: idle 40s · kev-4b-q4_k_m: loading
func nowLine(a gen.NodeActivity) string {
	var parts []string
	seen := map[string]bool{}
	for _, m := range a.Models {
		seen[m.Model] = true
		if m.Inflight == 0 {
			idle := int64(0)
			if m.IdleSeconds != nil {
				idle = *m.IdleSeconds
			}
			parts = append(parts, fmt.Sprintf("%s: idle %s", m.Model, shortDuration(time.Duration(idle)*time.Second)))
			continue
		}
		kinds := make([]string, 0, len(m.InflightByKind))
		for k := range m.InflightByKind {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		for i, k := range kinds {
			kinds[i] = fmt.Sprintf("%d %s", m.InflightByKind[k], k)
		}
		parts = append(parts, fmt.Sprintf("%s: %s in flight", m.Model, strings.Join(kinds, ", ")))
	}
	for _, op := range a.Operations {
		switch op.Op {
		case gen.Loading:
			since := ""
			if op.StartedAt != nil {
				since = " " + shortDuration(time.Since(*op.StartedAt))
			}
			parts = append(parts, fmt.Sprintf("%s: loading%s", op.Model, since))
		case gen.Downloading:
			pct := ""
			if op.ReceivedBytes != nil && op.TotalBytes != nil && *op.TotalBytes > 0 {
				pct = fmt.Sprintf(" %d%%", *op.ReceivedBytes*100 / *op.TotalBytes)
			}
			parts = append(parts, fmt.Sprintf("%s: downloading%s", op.Model, pct))
		}
	}
	if len(parts) == 0 {
		return "nothing loaded"
	}
	return strings.Join(parts, " · ")
}

// shortDuration renders a duration the way a glance wants it: 850ms, 12s,
// 3m05s, 2h10m.
func shortDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// renderTop draws one frame of `tera top`.
func renderTop(w io.Writer, state string, ra gen.RequestActivity) {
	fmt.Fprintln(w, styleTitle.Render("/// Teraflock activity"), styleDim.Render(ra.Now.Local().Format("15:04:05")), state)
	fmt.Fprintln(w, "  now       ", nowLine(gen.NodeActivity{Models: ra.Models, Operations: ra.Operations}))
	fmt.Fprintln(w)
	fmt.Fprintln(w, styleTitle.Render(fmt.Sprintf("IN FLIGHT (%d)", len(ra.Inflight))))
	if len(ra.Inflight) == 0 {
		fmt.Fprintln(w, styleDim.Render("  nothing running"))
	} else {
		fmt.Fprintf(w, styleDim.Render("  %-9s %-11s %-10s %9s %8s  %s")+"\n", "STARTED", "KIND", "ORIGIN", "ELAPSED", "TOKENS", "MODEL")
		for _, r := range ra.Inflight {
			fmt.Fprintf(w, "  %-9s %-11s %-10s %9s %8d  %s\n", r.StartedAt.Local().Format("15:04:05"), r.Kind, r.Origin,
				shortDuration(time.Duration(r.ElapsedMs)*time.Millisecond), r.Tokens, r.Model)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, styleTitle.Render(fmt.Sprintf("RECENT (%d)", len(ra.Recent))))
	if len(ra.Recent) == 0 {
		fmt.Fprintln(w, styleDim.Render("  no requests yet"))
		return
	}
	fmt.Fprintf(w, styleDim.Render("  %-9s %-11s %-10s %9s %8s %8s  %-13s %s")+"\n", "STARTED", "KIND", "ORIGIN", "TOOK", "IN", "OUT", "OUTCOME", "MODEL")
	for _, r := range ra.Recent {
		outcome := string(r.Outcome)
		pad := strings.Repeat(" ", max(13-len(outcome), 0))
		if r.Outcome == gen.RequestOutcomeOk {
			outcome = styleOK.Render(outcome)
		} else {
			outcome = styleWarn.Render(outcome)
		}
		fmt.Fprintf(w, "  %-9s %-11s %-10s %9s %8d %8d  %s%s %s\n", r.StartedAt.Local().Format("15:04:05"), r.Kind, r.Origin,
			shortDuration(time.Duration(r.DurationMs)*time.Millisecond), r.PromptTokens, r.CompletionTokens, outcome, pad, r.Model)
	}
}

func cmdTop() *cobra.Command {
	var (
		once     bool
		asJSON   bool
		recent   int
		interval time.Duration
	)
	c := &cobra.Command{
		Use:   "top",
		Short: "Live view of what the node is running: in-flight and recent requests per model",
		Long: `Shows what flockd is doing right now — which models have requests in
flight (kind, origin, elapsed, tokens so far), runtimes starting and
downloads — and the most recent finished requests, refreshed every second.

Origins: local = this machine's local API, mesh = a coordinator dispatch
(customer request or canary), challenge = a fingerprint challenge.
No request content is ever shown or kept.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			frame := func(ctx context.Context, redraw bool) error {
				ra, err := cl.Requests(ctx, recent)
				if err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(out).Encode(ra)
				}
				state := ""
				if st, err := cl.Status(ctx); err == nil {
					state = styleDim.Render("· " + st.State)
				}
				var b strings.Builder
				if redraw {
					b.WriteString("\x1b[H\x1b[2J") // cursor home + clear screen
				}
				renderTop(&b, state, ra)
				_, err = io.WriteString(out, b.String())
				return err
			}
			if once {
				return frame(cmd.Context(), false)
			}
			// Redraw in place on a terminal; append frames when piped.
			redraw := false
			if f, ok := out.(*os.File); ok {
				if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
					redraw = !asJSON
				}
			}
			if interval < 200*time.Millisecond {
				interval = 200 * time.Millisecond
			}
			t := time.NewTicker(interval)
			defer t.Stop()
			for {
				if err := frame(cmd.Context(), redraw); err != nil {
					return err
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-t.C:
				}
			}
		},
	}
	c.Flags().BoolVar(&once, "once", false, "print one snapshot and exit")
	c.Flags().BoolVar(&asJSON, "json", false, "print the raw RequestActivity JSON (one object per refresh)")
	c.Flags().IntVarP(&recent, "recent", "n", 20, "finished requests to show (max 200)")
	c.Flags().DurationVar(&interval, "interval", time.Second, "refresh interval")
	return c
}

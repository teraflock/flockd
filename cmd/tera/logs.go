package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/gen"
)

// cmdLogs reads the daemon's log ring (GET /api/v1/logs) and, with -f,
// follows the SSE `log` events on /api/v1/events?logs=1 (flockd#39). Same
// source the web dash and desktop app read — not the service manager's
// log file, which `tera up` tails on failure.
func cmdLogs() *cobra.Command {
	var (
		n      int
		follow bool
		level  string
		asJSON bool
	)
	c := &cobra.Command{
		Use:   "logs",
		Short: "Show recent daemon log lines (-f to follow)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			minRank, ok := levelRank(level)
			if level != "" && !ok {
				return fmt.Errorf("unknown --level %q (debug|info|warn|error)", level)
			}
			out := cmd.OutOrStdout()
			logs, err := cl.Logs(cmd.Context(), n)
			if err != nil {
				return err
			}
			for _, e := range logs {
				printLog(out, e, minRank, asJSON)
			}
			if !follow {
				return nil
			}
			return cl.Follow(cmd.Context(), true, func(ev client.Event) error {
				if ev.Name != "log" {
					return nil
				}
				var e gen.LogEntry
				if json.Unmarshal(ev.Data, &e) != nil {
					return nil
				}
				printLog(out, e, minRank, asJSON)
				return nil
			})
		},
	}
	c.Flags().IntVarP(&n, "lines", "n", 100, "number of recent lines to show (max 1024)")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines as the daemon logs them")
	c.Flags().StringVar(&level, "level", "", "minimum level to show: debug|info|warn|error")
	c.Flags().BoolVar(&asJSON, "json", false, "one JSON object per line instead of text")
	return c
}

// levelRank orders slog levels for --level filtering. Unknown strings
// (a custom level, "") rank as info so nothing is silently hidden.
func levelRank(level string) (int, bool) {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "", "INFO":
		return 1, level == "" || true
	case "DEBUG":
		return 0, true
	case "WARN", "WARNING":
		return 2, true
	case "ERROR":
		return 3, true
	}
	return 1, false
}

func printLog(w io.Writer, e gen.LogEntry, minRank int, asJSON bool) {
	if r, _ := levelRank(e.Level); r < minRank {
		return
	}
	if asJSON {
		// Keyed in reading order (time first), not the generated struct's
		// alphabetical one — this is a line format people grep.
		line := struct {
			Time    time.Time `json:"time"`
			Level   string    `json:"level"`
			Message string    `json:"message"`
			Attrs   *string   `json:"attrs,omitempty"`
		}{e.Time, e.Level, e.Message, e.Attrs}
		if raw, err := json.Marshal(line); err == nil {
			fmt.Fprintln(w, string(raw))
		}
		return
	}
	fmt.Fprintln(w, formatLog(e, 0))
}

// formatLog renders `time LEVEL message key=val…` in the dash's format.
// width > 0 truncates for a TUI pane (rune-aware, keeps the level colour).
func formatLog(e gen.LogEntry, width int) string {
	msg := e.Message
	if e.Attrs != nil && *e.Attrs != "" {
		msg += " " + *e.Attrs
	}
	ts := e.Time.Local().Format("15:04:05")
	lvl := fmt.Sprintf("%-5s", strings.ToUpper(e.Level))
	if width > 0 {
		// time (8) + space + level (5) + space = 15 columns before the message.
		if room := width - 15; room > 3 {
			if r := []rune(msg); len(r) > room {
				msg = string(r[:room-1]) + "…"
			}
		}
	}
	return styleDim.Render(ts) + " " + levelStyle(e.Level).Render(lvl) + " " + msg
}

func levelStyle(level string) lipgloss.Style {
	switch r, _ := levelRank(level); r {
	case 0:
		return styleDim
	case 2, 3:
		return styleWarn
	}
	return lipgloss.NewStyle()
}

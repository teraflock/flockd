package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/gen"
)

// cmdDashboard is the full-screen Bubbletea TUI: live tok/s sparkline,
// requests, earnings ticker, model slots — the marketing screenshot
// (SPEC §4.2).
func cmdDashboard() *cobra.Command {
	var web bool
	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Live node dashboard (TUI); --web opens the browser dashboard",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if web {
				fmt.Println("Opening", flagAPI, "…")
				raw, err := os.ReadFile(filepath.Join(dataDir(), client.TokenFile))
				if err == nil {
					fmt.Println(styleDim.Render("  bearer token (paste into the page): " + strings.TrimSpace(string(raw))))
				}
				openOrPrint(cmd, flagAPI)
				return nil
			}
			cl, err := newClient()
			if err != nil {
				return err
			}
			p := tea.NewProgram(newDashModel(cl), tea.WithAltScreen())
			_, err = p.Run()
			return err
		},
	}
	c.Flags().BoolVar(&web, "web", false, "open the embedded web dashboard instead")
	return c
}

// ---- bubbletea model ----

// The dashboard is live over GET /api/v1/events (flockd#38): every `status`
// snapshot the daemon emits (2s cadence, immediately on governor
// transitions) lands as an eventMsg, `models_changed` / `model_progress`
// refetch the model list, and `log` lines feed the logs pane. What the
// stream does not carry — earnings, the model list — is polled every
// pollEvery; the same poll is the belt-and-braces fallback while the
// stream is down, with a capped reconnect backoff. Nothing here reads a
// field that is not in api/openapi.yaml.

type tickMsg time.Time

// dataMsg is a fallback poll / initial fetch result.
type dataMsg struct {
	status   gen.Status
	earnings gen.Earnings
	models   gen.ModelList
	logs     []gen.LogEntry
	withLogs bool
	err      error
}

// modelsMsg is a model-list refetch triggered by a models_changed /
// model_progress event.
type modelsMsg struct {
	models gen.ModelList
	err    error
}

// streamMsg is the outcome of opening the event stream.
type streamMsg struct {
	s   *client.Stream
	err error
}

// eventMsg is one event off the stream.
type eventMsg struct{ ev client.Event }

// streamEndMsg says the stream closed (err nil = daemon ended it).
type streamEndMsg struct{ err error }

// reconnectMsg fires after the backoff to reopen the stream.
type reconnectMsg struct{}

// logPaneLines is how many ring entries the TUI keeps for scrolling.
const logPaneLines = 200

const (
	pollEvery      = 10 * time.Second
	backoffStart   = time.Second
	backoffMax     = 30 * time.Second
	modelsRefetchT = time.Second // model_progress arrives ~4×/s; refetch at most this often
)

type dashModel struct {
	cl      *client.Client
	width   int
	height  int
	spark   []float64
	status  gen.Status
	earn    gen.Earnings
	models  gen.ModelList
	lastErr error
	haveOne bool

	// Event stream state.
	ctx        context.Context
	cancel     context.CancelFunc
	stream     *client.Stream
	connected  bool
	streamErr  error
	backoff    time.Duration
	retryAt    time.Time
	ticks      int
	lastModels time.Time
	now        time.Time

	// Logs pane (flockd#39): toggled with `l`; logScroll counts lines
	// scrolled back from the newest (0 = following).
	showLogs  bool
	logs      []gen.LogEntry
	logScroll int
}

func newDashModel(cl *client.Client) *dashModel {
	ctx, cancel := context.WithCancel(context.Background())
	return &dashModel{cl: cl, spark: make([]float64, 0, 64), ctx: ctx, cancel: cancel, backoff: backoffStart, now: time.Now()}
}

// fetchCmd is the poll: status (only needed while the stream is down, but
// cheap and keeps the fallback honest), earnings and models (never on the
// stream), and the log ring when the stream is not feeding the pane.
func (m *dashModel) fetchCmd() tea.Cmd {
	withLogs := m.showLogs && !m.connected
	ctx := m.ctx
	return func() tea.Msg {
		var d dataMsg
		var err error
		if d.status, err = m.cl.Status(ctx); err != nil {
			d.err = err
			return d
		}
		d.earnings, _ = m.cl.Earnings(ctx)
		d.models, _ = m.cl.Models(ctx)
		if withLogs {
			if logs, err := m.cl.Logs(ctx, logPaneLines); err == nil {
				d.logs, d.withLogs = logs, true
			}
		}
		return d
	}
}

func (m *dashModel) modelsCmd() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		ml, err := m.cl.Models(ctx)
		return modelsMsg{ml, err}
	}
}

// connectCmd opens the event stream (with log lines, so the pane is live
// whenever it is shown).
func (m *dashModel) connectCmd() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		s, err := m.cl.Events(ctx, true)
		return streamMsg{s, err}
	}
}

// readCmd waits for the next event; re-issued after each one.
func readCmd(s *client.Stream) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-s.C
		if !ok {
			return streamEndMsg{s.Err()}
		}
		return eventMsg{ev}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// scheduleReconnect arms the next connect attempt and doubles the wait,
// 1s -> 30s.
func (m *dashModel) scheduleReconnect() tea.Cmd {
	m.connected, m.stream = false, nil
	wait := m.backoff
	m.retryAt = m.now.Add(wait)
	m.backoff = min(m.backoff*2, backoffMax)
	return tea.Tick(wait, func(time.Time) tea.Msg { return reconnectMsg{} })
}

func (m *dashModel) Init() tea.Cmd {
	return tea.Batch(m.connectCmd(), m.fetchCmd(), tick())
}

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			m.cancel()
			return m, tea.Quit
		case "l":
			m.showLogs = !m.showLogs
			m.logScroll = 0
			if m.showLogs && !m.connected {
				return m, m.fetchCmd()
			}
		case "up", "k":
			if m.showLogs {
				m.scrollLogs(1)
			}
		case "down", "j":
			if m.showLogs {
				m.scrollLogs(-1)
			}
		case "pgup":
			if m.showLogs {
				m.scrollLogs(m.logPaneHeight())
			}
		case "pgdown":
			if m.showLogs {
				m.scrollLogs(-m.logPaneHeight())
			}
		case "end", "G":
			m.logScroll = 0
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		// The clock redraw; every pollEvery it is also the fallback poll
		// (while connected the poll only refreshes earnings and models).
		m.now = time.Time(msg)
		m.ticks++
		if m.ticks%int(pollEvery/time.Second) == 0 {
			return m, tea.Batch(m.fetchCmd(), tick())
		}
		return m, tick()
	case streamMsg:
		if msg.err != nil {
			m.streamErr = msg.err
			return m, m.scheduleReconnect()
		}
		m.stream, m.connected, m.streamErr = msg.s, true, nil
		m.backoff = backoffStart
		if m.showLogs {
			// The stream carries new lines only; seed the pane with history.
			return m, tea.Batch(readCmd(m.stream), m.logsCmd())
		}
		return m, readCmd(m.stream)
	case streamEndMsg:
		if m.ctx.Err() != nil {
			return m, nil
		}
		m.streamErr = msg.err
		if m.streamErr == nil {
			m.streamErr = fmt.Errorf("daemon closed the event stream")
		}
		return m, m.scheduleReconnect()
	case reconnectMsg:
		return m, m.connectCmd()
	case eventMsg:
		if m.stream == nil {
			return m, nil
		}
		cmd := m.applyEvent(msg.ev)
		return m, tea.Batch(readCmd(m.stream), cmd)
	case modelsMsg:
		if msg.err == nil {
			m.models = msg.models
		}
	case logsMsg:
		if msg.err == nil {
			m.logs = msg.logs
		}
	case dataMsg:
		if msg.err != nil {
			m.lastErr = msg.err
			return m, nil
		}
		m.lastErr = nil
		m.haveOne = true
		m.status = msg.status
		m.earn = msg.earnings
		m.models = msg.models
		if msg.withLogs {
			m.logs = msg.logs
		}
		if !m.connected {
			// Polling is the only source of samples while the stream is down.
			m.pushSample(msg.status.Stats.TokensPerSec1m)
		}
	}
	return m, nil
}

// logsMsg is the log-ring history fetched when the pane opens on a live
// stream (the stream carries only new lines).
type logsMsg struct {
	logs []gen.LogEntry
	err  error
}

func (m *dashModel) logsCmd() tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		logs, err := m.cl.Logs(ctx, logPaneLines)
		return logsMsg{logs, err}
	}
}

// applyEvent folds one stream event into the model; the returned cmd is a
// follow-up fetch when the event only says "something changed".
func (m *dashModel) applyEvent(ev client.Event) tea.Cmd {
	switch ev.Name {
	case "status":
		var st gen.Status
		if json.Unmarshal(ev.Data, &st) != nil {
			return nil
		}
		m.status, m.haveOne, m.lastErr = st, true, nil
		m.pushSample(st.Stats.TokensPerSec1m)
	case "models_changed":
		m.lastModels = m.now
		return m.modelsCmd()
	case "model_progress":
		// Throttled: the list refetch is for the progress column, not
		// every 250ms frame.
		if m.now.Sub(m.lastModels) < modelsRefetchT {
			return nil
		}
		m.lastModels = m.now
		return m.modelsCmd()
	case "log":
		var e gen.LogEntry
		if json.Unmarshal(ev.Data, &e) != nil {
			return nil
		}
		m.logs = append(m.logs, e)
		if len(m.logs) > logPaneLines {
			m.logs = m.logs[len(m.logs)-logPaneLines:]
		}
		if m.logScroll > 0 {
			m.logScroll++ // keep the scrolled-back view still
		}
	}
	return nil
}

// pushSample appends to the 60-sample sparkline window.
func (m *dashModel) pushSample(v float64) {
	m.spark = append(m.spark, v)
	if len(m.spark) > 60 {
		m.spark = m.spark[1:]
	}
}

// scrollLogs moves the pane by n lines (positive = older), clamped.
func (m *dashModel) scrollLogs(n int) {
	maxBack := len(m.logs) - m.logPaneHeight()
	if maxBack < 0 {
		maxBack = 0
	}
	m.logScroll += n
	if m.logScroll > maxBack {
		m.logScroll = maxBack
	}
	if m.logScroll < 0 {
		m.logScroll = 0
	}
}

// logPaneHeight is the number of log lines the pane shows: whatever the
// terminal has left under the panels, between 5 and 20.
func (m *dashModel) logPaneHeight() int {
	// header + two panel rows (5 rows each incl. borders) + models panel
	// + update line + footer ≈ 20 rows on a fresh dash; the models panel
	// grows with the model count.
	used := 20 + len(m.models.Models)
	h := m.height - used - 2 // pane border
	if h < 5 {
		h = 5
	}
	if h > 20 {
		h = 20
	}
	return h
}

// logsPane renders the last lines of the ring, offset by logScroll.
func (m *dashModel) logsPane() string {
	h := m.logPaneHeight()
	title := dashHeader.Render("LOGS")
	if m.logScroll > 0 {
		title += dashLabel.Render(fmt.Sprintf("  ↑ %d lines back · End to follow", m.logScroll))
	} else {
		title += dashLabel.Render("  following · ↑/↓ PgUp/PgDn scroll")
	}
	end := len(m.logs) - m.logScroll
	if end < 0 {
		end = 0
	}
	start := end - h
	if start < 0 {
		start = 0
	}
	lines := make([]string, 0, h+1)
	lines = append(lines, title)
	if len(m.logs) == 0 {
		lines = append(lines, dashLabel.Render("nothing logged yet"))
	}
	for _, e := range m.logs[start:end] {
		lines = append(lines, formatLog(e, 84))
	}
	return dashPanel.Width(88).Render(strings.Join(lines, "\n"))
}

var (
	dashPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#39445a")).
			Padding(0, 1)
	// Brand palette v1 (website/docs/brand.md); lipgloss degrades hex to
	// the nearest ANSI color on non-truecolor terminals. Warnings are
	// orange — gold is reserved for the brand accent (dashHeader).
	dashHeader = lipgloss.NewStyle().Foreground(lipgloss.Color("#f5b60d")).Bold(true)
	dashLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("#8b95a9"))
	dashGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("#34d399"))
	dashAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("#fb923c"))
	dashBig    = lipgloss.NewStyle().Foreground(lipgloss.Color("#e6ebf4")).Bold(true)
)

var sparkBars = []rune("▁▂▃▄▅▆▇█")

func sparkline(vals []float64, width int) string {
	if len(vals) == 0 {
		return strings.Repeat(" ", width)
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	maxV := 0.0
	for _, v := range vals {
		if v > maxV {
			maxV = v
		}
	}
	if maxV == 0 {
		maxV = 1
	}
	var b strings.Builder
	for _, v := range vals {
		idx := int(v / maxV * float64(len(sparkBars)-1))
		b.WriteRune(sparkBars[idx])
	}
	return b.String()
}

func (m *dashModel) View() string {
	if !m.haveOne {
		if m.lastErr != nil {
			return dashPanel.Render(dashAmber.Render("cannot reach flockd") + "\n" + m.lastErr.Error() + "\n\n" + dashLabel.Render("start it with `tera up` or `flockd --standalone`  ·  q to quit"))
		}
		return dashLabel.Render("connecting to flockd …")
	}
	st := m.status

	stateStr := dashAmber.Render(strings.ToUpper(st.State))
	if st.State == "serving" {
		stateStr = dashGreen.Render("SERVING")
	}
	mode := ""
	if st.Standalone {
		mode = dashLabel.Render(" · standalone")
	}
	header := dashHeader.Render("/// TERAFLOCK") + "  " +
		stateStr + mode + "  " +
		dashLabel.Render(fmt.Sprintf("node %s · v%s · up %s", short(st.NodeId), st.Version, (time.Duration(st.UptimeSeconds)*time.Second).String()))

	// Throughput panel.
	tp := fmt.Sprintf("%s %s\n%s\n%s",
		dashBig.Render(fmt.Sprintf("%6.1f", st.Stats.TokensPerSec1m)),
		dashLabel.Render("tok/s (1m)"),
		dashGreen.Render(sparkline(m.spark, 40)),
		dashLabel.Render(fmt.Sprintf("in-flight %d · reqs %d · req/min %.0f",
			st.Inflight, st.Stats.TotalRequests, st.Stats.RequestsPerMin)))
	throughput := dashPanel.Width(46).Render(dashHeader.Render("THROUGHPUT") + "\n" + tp)

	// Earnings ticker panel.
	ep := fmt.Sprintf("%s %s\n%s\n%s",
		dashBig.Render(fmt.Sprintf("$%.6f", m.earn.EstUsd)),
		dashLabel.Render("earned"),
		dashLabel.Render(fmt.Sprintf("%.4f credits · est $%.4f/day", m.earn.EarnedCredits, m.earn.EstUsdPerDay)),
		dashLabel.Render(fmt.Sprintf("lifetime tokens %d", m.earn.LifetimeTokens)))
	earnings := dashPanel.Width(40).Render(dashHeader.Render("EARNINGS") + "\n" + ep)

	// Hardware panel.
	hw := "unknown"
	if st.Hardware != nil {
		var gpus []string
		for _, g := range st.Hardware.Gpus {
			gpus = append(gpus, fmt.Sprintf("%s · %s · %dGB", g.Model, g.Accel, g.VramMb/1024))
		}
		power := "⚡ AC"
		if st.OnBattery {
			power = dashAmber.Render("🔋 battery")
		}
		temp := ""
		if st.TempCelsius > 0 {
			temp = fmt.Sprintf(" · %.0f°C", st.TempCelsius)
		}
		accel := ""
		if st.RuntimeAccel != nil && *st.RuntimeAccel != "" {
			accel = " · runtime " + *st.RuntimeAccel
		}
		hw = fmt.Sprintf("%s/%s · %d cores · %dGB RAM\n%s\n%s%s%s",
			st.Hardware.Os, st.Hardware.Arch, st.Hardware.CpuCores, st.Hardware.RamMb/1024,
			strings.Join(gpus, "\n"), power, temp, accel)
	}
	if st.Disk.Dir != "" {
		hw += fmt.Sprintf("\ndisk %s models · %s free", gb(st.Disk.ModelsBytes), gb(st.Disk.FreeBytes))
	}
	hardware := dashPanel.Width(46).Render(dashHeader.Render("HARDWARE") + "\n" + hw)

	mesh := dashPanel.Width(40).Render(dashHeader.Render("MESH") + "\n" + m.meshPanel(time.Now()))

	// Models panel.
	var rows []string
	for _, mm := range m.models.Models {
		mark := "○"
		if mm.Loaded {
			mark = dashGreen.Render("●")
		}
		pin := ""
		if mm.Pinned {
			pin = " 📌"
		}
		def := ""
		if mm.Default {
			def = dashLabel.Render(" (default)")
		}
		mem := ""
		if mm.LoadedMb != nil {
			mem = fmt.Sprintf(" %.1fGB", float64(*mm.LoadedMb)/1024)
		}
		state := mm.State
		if state == "missing" {
			state = dashAmber.Render(state)
		} else {
			state = dashLabel.Render(state)
		}
		rows = append(rows, fmt.Sprintf("%s %s%s%s  %s%s", mark, mm.Id, def, pin, state, dashLabel.Render(mem)))
	}
	if len(rows) == 0 {
		rows = []string{dashLabel.Render("no models")}
	}
	modelsPanel := dashPanel.Width(88).Render(dashHeader.Render("MODEL SLOTS") + "\n" + strings.Join(rows, "\n"))

	// Help line. The TUI is a read-only view today; writes live in the
	// CLI (flockd#41 decides whether p/u keys join `l`).
	footer := dashLabel.Render("q quit · l logs  |  read-only view — change things with tera limits / tera models  ·  live")
	switch {
	case m.lastErr != nil:
		footer = dashAmber.Render("connection lost: " + m.lastErr.Error())
	case !m.connected:
		footer = dashAmber.Render(m.reconnectLine())
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top, throughput, earnings)
	mid := lipgloss.JoinHorizontal(lipgloss.Top, hardware, mesh)
	parts := []string{header, top, mid, modelsPanel}
	if line := updateLine(st.Update); line != "" {
		parts = append(parts, dashAmber.Render("⬆ "+line))
	}
	if m.showLogs {
		parts = append(parts, m.logsPane())
	}
	parts = append(parts, footer)
	return strings.Join(parts, "\n")
}

// reconnectLine is the footer while the event stream is down: the reason,
// when the next attempt is, and that the 10s poll is still feeding the
// screen.
func (m *dashModel) reconnectLine() string {
	wait := m.retryAt.Sub(m.now).Round(time.Second)
	if wait < 0 {
		wait = 0
	}
	reason := ""
	if m.streamErr != nil {
		reason = " (" + firstLine(m.streamErr.Error()) + ")"
	}
	return fmt.Sprintf("event stream down%s — reconnecting in %s · polling every %s", reason, wait, pollEvery)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// meshPanel is what the daemon actually knows about its place in the
// mesh — enrollment, certificate, version/update state, model memory —
// from /api/v1/status. It replaced a hardcoded "probation" placeholder
// (flockd#40): nothing on the wire tells a node its reputation yet, and
// the panel must never fake or infer one (the proto ask is on docs#12).
func (m *dashModel) meshPanel(now time.Time) string {
	st := m.status
	var lines []string
	switch {
	case st.Standalone:
		lines = append(lines, dashAmber.Render("standalone")+dashLabel.Render(" · in-process fake coordinator"))
	case st.Enrolled:
		lines = append(lines, dashGreen.Render("enrolled")+dashLabel.Render(" · node "+short(st.NodeId)))
	default:
		lines = append(lines, dashAmber.Render("not enrolled")+dashLabel.Render(" · run tera login"))
	}
	if st.Enrolled && st.CertExpiresAt != nil {
		left := st.CertExpiresAt.Sub(now)
		cert := fmt.Sprintf("cert expires in %s", humanDays(left))
		if left < 7*24*time.Hour {
			cert = dashAmber.Render(cert + " · rotation due")
		} else {
			cert = dashLabel.Render(cert)
		}
		lines = append(lines, cert)
	}
	switch u := st.Update; {
	case u == nil:
		lines = append(lines, dashLabel.Render("v"+st.Version+" · version check pending"))
	case u.BelowMinimum != nil && *u.BelowMinimum:
		floor := ""
		if u.Minimum != nil {
			floor = *u.Minimum
		}
		lines = append(lines, dashAmber.Render("v"+st.Version+" below mesh minimum "+floor+" · drained until updated"))
	case u.Available:
		lines = append(lines, dashAmber.Render("v"+st.Version+" · "+u.Latest+" available"))
	default:
		lines = append(lines, dashLabel.Render("v"+st.Version+" · up to date"))
	}
	if st.Memory.BudgetMb > 0 {
		lines = append(lines, dashLabel.Render(fmt.Sprintf("models %.1f / %.1fGB memory budget",
			float64(st.Memory.UsedMb)/1024, float64(st.Memory.BudgetMb)/1024)))
	}
	return strings.Join(lines, "\n")
}

// humanDays renders a duration as days (or hours under a day).
func humanDays(d time.Duration) string {
	if d < 0 {
		return "0h (expired)"
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
)

// cmdDashboard is the full-screen Bubbletea TUI: live tok/s sparkline,
// requests, earnings ticker, model slots — the marketing screenshot
// (SPEC §4.2).
func cmdDashboard() *cobra.Command {
	var web bool
	c := &cobra.Command{
		Use:   "dashboard",
		Short: "Live node dashboard (TUI); --web opens the browser dashboard",
		RunE: func(cmd *cobra.Command, args []string) error {
			if web {
				fmt.Println("Opening", flagAPI, "…")
				raw, err := os.ReadFile(filepath.Join(dataDir(), "local_api_token"))
				if err == nil {
					fmt.Println(styleDim.Render("  bearer token (paste into the page): " + strings.TrimSpace(string(raw))))
				}
				return openInBrowser(flagAPI)
			}
			cl, err := client()
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

type tickMsg time.Time

type dataMsg struct {
	status   statusResp
	earnings earningsResp
	models   modelsResp
	err      error
}

type dashModel struct {
	cl      *apiClient
	width   int
	height  int
	spark   []float64
	status  statusResp
	earn    earningsResp
	models  modelsResp
	lastErr error
	haveOne bool
}

func newDashModel(cl *apiClient) *dashModel {
	return &dashModel{cl: cl, spark: make([]float64, 0, 64)}
}

func (m *dashModel) fetch() tea.Msg {
	var d dataMsg
	if err := m.cl.get("/api/v1/status", &d.status); err != nil {
		d.err = err
		return d
	}
	_ = m.cl.get("/api/v1/earnings", &d.earnings)
	_ = m.cl.get("/api/v1/models", &d.models)
	return d
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *dashModel) Init() tea.Cmd {
	return tea.Batch(m.fetch, tick())
}

func (m *dashModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(m.fetch, tick())
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
		m.spark = append(m.spark, msg.status.Stats.TokensPerSec1m)
		if len(m.spark) > 60 {
			m.spark = m.spark[1:]
		}
	}
	return m, nil
}

var (
	dashPanel = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("240")).
			Padding(0, 1)
	dashHeader = lipgloss.NewStyle().Foreground(lipgloss.Color("220")).Bold(true)
	dashLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	dashGreen  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	dashAmber  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	dashBig    = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
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
			return dashPanel.Render(dashAmber.Render("cannot reach flockd") + "\n" + m.lastErr.Error() + "\n\n" + dashLabel.Render("start it with `flock up` or `flockd --standalone`  ·  q to quit"))
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
	header := dashHeader.Render("⬡ TERAFLOCK") + "  " +
		stateStr + mode + "  " +
		dashLabel.Render(fmt.Sprintf("node %s · v%s · up %s", short(st.NodeID), st.Version, (time.Duration(st.UptimeSeconds)*time.Second).String()))

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
		dashBig.Render(fmt.Sprintf("$%.6f", m.earn.EstUSD)),
		dashLabel.Render("earned"),
		dashLabel.Render(fmt.Sprintf("%.4f credits · est $%.4f/day", m.earn.EarnedCredits, m.earn.EstUSDPerDay)),
		dashLabel.Render(fmt.Sprintf("lifetime tokens %d", m.earn.LifetimeTokens)))
	earnings := dashPanel.Width(40).Render(dashHeader.Render("EARNINGS") + "\n" + ep)

	// Hardware panel.
	hw := "unknown"
	if st.Hardware != nil {
		var gpus []string
		for _, g := range st.Hardware.GPUs {
			gpus = append(gpus, fmt.Sprintf("%s · %s · %dGB", g.Model, g.Accel, g.VRAMMB/1024))
		}
		power := "⚡ AC"
		if st.OnBattery {
			power = dashAmber.Render("🔋 battery")
		}
		temp := ""
		if st.TempCelsius > 0 {
			temp = fmt.Sprintf(" · %.0f°C", st.TempCelsius)
		}
		hw = fmt.Sprintf("%s/%s · %d cores · %dGB RAM\n%s\n%s%s",
			st.Hardware.OS, st.Hardware.Arch, st.Hardware.CPUCores, st.Hardware.RAMMB/1024,
			strings.Join(gpus, "\n"), power, temp)
	}
	hardware := dashPanel.Width(46).Render(dashHeader.Render("HARDWARE") + "\n" + hw)

	// Reputation placeholder until the trust engine exists (Phase 3).
	rep := dashPanel.Width(40).Render(dashHeader.Render("REPUTATION") + "\n" +
		dashBig.Render("—") + " " + dashLabel.Render("probation") + "\n" +
		dashLabel.Render("reputation starts accruing once\nenrolled with a live coordinator"))

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
		rows = append(rows, fmt.Sprintf("%s %s%s%s  %s", mark, mm.ID, def, pin, dashLabel.Render(mm.State)))
	}
	if len(rows) == 0 {
		rows = []string{dashLabel.Render("no models")}
	}
	modelsPanel := dashPanel.Width(88).Render(dashHeader.Render("MODEL SLOTS") + "\n" + strings.Join(rows, "\n"))

	footer := dashLabel.Render("q quit · polls " + flagAPI + "/api/v1 every 1s")
	if m.lastErr != nil {
		footer = dashAmber.Render("connection lost: " + m.lastErr.Error())
	}

	top := lipgloss.JoinHorizontal(lipgloss.Top, throughput, earnings)
	mid := lipgloss.JoinHorizontal(lipgloss.Top, hardware, rep)
	return strings.Join([]string{header, top, mid, modelsPanel, footer}, "\n")
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

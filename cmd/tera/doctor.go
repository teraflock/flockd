package main

// `tera doctor`: a fixed list of read-only checks that says, in the order
// an operator would debug it, why a node is or is not serving. The command
// changes nothing on the machine: it reads files, asks the service manager
// and the local API for their state, fetches the runtime manifest and
// opens one TLS connection to the coordinator without presenting the
// node's certificate.
//
// The pieces: this file holds the result type, the runner (one goroutine
// per check, each with its own timeout and panic guard) and the two
// renderers; doctor_checks.go holds the checks, written against the small
// interfaces of doctorEnv so they run under test with fakes;
// doctor_probe.go is the production side of those interfaces.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
)

// doctorStatus is the outcome of one check.
type doctorStatus string

const (
	doctorPass doctorStatus = "pass"
	doctorWarn doctorStatus = "warn"
	doctorFail doctorStatus = "fail"
	doctorSkip doctorStatus = "skip"
)

// doctorResult is one line of `tera doctor`. The JSON shape (--json prints
// an array of these, in check order) is a stable contract: fields are only
// ever added. Detail may hold several lines separated by "\n"; Remedy is
// empty unless Status is warn or fail.
type doctorResult struct {
	ID     string       `json:"id"`
	Title  string       `json:"title"`
	Status doctorStatus `json:"status"`
	Detail string       `json:"detail"`
	Remedy string       `json:"remedy"`
}

// doctorCheck is one entry of the fixed list. run fills Status, Detail and
// Remedy; the runner owns ID and Title.
type doctorCheck struct {
	id, title string
	timeout   time.Duration
	run       func(ctx context.Context, env *doctorEnv) doctorResult
}

// doctorChecks is the list, in the order an operator would debug a node
// that is not serving: is it installed, is it started, does it answer, is
// it on the mesh, can it run a model, does it have one, is it current.
func doctorChecks() []doctorCheck {
	return []doctorCheck{
		{"install", "Install", 8 * time.Second, checkInstall},
		{"service", "Service", 16 * time.Second, checkService},
		{"daemon", "Daemon", 16 * time.Second, checkDaemon},
		{"enrollment", "Enrollment", 12 * time.Second, checkEnrollment},
		{"mesh", "Mesh", 16 * time.Second, checkMesh},
		{"runtime", "Hardware and runtime", 18 * time.Second, checkRuntime},
		{"models", "Models", 14 * time.Second, checkModels},
		{"update", "Update", 12 * time.Second, checkUpdate},
	}
}

// doctorGrace is how long past its timeout a check may take to notice its
// cancelled context before the runner stops waiting for it.
const doctorGrace = time.Second

// doctorIssuesURL is where a crash of the doctor itself is reported.
const doctorIssuesURL = "https://github.com/teraflock/flockd/issues"

// errDoctorFailed makes `tera doctor` exit 1 without an "error:" line: the
// report on stdout already says what failed.
var errDoctorFailed = errors.New("doctor: at least one check failed")

func cmdDoctor() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check why this node is or is not serving (read-only; changes nothing)",
		Long: `Run a fixed list of checks and print one line for each: pass, warning or
fail, with a plain explanation and, when something is wrong, the command
that fixes it. In order: the install, the service, the daemon, enrollment,
the connection to the mesh, hardware and runtime, models, and updates.

tera doctor only reads. It installs nothing, restarts nothing and writes no
configuration; the network traffic is the runtime manifest and one TLS
handshake with the coordinator, without the node's certificate.

Exit status is 0 when no check failed (warnings allowed) and 1 otherwise.
--json prints the same results as a stable array of
{id, title, status, detail, remedy} with status pass, warn, fail or skip.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			return doctorReport(cmd.Context(), newDoctorEnv(cmd), doctorChecks(), out, asJSON, doctorLookFor(out))
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print the results as a JSON array instead of text")
	return c
}

// doctorReport runs the checks, prints the report in the asked form and
// returns errDoctorFailed when a check failed.
func doctorReport(ctx context.Context, env *doctorEnv, checks []doctorCheck, out io.Writer, asJSON bool, look doctorLook) error {
	results := runDoctor(ctx, env, checks)
	if asJSON {
		if err := renderDoctorJSON(out, results); err != nil {
			return err
		}
	} else {
		renderDoctor(out, env.headline(), results, look)
	}
	if doctorFailed(results) {
		return errDoctorFailed
	}
	return nil
}

// doctorLookFor picks the text form for a writer: styled and wrapped to
// the window on a terminal, plain anywhere else.
func doctorLookFor(out io.Writer) doctorLook {
	f, isFile := out.(*os.File)
	if !isFile || !isTerminal(f) {
		return doctorLook{}
	}
	look := doctorLook{color: true}
	if w, _, err := term.GetSize(f.Fd()); err == nil {
		look.width = w
	}
	return look
}

// runDoctor runs every check at once (each has its own timeout, so the
// whole command takes as long as the slowest one, not their sum) and
// returns the results in list order.
func runDoctor(ctx context.Context, env *doctorEnv, checks []doctorCheck) []doctorResult {
	if ctx == nil {
		ctx = context.Background()
	}
	env.ctx = ctx
	results := make([]doctorResult, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runDoctorCheck(ctx, env, c)
		}()
	}
	wg.Wait()
	return results
}

// runDoctorCheck runs one check under its timeout. A check that panics or
// never returns becomes a warning on its own line and leaves the others
// alone: "could not tell" is not the same as "broken", so neither turns
// the exit status to 1.
func runDoctorCheck(ctx context.Context, env *doctorEnv, c doctorCheck) doctorResult {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	done := make(chan doctorResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- doctorResult{
					Status: doctorWarn,
					Detail: fmt.Sprintf("This check crashed (%v), so its answer is unknown. The other checks are not affected.", r),
					Remedy: "This is a bug in tera doctor: please report it at " + doctorIssuesURL + ".",
				}
			}
		}()
		done <- c.run(cctx, env)
	}()
	limit := time.NewTimer(c.timeout + doctorGrace)
	defer limit.Stop()
	var res doctorResult
	select {
	case res = <-done:
	case <-limit.C:
		res = doctorResult{
			Status: doctorWarn,
			Detail: fmt.Sprintf("This check did not finish within %s, so its answer is unknown.", c.timeout),
			Remedy: "Run " + env.cmd("doctor") + " again.",
		}
	}
	return finishDoctorResult(c, res)
}

// finishDoctorResult stamps the identity on a result and holds it to the
// output contract whatever the check returned.
func finishDoctorResult(c doctorCheck, res doctorResult) doctorResult {
	res.ID, res.Title = c.id, c.title
	res.Detail = strings.TrimSpace(res.Detail)
	res.Remedy = strings.TrimSpace(res.Remedy)
	switch res.Status {
	case doctorPass, doctorSkip:
		res.Remedy = ""
	case doctorWarn, doctorFail:
	default:
		res.Status = doctorWarn
		if res.Detail == "" {
			res.Detail = "This check returned no answer."
		}
	}
	return res
}

// doctorFailed reports whether any check failed: the exit status.
func doctorFailed(results []doctorResult) bool {
	for _, r := range results {
		if r.Status == doctorFail {
			return true
		}
	}
	return false
}

// doctorSummary is the closing line: how many checks are a warning or a
// failure. Skipped checks do not count.
func doctorSummary(results []doctorResult) (line string, clean bool) {
	n := 0
	for _, r := range results {
		if r.Status == doctorWarn || r.Status == doctorFail {
			n++
		}
	}
	switch n {
	case 0:
		return "Everything looks good", true
	case 1:
		return "1 check needs attention", false
	}
	return fmt.Sprintf("%d checks need attention", n), false
}

// renderDoctorJSON prints the machine-readable form: always an array, one
// element per check, in check order.
func renderDoctorJSON(w io.Writer, results []doctorResult) error {
	if results == nil {
		results = []doctorResult{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(results)
}

// doctorLook is how the text report is drawn. color is true on a terminal:
// glyphs in the brand styles and details wrapped to width. Off a terminal
// (a pipe, a file, a log) the report is plain ASCII markers and unwrapped
// lines, so it greps and diffs.
type doctorLook struct {
	color bool
	width int // terminal columns; 0 = unknown
}

// doctorWrapDefault is the wrap width on a terminal whose size is unknown.
const doctorWrapDefault = 100

// marker is the leading status mark and how to style it.
func (l doctorLook) marker(s doctorStatus) string {
	if !l.color {
		switch s {
		case doctorPass:
			return "[ok]  "
		case doctorWarn:
			return "[warn]"
		case doctorFail:
			return "[FAIL]"
		}
		return "[skip]"
	}
	// There is no red in the palette: a failure is the warning orange,
	// told apart by its glyph, the way `tera up` prints one.
	switch s {
	case doctorPass:
		return styleOK.Render("✓")
	case doctorWarn:
		return styleWarn.Render("!")
	case doctorFail:
		return styleWarn.Bold(true).Render("✗")
	}
	return styleDim.Render("-")
}

func (l doctorLook) markerWidth() int {
	if l.color {
		return 1
	}
	return 6
}

// renderDoctor prints the report: a headline, one aligned block per check
// (marker, title, detail, and the fix on its own line when there is one),
// then the summary line.
func renderDoctor(w io.Writer, headline string, results []doctorResult, look doctorLook) {
	if look.color {
		fmt.Fprintln(w, styleTitle.Render("/// Teraflock doctor"), styleDim.Render(headline))
	} else {
		fmt.Fprintln(w, "Teraflock doctor:", headline)
	}
	fmt.Fprintln(w)

	titleWidth := 0
	for _, r := range results {
		if n := utf8.RuneCountInString(r.Title); n > titleWidth {
			titleWidth = n
		}
	}
	indent := look.markerWidth() + 1 + titleWidth + 2
	wrap := 0
	if look.color {
		wrap = look.width
		if wrap <= 0 {
			wrap = doctorWrapDefault
		}
		// A very narrow terminal still gets readable lines; they wrap on
		// their own past this.
		if wrap = wrap - indent - 1; wrap < 32 {
			wrap = 32
		}
	}
	pad := strings.Repeat(" ", indent)

	for _, r := range results {
		detail := r.Detail
		if r.Status == doctorSkip {
			detail = "Skipped: " + lowerFirst(detail)
		}
		lines := wrapLines(detail, wrap)
		if len(lines) == 0 {
			lines = []string{""}
		}
		title := r.Title + strings.Repeat(" ", titleWidth-utf8.RuneCountInString(r.Title))
		for i, line := range lines {
			if look.color && r.Status == doctorSkip {
				line = styleDim.Render(line)
			}
			if i == 0 {
				fmt.Fprintf(w, "%s %s  %s\n", look.marker(r.Status), title, line)
				continue
			}
			fmt.Fprintf(w, "%s%s\n", pad, line)
		}
		if r.Remedy != "" {
			const lead = "Fix: "
			fix := wrapLines(r.Remedy, max(wrap-len(lead), 0))
			for i, line := range fix {
				prefix := lead
				if i > 0 {
					prefix = strings.Repeat(" ", len(lead))
				}
				if look.color {
					fmt.Fprintf(w, "%s%s%s\n", pad, styleWarn.Render(prefix), line)
					continue
				}
				fmt.Fprintf(w, "%s%s%s\n", pad, prefix, line)
			}
		}
	}

	fmt.Fprintln(w)
	summary, clean := doctorSummary(results)
	switch {
	case !look.color:
		fmt.Fprintln(w, summary)
	case clean:
		fmt.Fprintln(w, styleOK.Render(summary))
	default:
		fmt.Fprintln(w, styleWarn.Render(summary))
	}
}

// wrapLines splits text on newlines and, when width > 0, wraps each line
// at word boundaries. A word longer than the width (a path, a URL) stays
// whole on its own line.
func wrapLines(text string, width int) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \t")
		if width <= 0 || utf8.RuneCountInString(line) <= width {
			out = append(out, line)
			continue
		}
		cur, curLen := "", 0
		for _, word := range strings.Fields(line) {
			n := utf8.RuneCountInString(word)
			if curLen > 0 && curLen+1+n > width {
				out = append(out, cur)
				cur, curLen = "", 0
			}
			if curLen > 0 {
				cur += " "
				curLen++
			}
			cur += word
			curLen += n
		}
		if curLen > 0 {
			out = append(out, cur)
		}
	}
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// lowerFirst lowercases a leading ASCII capital, for a sentence that
// continues another ("Skipped: the daemon ...").
func lowerFirst(s string) string {
	if len(s) > 1 && s[0] >= 'A' && s[0] <= 'Z' && (s[1] < 'A' || s[1] > 'Z') {
		return string(s[0]+('a'-'A')) + s[1:]
	}
	return s
}

// finding accumulates what one check learned: its lines, the worst status
// seen and the fix for that worst status. Checks that look at several
// things use it so one problem never hides another's explanation.
type finding struct {
	status doctorStatus
	lines  []string
	remedy string
}

func doctorSeverity(s doctorStatus) int {
	switch s {
	case doctorWarn:
		return 1
	case doctorFail:
		return 2
	}
	return 0
}

// note adds a line without changing the status.
func (f *finding) note(format string, args ...any) {
	f.lines = append(f.lines, fmt.Sprintf(format, args...))
}

// raise adds a line and, when st is worse than anything so far, makes it
// the check's status with remedy as its fix.
func (f *finding) raise(st doctorStatus, remedy, format string, args ...any) {
	f.note(format, args...)
	if doctorSeverity(st) > doctorSeverity(f.status) {
		f.status, f.remedy = st, remedy
	}
}

func (f *finding) result() doctorResult {
	st := f.status
	if st == "" {
		st = doctorPass
	}
	return doctorResult{Status: st, Detail: strings.Join(f.lines, "\n"), Remedy: f.remedy}
}

func doctorSkipped(format string, args ...any) doctorResult {
	return doctorResult{Status: doctorSkip, Detail: fmt.Sprintf(format, args...)}
}

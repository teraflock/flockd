package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func staticCheck(id, title string, res doctorResult) doctorCheck {
	return doctorCheck{id, title, time.Second, func(context.Context, *doctorEnv) doctorResult { return res }}
}

// A check that panics and a check that never returns each become a
// warning on their own line; the checks around them still report, in list
// order, and neither turns the exit status to 1.
func TestRunDoctorIsolatesBrokenChecks(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	checks := []doctorCheck{
		staticCheck("a", "A", doctorResult{Status: doctorPass, Detail: "fine"}),
		{"boom", "Boom", time.Second, func(context.Context, *doctorEnv) doctorResult { panic("kaboom") }},
		{"hang", "Hang", 20 * time.Millisecond, func(context.Context, *doctorEnv) doctorResult {
			<-block // ignores its context
			return doctorResult{Status: doctorPass}
		}},
		staticCheck("z", "Z", doctorResult{Status: doctorFail, Detail: "broken", Remedy: "fix it"}),
	}
	start := time.Now()
	got := runDoctor(context.Background(), &doctorEnv{}, checks)
	if d := time.Since(start); d > doctorGrace+2*time.Second {
		t.Fatalf("runDoctor took %s with a hung check", d)
	}
	if len(got) != 4 {
		t.Fatalf("got %d results, want 4", len(got))
	}
	for i, id := range []string{"a", "boom", "hang", "z"} {
		if got[i].ID != id {
			t.Errorf("result %d is %q, want %q (order must be the list's)", i, got[i].ID, id)
		}
	}
	if got[0].Status != doctorPass || got[3].Status != doctorFail {
		t.Errorf("healthy checks were disturbed: %+v / %+v", got[0], got[3])
	}
	if got[1].Status != doctorWarn || !strings.Contains(got[1].Detail, "kaboom") || !strings.Contains(got[1].Remedy, doctorIssuesURL) {
		t.Errorf("panicking check = %+v", got[1])
	}
	if got[2].Status != doctorWarn || !strings.Contains(got[2].Detail, "did not finish within 20ms") || !strings.Contains(got[2].Remedy, "tera doctor") {
		t.Errorf("hung check = %+v", got[2])
	}
}

// Every check gets its own deadline through the context.
func TestRunDoctorCheckTimeoutReachesTheCheck(t *testing.T) {
	c := doctorCheck{"slow", "Slow", 30 * time.Millisecond, func(ctx context.Context, _ *doctorEnv) doctorResult {
		select {
		case <-ctx.Done():
			return doctorResult{Status: doctorFail, Detail: "gave up in time", Remedy: "r"}
		case <-time.After(5 * time.Second):
			return doctorResult{Status: doctorPass}
		}
	}}
	got := runDoctor(context.Background(), &doctorEnv{}, []doctorCheck{c})
	if got[0].Status != doctorFail || got[0].Detail != "gave up in time" {
		t.Fatalf("got %+v", got[0])
	}
}

func TestFinishDoctorResultHoldsTheContract(t *testing.T) {
	c := doctorCheck{id: "x", title: "X"}
	for _, tc := range []struct {
		name string
		in   doctorResult
		want doctorResult
	}{
		{"pass drops a remedy", doctorResult{Status: doctorPass, Detail: " ok ", Remedy: "do"}, doctorResult{ID: "x", Title: "X", Status: doctorPass, Detail: "ok"}},
		{"skip drops a remedy", doctorResult{Status: doctorSkip, Detail: "n/a", Remedy: "do"}, doctorResult{ID: "x", Title: "X", Status: doctorSkip, Detail: "n/a"}},
		{"warn keeps it", doctorResult{Status: doctorWarn, Detail: "hm", Remedy: "do"}, doctorResult{ID: "x", Title: "X", Status: doctorWarn, Detail: "hm", Remedy: "do"}},
		{"fail keeps it", doctorResult{Status: doctorFail, Detail: "no", Remedy: "do"}, doctorResult{ID: "x", Title: "X", Status: doctorFail, Detail: "no", Remedy: "do"}},
		{"an empty answer is a warning", doctorResult{}, doctorResult{ID: "x", Title: "X", Status: doctorWarn, Detail: "This check returned no answer."}},
		{"an unknown status is a warning", doctorResult{Status: "great", Detail: "d"}, doctorResult{ID: "x", Title: "X", Status: doctorWarn, Detail: "d"}},
	} {
		if got := finishDoctorResult(c, tc.in); got != tc.want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, tc.want)
		}
	}
}

func TestDoctorSummaryAndExit(t *testing.T) {
	r := func(sts ...doctorStatus) []doctorResult {
		var out []doctorResult
		for _, s := range sts {
			out = append(out, doctorResult{Status: s})
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		results []doctorResult
		summary string
		failed  bool
	}{
		{"nothing ran", nil, "Everything looks good", false},
		{"all pass", r(doctorPass, doctorPass), "Everything looks good", false},
		{"skips do not count", r(doctorPass, doctorSkip, doctorSkip), "Everything looks good", false},
		{"one warning", r(doctorPass, doctorWarn), "1 check needs attention", false},
		{"one failure", r(doctorFail, doctorPass), "1 check needs attention", true},
		{"mixed", r(doctorFail, doctorWarn, doctorWarn, doctorSkip, doctorPass), "3 checks need attention", true},
		{"warnings only never fail", r(doctorWarn, doctorWarn), "2 checks need attention", false},
	} {
		got, clean := doctorSummary(tc.results)
		if got != tc.summary || clean != (tc.summary == "Everything looks good") {
			t.Errorf("%s: summary = %q (clean %v), want %q", tc.name, got, clean, tc.summary)
		}
		if doctorFailed(tc.results) != tc.failed {
			t.Errorf("%s: failed = %v, want %v", tc.name, !tc.failed, tc.failed)
		}
	}
}

var renderSample = []doctorResult{
	{ID: "install", Title: "Install", Status: doctorPass, Detail: "tera 1.0.0 and flockd 1.0.0 are installed in /usr/local/bin."},
	{ID: "service", Title: "Service", Status: doctorWarn, Detail: "The systemd user service is installed and running.\nLingering is off for ada.", Remedy: "Run `sudo loginctl enable-linger ada`."},
	{ID: "enrollment", Title: "Enrollment", Status: doctorFail, Detail: "This node is not enrolled.", Remedy: "Run `tera login`."},
	{ID: "mesh", Title: "Mesh", Status: doctorSkip, Detail: "The node runs standalone."},
}

// The plain form (stdout is not a terminal): ASCII markers, one aligned
// column, the fix under the detail, no styling, the summary last.
func TestRenderDoctorPlain(t *testing.T) {
	var b bytes.Buffer
	renderDoctor(&b, "tera 1.0.0, data in /home/ada/.teraflock", renderSample, doctorLook{})
	want := strings.Join([]string{
		"Teraflock doctor: tera 1.0.0, data in /home/ada/.teraflock",
		"",
		"[ok]   Install     tera 1.0.0 and flockd 1.0.0 are installed in /usr/local/bin.",
		"[warn] Service     The systemd user service is installed and running.",
		"                   Lingering is off for ada.",
		"                   Fix: Run `sudo loginctl enable-linger ada`.",
		"[FAIL] Enrollment  This node is not enrolled.",
		"                   Fix: Run `tera login`.",
		"[skip] Mesh        Skipped: the node runs standalone.",
		"",
		"2 checks need attention",
		"",
	}, "\n")
	if got := b.String(); got != want {
		t.Fatalf("plain report:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(b.String(), "\x1b[") {
		t.Fatal("plain report carries terminal escapes")
	}
}

// The terminal form: glyph markers and details wrapped to the window with
// a hanging indent, so a long sentence stays in its column.
func TestRenderDoctorTerminalWrapsToWidth(t *testing.T) {
	long := []doctorResult{{ID: "daemon", Title: "Daemon", Status: doctorWarn,
		Detail: "one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen",
		Remedy: "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigma"}}
	var b bytes.Buffer
	renderDoctor(&b, "h", long, doctorLook{color: true, width: 50})
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	// headline, blank, >= 2 detail lines, >= 2 fix lines, blank, summary
	if len(lines) < 8 {
		t.Fatalf("expected wrapped output, got %d lines:\n%s", len(lines), b.String())
	}
	for _, l := range lines[2 : len(lines)-2] {
		if n := len([]rune(stripANSI(l))); n > 50 {
			t.Errorf("line is %d columns, wider than the 50-column window: %q", n, l)
		}
	}
	plain := stripANSI(b.String())
	for _, want := range []string{"! Daemon  one two", "\n          ", "Fix: alpha", "1 check needs attention"} {
		if !strings.Contains(plain, want) {
			t.Errorf("terminal report misses %q:\n%s", want, plain)
		}
	}
	var all bytes.Buffer
	renderDoctor(&all, "h", renderSample, doctorLook{color: true})
	for _, glyph := range []string{"✓ Install", "! Service", "✗ Enrollment", "- Mesh"} {
		if !strings.Contains(stripANSI(all.String()), glyph) {
			t.Errorf("terminal report misses %q:\n%s", glyph, stripANSI(all.String()))
		}
	}
}

// stripANSI removes SGR escapes (the test may or may not run with colour).
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestRenderDoctorEverythingGood(t *testing.T) {
	var b bytes.Buffer
	renderDoctor(&b, "h", renderSample[:1], doctorLook{})
	if !strings.HasSuffix(b.String(), "\nEverything looks good\n") {
		t.Fatalf("summary line:\n%s", b.String())
	}
}

func TestWrapLines(t *testing.T) {
	for _, tc := range []struct {
		in    string
		width int
		want  []string
	}{
		{"", 10, nil},
		{"short", 0, []string{"short"}},
		{"a b\nc", 0, []string{"a b", "c"}},
		{"aaa bbb ccc", 7, []string{"aaa bbb", "ccc"}},
		{"aaa bbb ccc", 3, []string{"aaa", "bbb", "ccc"}},
		{"/a/very/long/path/that/does/not/fit tail", 10, []string{"/a/very/long/path/that/does/not/fit", "tail"}},
		{"héllo wörld", 5, []string{"héllo", "wörld"}},
	} {
		got := wrapLines(tc.in, tc.width)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") || len(got) != len(tc.want) {
			t.Errorf("wrapLines(%q, %d) = %q, want %q", tc.in, tc.width, got, tc.want)
		}
	}
}

// --json: an array of exactly {id, title, status, detail, remedy}, in
// check order, with remedy present (empty) on a passing check.
func TestRenderDoctorJSON(t *testing.T) {
	var b bytes.Buffer
	if err := renderDoctorJSON(&b, renderSample); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatalf("not a JSON array: %v\n%s", err, b.String())
	}
	if len(got) != len(renderSample) {
		t.Fatalf("%d elements, want %d", len(got), len(renderSample))
	}
	for i, el := range got {
		if len(el) != 5 {
			t.Errorf("element %d has keys %v, want exactly id,title,status,detail,remedy", i, el)
		}
		for _, k := range []string{"id", "title", "status", "detail", "remedy"} {
			if _, ok := el[k].(string); !ok {
				t.Errorf("element %d: %q is missing or not a string", i, k)
			}
		}
		if el["id"] != renderSample[i].ID || el["status"] != string(renderSample[i].Status) {
			t.Errorf("element %d = %v, want %s/%s", i, el, renderSample[i].ID, renderSample[i].Status)
		}
	}
	if got[0]["remedy"] != "" || got[2]["remedy"] != "Run `tera login`." {
		t.Errorf("remedies = %q / %q", got[0]["remedy"], got[2]["remedy"])
	}
	// Commands in remedies must survive as typed (no \u0026 for &&, <id>).
	b.Reset()
	_ = renderDoctorJSON(&b, []doctorResult{{Remedy: "a && b <id>"}})
	if !strings.Contains(b.String(), "a && b <id>") {
		t.Errorf("JSON escapes shell characters: %s", b.String())
	}
	b.Reset()
	if err := renderDoctorJSON(&b, nil); err != nil || strings.TrimSpace(b.String()) != "[]" {
		t.Errorf("no results = %q, %v; want []", b.String(), err)
	}
}

// The whole command path: exit status 1 (errDoctorFailed) only when a
// check failed, in both output forms.
func TestDoctorReportExitStatus(t *testing.T) {
	pass := staticCheck("a", "A", doctorResult{Status: doctorPass, Detail: "ok"})
	warn := staticCheck("b", "B", doctorResult{Status: doctorWarn, Detail: "hm", Remedy: "r"})
	fail := staticCheck("c", "C", doctorResult{Status: doctorFail, Detail: "no", Remedy: "r"})
	skip := staticCheck("d", "D", doctorResult{Status: doctorSkip, Detail: "n/a"})
	for _, tc := range []struct {
		name     string
		checks   []doctorCheck
		wantFail bool
	}{
		{"all pass", []doctorCheck{pass, pass}, false},
		{"warnings and skips", []doctorCheck{pass, warn, skip}, false},
		{"one failure", []doctorCheck{pass, warn, fail}, true},
	} {
		for _, asJSON := range []bool{false, true} {
			var b bytes.Buffer
			err := doctorReport(context.Background(), &doctorEnv{teraVersion: "t", dataDir: "/d"}, tc.checks, &b, asJSON, doctorLook{})
			if (err != nil) != tc.wantFail || (err != nil && !errors.Is(err, errDoctorFailed)) {
				t.Errorf("%s (json=%v): err = %v, want failure %v", tc.name, asJSON, err, tc.wantFail)
			}
			if asJSON != json.Valid(b.Bytes()) {
				t.Errorf("%s (json=%v): output form is wrong:\n%s", tc.name, asJSON, b.String())
			}
		}
	}
}

// The command is registered with its flag, and takes no arguments.
func TestDoctorCommandShape(t *testing.T) {
	c := cmdDoctor()
	if c.Use != "doctor" || c.Flags().Lookup("json") == nil {
		t.Fatalf("cmdDoctor: use %q, flags %v", c.Use, c.Flags().FlagUsages())
	}
	if err := c.Args(c, []string{"extra"}); err == nil {
		t.Fatal("doctor accepts a positional argument")
	}
}

func TestDoctorChecksListIsStable(t *testing.T) {
	var ids []string
	for _, c := range doctorChecks() {
		ids = append(ids, c.id)
		if c.title == "" || c.run == nil || c.timeout <= 0 || c.timeout > 20*time.Second {
			t.Errorf("check %q: title %q, timeout %s", c.id, c.title, c.timeout)
		}
	}
	if got, want := strings.Join(ids, ","), "install,service,daemon,enrollment,mesh,runtime,models,update"; got != want {
		t.Fatalf("checks = %s, want %s (ids and order are part of the --json contract)", got, want)
	}
}

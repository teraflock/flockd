//go:build windows

package svc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Lock in the restart-storm cap (the systemd StartLimitBurst incident),
// the interactive-session principal the idle source depends on, the
// log-file wiring and the quoting of paths with spaces.
func TestRenderTaskXML(t *testing.T) {
	x := renderTaskXML(`C:\Program Files\Teraflock\flockd.exe`, []string{"--standalone"},
		Options{LogPath: `C:\Users\Jane Doe\.teraflock\flockd.log`}, `DESKTOP-1\Jane Doe`)
	for _, want := range []string{
		`<URI>\flockd</URI>`,
		`<LogonType>InteractiveToken</LogonType>`,
		`<RunLevel>LeastPrivilege</RunLevel>`,
		`<UserId>DESKTOP-1\Jane Doe</UserId>`,
		`<LogonTrigger>`,
		`<Interval>PT1M</Interval>`,
		`<Count>5</Count>`,
		`<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>`,
		`<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>`,
		`<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>`,
		`<Priority>5</Priority>`,
		`<Command>C:\Program Files\Teraflock\flockd.exe</Command>`,
		`<Arguments>--standalone --log-file &#34;C:\Users\Jane Doe\.teraflock\flockd.log&#34;</Arguments>`,
		`<WorkingDirectory>C:\Program Files\Teraflock</WorkingDirectory>`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task xml missing %q\n---\n%s", want, x)
		}
	}
	if noLog := renderTaskXML(`C:\flockd.exe`, nil, Options{}, `X\y`); strings.Contains(noLog, "--log-file") {
		t.Error("--log-file rendered without a LogPath")
	}
	if !strings.Contains(renderTaskXML(`C:\flockd.exe`, nil, Options{}, `X\y`), `<Arguments></Arguments>`) {
		t.Error("no args should render an empty Arguments element")
	}
	if esc := renderTaskXML(`C:\a&b\flockd.exe`, nil, Options{}, `X\y`); !strings.Contains(esc, `C:\a&amp;b\flockd.exe`) {
		t.Errorf("XML escaping: %s", esc)
	}
}

func TestQuoteArg(t *testing.T) {
	cases := map[string]string{
		"--standalone":        "--standalone",
		`C:\plain\path`:       `C:\plain\path`,
		`C:\with space\x.log`: `"C:\with space\x.log"`,
		`say "hi"`:            `"say \"hi\""`,
		"":                    `""`,
	}
	for in, want := range cases {
		if got := quoteArg(in); got != want {
			t.Errorf("quoteArg(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestUTF16LEBOM(t *testing.T) {
	b := utf16LEBOM("<T/>")
	want := []byte{0xFF, 0xFE, '<', 0, 'T', 0, '/', 0, '>', 0}
	if string(b) != string(want) {
		t.Errorf("utf16LEBOM = % x, want % x", b, want)
	}
}

func TestParseTaskState(t *testing.T) {
	cases := map[string]Status{
		"Running\r\n":      StatusRunning,
		"Ready\r\n":        StatusStopped,
		"Disabled":         StatusStopped,
		"Queued\n":         StatusStopped,
		"NotInstalled\r\n": StatusNotInstalled,
		"":                 StatusUnknown,
		"Unknown\r\n":      StatusUnknown,
	}
	for in, want := range cases {
		if got := parseTaskState(in); got != want {
			t.Errorf("parseTaskState(%q) = %s, want %s", in, got, want)
		}
	}
}

// Live smoke test against the real Task Scheduler (runs on the
// windows-latest CI lane; set FLOCKD_SVC_LIVE=1 to run it elsewhere —
// it registers and removes a task named flockd, so not on a box that
// has the daemon installed). The "daemon" is a batch file that writes a
// marker and exits 0, so the task can be started, seen and ended without
// a flockd.exe.
func TestTaskManagerLive(t *testing.T) {
	if os.Getenv("FLOCKD_SVC_LIVE") == "" && os.Getenv("GITHUB_ACTIONS") == "" {
		t.Skip("set FLOCKD_SVC_LIVE=1 to exercise schtasks for real")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m := &taskManager{}

	if st, err := m.Status(ctx); err != nil {
		t.Fatalf("Status before install: %v", err)
	} else if st != StatusNotInstalled {
		t.Fatalf("a flockd task already exists (state %s); refusing to clobber it", st)
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "ran.txt")
	bin := filepath.Join(dir, "fake-flockd.cmd")
	// The task passes --log-file <path>; the fake daemon records that it
	// ran and what it was given, then sleeps a little so Running is
	// observable, and exits 0 (no RestartOnFailure retry).
	script := "@echo started %* > \"" + marker + "\"\r\n@ping -n 4 127.0.0.1 > nul\r\n@exit /b 0\r\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "flockd.log")
	if err := m.Install(ctx, bin, []string{"--standalone"}, Options{LogPath: logPath}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	t.Cleanup(func() {
		if err := m.Uninstall(context.Background()); err != nil {
			t.Errorf("cleanup Uninstall: %v", err)
		}
	})
	def, err := m.definitionPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(def); err != nil {
		t.Errorf("definition file not written: %v", err)
	}
	if st, err := m.Status(ctx); err != nil || st != StatusStopped {
		t.Fatalf("Status after install = %s, %v; want stopped", st, err)
	}
	// schtasks /Query is the operator-visible check the issue names.
	if out, err := exec.CommandContext(ctx, "schtasks", "/Query", "/TN", taskName).CombinedOutput(); err != nil {
		t.Errorf("schtasks /Query /TN flockd: %v (%s)", err, out)
	}

	if err := m.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		if b, err := os.ReadFile(marker); err == nil {
			got := strings.TrimSpace(string(b))
			if !strings.Contains(got, "--standalone") || !strings.Contains(got, "--log-file") || !strings.Contains(got, logPath) {
				t.Errorf("fake daemon argv = %q; want --standalone and --log-file %s", got, logPath)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task started but the fake daemon never ran (marker missing after 30s)")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// Running is observable while the script pings.
	if st, err := m.Status(ctx); err != nil {
		t.Errorf("Status while running: %v", err)
	} else {
		t.Logf("state while the fake daemon runs: %s", st)
	}

	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if st, err := m.Status(ctx); err != nil || st != StatusStopped {
		t.Errorf("Status after stop = %s, %v; want stopped (disabled)", st, err)
	}

	if err := m.Uninstall(ctx); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if st, err := m.Status(ctx); err != nil || st != StatusNotInstalled {
		t.Errorf("Status after uninstall = %s, %v; want not-installed", st, err)
	}
	if out, err := exec.CommandContext(ctx, "schtasks", "/Query", "/TN", taskName).CombinedOutput(); err == nil {
		t.Errorf("schtasks still finds the task after uninstall: %s", out)
	}
	if _, err := os.Stat(def); !os.IsNotExist(err) {
		t.Errorf("definition file still present after uninstall: %v", err)
	}
}

//go:build windows

package llamacpp

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// Live tests of the job object and the console break; they run for real
// on the windows-latest CI lane against the fake llama-server child from
// supervisor_test.go (the real llama-server.exe needs a Windows runtime
// artifact, teraflock/docs#8).

func healthOK(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func waitHealth(t *testing.T, url string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for healthOK(url) != want {
		if time.Now().After(deadline) {
			t.Fatalf("child at %s: healthy=%v after %s, want %v", url, !want, timeout, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// The supervisor's own path: a spawned child joins the job, and closing
// the guard (what stop() does last, and what the kernel does when the
// daemon dies) kills it.
func TestJobObjectKillsChildOnClose(t *testing.T) {
	s, url := newTestSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	pid := s.pid()
	if s.guard == nil || s.guard.job == 0 {
		t.Fatal("supervisor has no job object after start")
	}
	// Mark the supervisor stopped first so the supervise loop does not
	// respawn the child the moment the job kills it.
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	s.mu.Unlock()
	if err := s.guard.Close(); err != nil {
		t.Fatalf("close job: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatalf("child pid %d still alive 10s after the job handle closed", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitHealth(t, url, false, 5*time.Second)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervise loop did not exit after the child was killed")
	}
}

// The crash case flockd#31 is about: the "daemon" (helper process) is
// hard-killed with TerminateProcess — no stop path runs — and the child
// it spawned under the job must die with it.
func TestJobObjectKillsChildWhenParentDies(t *testing.T) {
	port, err := ephemeralPort()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	parent := exec.Command(exe)
	parent.Env = append(os.Environ(), "FLOCKD_JOB_PARENT_PORT="+strconv.Itoa(port))
	parent.Stderr = os.Stderr
	stdout, err := parent.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = parent.Process.Kill()
		t.Fatalf("helper did not report its child: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "child ")))
	if err != nil {
		_ = parent.Process.Kill()
		t.Fatalf("helper output %q", line)
	}
	t.Cleanup(func() {
		if processAlive(childPID) {
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(childPID)); err == nil {
				_ = windows.TerminateProcess(h, 1)
				_ = windows.CloseHandle(h)
			}
		}
	})
	waitHealth(t, url, true, 20*time.Second)
	if !processAlive(childPID) {
		t.Fatalf("child pid %d not alive while parent runs", childPID)
	}

	if err := parent.Process.Kill(); err != nil { // TerminateProcess: no cleanup runs in the helper
		t.Fatal(err)
	}
	_ = parent.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for processAlive(childPID) {
		if time.Now().After(deadline) {
			t.Fatalf("orphan: child pid %d survived its parent's death for 10s", childPID)
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitHealth(t, url, false, 5*time.Second)
}

// Console break: only meaningful when the test process has a console
// (interactive run). CI runners typically do not; the test reports which
// path it took instead of pretending.
func TestTerminateConsoleBreak(t *testing.T) {
	attr := childSysProcAttr()
	if attr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("child must be created in its own process group")
	}
	if !hasConsole() {
		if attr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
			t.Error("no console: child must get CREATE_NO_WINDOW")
		}
		t.Skip("no console attached: terminate() is a no-op here and stop() relies on the grace timer + Kill (verified by TestSupervisorStartHealthStop)")
	}
	if attr.CreationFlags&windows.CREATE_NO_WINDOW != 0 {
		t.Error("console attached: child must share it (no CREATE_NO_WINDOW) for CTRL_BREAK to reach it")
	}
	s, url := newTestSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.stopped = true // keep the supervise loop from respawning
	cmd := s.cmd
	s.mu.Unlock()
	if err := terminate(cmd); err != nil {
		t.Fatalf("GenerateConsoleCtrlEvent: %v", err)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child ignored CTRL_BREAK for 5s")
	}
	waitHealth(t, url, false, 5*time.Second)
	_ = s.guard.Close()
}

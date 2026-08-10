package llamacpp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// supervisor keeps one llama-server child process alive: start, health-gate,
// restart with exponential backoff on crash, graceful stop. A llama.cpp
// crash never takes the daemon down (SPEC §A1.3).
type supervisor struct {
	bin    string
	args   []string
	url    string // http://127.0.0.1:<port>
	log    *slog.Logger
	client *http.Client

	restarts   atomic.Int64
	healthyFn  func(ctx context.Context) error
	backoffMin time.Duration
	backoffMax time.Duration

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	done    chan struct{} // closed when the run loop exits
}

func newSupervisor(bin string, args []string, url string, log *slog.Logger) *supervisor {
	s := &supervisor{
		bin:        bin,
		args:       args,
		url:        url,
		log:        log,
		client:     &http.Client{Timeout: 5 * time.Second},
		backoffMin: time.Second,
		backoffMax: 30 * time.Second,
		done:       make(chan struct{}),
	}
	s.healthyFn = s.checkHealth
	return s
}

// start launches the child and blocks until it reports healthy (or ctx ends).
// It then keeps supervising in the background.
func (s *supervisor) start(ctx context.Context) error {
	if err := s.spawn(); err != nil {
		return err
	}
	if err := s.waitHealthy(ctx, 120*time.Second); err != nil {
		s.stop(context.Background())
		return fmt.Errorf("llamacpp: llama-server never became healthy: %w", err)
	}
	go s.superviseLoop()
	return nil
}

func (s *supervisor) spawn() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return fmt.Errorf("llamacpp: supervisor stopped")
	}
	cmd := exec.Command(s.bin, s.args...) //nolint:gosec // pinned, SHA-verified binary
	cmd.Stdout = slogWriter{s.log, slog.LevelDebug}
	cmd.Stderr = slogWriter{s.log, slog.LevelDebug}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("llamacpp: start llama-server: %w", err)
	}
	s.cmd = cmd
	s.log.Info("llama-server started", "pid", cmd.Process.Pid, "url", s.url)
	return nil
}

func (s *supervisor) superviseLoop() {
	defer close(s.done)
	backoff := s.backoffMin
	for {
		s.mu.Lock()
		cmd, stopped := s.cmd, s.stopped
		s.mu.Unlock()
		if stopped || cmd == nil {
			return
		}

		started := time.Now()
		err := cmd.Wait()

		s.mu.Lock()
		stopped = s.stopped
		s.mu.Unlock()
		if stopped {
			return
		}

		s.log.Warn("llama-server exited unexpectedly, restarting", "err", err, "uptime", time.Since(started))
		if time.Since(started) > time.Minute {
			backoff = s.backoffMin // healthy for a while: reset backoff
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, s.backoffMax)

		if err := s.spawn(); err != nil {
			s.log.Error("llama-server respawn failed", "err", err)
			return
		}
		s.restarts.Add(1)
		if err := s.waitHealthy(context.Background(), 120*time.Second); err != nil {
			s.log.Error("restarted llama-server unhealthy", "err", err)
		}
	}
}

func (s *supervisor) waitHealthy(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := s.healthyFn(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (s *supervisor) checkHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health: status %s", resp.Status)
	}
	return nil
}

// stop terminates the child gracefully (SIGTERM, then SIGKILL after 5s).
func (s *supervisor) stop(ctx context.Context) {
	s.mu.Lock()
	s.stopped = true
	cmd := s.cmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = terminate(cmd)
	killTimer := time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	select {
	case <-s.done:
	case <-ctx.Done():
		_ = cmd.Process.Kill()
	case <-time.After(6 * time.Second):
	}
	killTimer.Stop()
}

// ephemeralPort asks the kernel for a free loopback port. Inherently a small
// TOCTOU race, but standard practice for supervising servers that pick their
// own listener from a --port flag.
func ephemeralPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("llamacpp: reserve port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

type slogWriter struct {
	log   *slog.Logger
	level slog.Level
}

func (w slogWriter) Write(p []byte) (int, error) {
	w.log.Log(context.Background(), w.level, "llama-server", "out", string(p))
	return len(p), nil
}

package llamacpp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestMain doubles as the fake llama-server child process: when re-exec'd
// with HIVED_FAKE_LLAMA_PORT set, it serves /health on that port until
// killed (the standard helper-process pattern).
func TestMain(m *testing.M) {
	if port := os.Getenv("HIVED_FAKE_LLAMA_PORT"); port != "" {
		runFakeLlamaChild(port)
		return
	}
	os.Exit(m.Run())
}

func runFakeLlamaChild(port string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/die", func(w http.ResponseWriter, r *http.Request) {
		os.Exit(3)
	})
	l, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		os.Exit(1)
	}
	_ = http.Serve(l, mux) //nolint:gosec // test helper
	os.Exit(0)
}

// newTestSupervisor re-execs the test binary as the supervised child.
func newTestSupervisor(t *testing.T) (*supervisor, string) {
	t.Helper()
	port, err := ephemeralPort()
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := newSupervisor(exe, nil, url, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	s.backoffMin = 50 * time.Millisecond
	// exec.Cmd env is inherited from the parent; set the trigger var here
	// and clean it up after.
	t.Setenv("HIVED_FAKE_LLAMA_PORT", strconv.Itoa(port))
	return s, url
}

func TestSupervisorStartHealthStop(t *testing.T) {
	s, _ := newTestSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.checkHealth(ctx); err != nil {
		t.Fatalf("health after start: %v", err)
	}
	s.stop(ctx)
	select {
	case <-s.done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor loop did not exit after stop")
	}
	if err := s.checkHealth(context.Background()); err == nil {
		t.Fatal("child still serving after stop")
	}
}

func TestSupervisorRestartsOnCrash(t *testing.T) {
	s, url := newTestSupervisor(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.stop(context.Background())

	// Ask the child to crash itself.
	_, _ = http.Get(url + "/die")

	deadline := time.Now().Add(30 * time.Second)
	for {
		if s.restarts.Load() >= 1 && s.checkHealth(ctx) == nil {
			return // restarted and healthy again
		}
		if time.Now().After(deadline) {
			t.Fatalf("child not restarted: restarts=%d", s.restarts.Load())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

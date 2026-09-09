package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/localapi/client"
)

// fakeDaemon serves just enough of the local API for the pull loop: the
// event stream (scripted) and the model list (state flips to ready when
// the script says so).
func fakeDaemon(t *testing.T, script string, ready *atomic.Bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, chunk := range strings.Split(script, "|") {
			fmt.Fprint(w, chunk)
			fl.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) {
		state := "downloading"
		if ready.Load() {
			state = "ready"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]any{
			{"id": "m1", "size_bytes": 1000, "state": state, "origin": "operator"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestClient(t *testing.T, base string) *client.Client {
	t.Helper()
	c, err := client.FromResolved(client.Resolved{Base: base, Token: "tok", TokenSource: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWaitForPullFollowsProgressToReady(t *testing.T) {
	var ready atomic.Bool
	script := "event: status\ndata: {}\n\n" +
		"|event: model_progress\ndata: {\"model\":\"other\",\"received_bytes\":1,\"total_bytes\":2}\n\n" +
		"|event: model_progress\ndata: {\"model\":\"m1\",\"received_bytes\":500,\"total_bytes\":1000}\n\n" +
		"|event: models_changed\ndata: {\"model\":\"m1\",\"change\":\"downloaded\"}\n\n"
	srv := fakeDaemon(t, script, &ready)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := waitForPull(ctx, newTestClient(t, srv.URL), &out, "m1", false); err != nil {
		t.Fatalf("waitForPull: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "50%") || !strings.Contains(s, "100%") || !strings.Contains(s, "m1 ready") {
		t.Errorf("progress output missing 50%%/100%%/ready:\n%s", s)
	}
	if strings.Contains(s, "other") {
		t.Errorf("progress for another model leaked:\n%s", s)
	}
}

func TestWaitForPullReportsFailure(t *testing.T) {
	var ready atomic.Bool
	script := "event: activity\ndata: {\"kind\":\"download_failed\",\"model\":\"m1\",\"detail\":\"sha256 mismatch\"}\n\n"
	srv := fakeDaemon(t, script, &ready)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitForPull(ctx, newTestClient(t, srv.URL), &out, "m1", false)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want the daemon's reason", err)
	}
}

func TestWaitForPullFallsBackToPolling(t *testing.T) {
	// The stream sends nothing useful; the model list flips to ready and
	// the 2s poll must notice.
	var ready atomic.Bool
	srv := fakeDaemon(t, "event: status\ndata: {}\n\n", &ready)
	go func() { time.Sleep(300 * time.Millisecond); ready.Store(true) }()
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitForPull(ctx, newTestClient(t, srv.URL), &out, "m1", false); err != nil {
		t.Fatalf("waitForPull: %v", err)
	}
	if !strings.Contains(out.String(), "m1 ready") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestPullMsgFromEventIgnoresOtherModels(t *testing.T) {
	ev := client.Event{Name: "models_changed", Data: json.RawMessage(`{"model":"zzz","change":"downloaded"}`)}
	if _, ok := pullMsgFromEvent(ev, "m1"); ok {
		t.Error("event for another model accepted")
	}
	ev = client.Event{Name: "models_changed", Data: json.RawMessage(`{"model":"m1","change":"loaded"}`)}
	if _, ok := pullMsgFromEvent(ev, "m1"); ok {
		t.Error("non-download change accepted")
	}
}

func TestProgressPipedPrintsPerDecile(t *testing.T) {
	var out bytes.Buffer
	p := newProgress(&out, false)
	for _, b := range []int64{50, 120, 130, 900, 1000} {
		p.update(b, 1000)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 { // 5%, 12%, 90%, 100% — 13% shares 12%'s decile
		t.Fatalf("lines = %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "  5%") || !strings.Contains(lines[3], "100%") {
		t.Errorf("first/last lines = %q / %q", lines[0], lines[3])
	}
}

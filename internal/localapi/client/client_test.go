package client_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/client/clienttest"
)

func newClient(t *testing.T, d *clienttest.Daemon, token string) *client.Client {
	t.Helper()
	c, err := client.FromResolved(client.Resolved{Base: d.URL, Token: token, TokenSource: "test", DataDir: "/tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestResolveTokenPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FLOCKD_DATA_DIR", dir)
	t.Setenv("TERA_TOKEN", "")

	// Nothing anywhere: no token, and the source says where we looked.
	r := client.Resolve(client.Options{})
	if r.Token != "" || !strings.Contains(r.TokenSource, filepath.Join(dir, client.TokenFile)) {
		t.Fatalf("empty resolve = %+v", r)
	}
	if r.Base != client.DefaultBase || r.DataDir != dir {
		t.Fatalf("base/dataDir = %q/%q", r.Base, r.DataDir)
	}

	// The file.
	if err := os.WriteFile(filepath.Join(dir, client.TokenFile), []byte("  filetok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = client.Resolve(client.Options{})
	if r.Token != "filetok" || r.TokenSource != filepath.Join(dir, client.TokenFile) {
		t.Fatalf("file resolve = %+v", r)
	}

	// $TERA_TOKEN beats the file.
	t.Setenv("TERA_TOKEN", "envtok ")
	r = client.Resolve(client.Options{})
	if r.Token != "envtok" || r.TokenSource != "$TERA_TOKEN" {
		t.Fatalf("env resolve = %+v", r)
	}

	// --token beats everything; --api trailing slash is trimmed; explicit
	// data dir wins over the env.
	r = client.Resolve(client.Options{Token: "flagtok", API: "http://127.0.0.1:9999/", DataDir: "/elsewhere"})
	if r.Token != "flagtok" || r.TokenSource != "--token" || r.Base != "http://127.0.0.1:9999" || r.DataDir != "/elsewhere" {
		t.Fatalf("flag resolve = %+v", r)
	}
}

func TestStatusModelsAndLimits(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, d.Token)
	ctx := context.Background()

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "serving" || st.NodeId != "node-test" || !st.Standalone || st.Hardware == nil {
		t.Fatalf("status = %+v", st)
	}
	ml, err := c.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The mock runtime has no model store: the list is empty but well-formed.
	if ml.Models == nil {
		t.Fatalf("models = %+v", ml)
	}
	if _, ok, err := c.FindModel(ctx, "nope"); err != nil || ok {
		t.Fatalf("FindModel(nope) = %v, %v", ok, err)
	}
	h, err := c.Health(ctx)
	if err != nil || !h.Ok {
		t.Fatalf("health = %+v, %v", h, err)
	}
	if _, err := c.Earnings(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Activity(ctx); err != nil {
		t.Fatal(err)
	}
	ids, err := c.OpenAIModels(ctx)
	if err != nil || len(ids) != 1 || ids[0] != clienttest.Model {
		t.Fatalf("/v1/models = %v, %v", ids, err)
	}
}

func TestUnauthorizedExplainsTokenSource(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c, err := client.FromResolved(client.Resolved{Base: d.URL, Token: "wrong", TokenSource: "$TERA_TOKEN", DataDir: "/data/dir"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Status(context.Background())
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"$TERA_TOKEN", "--data-dir /data/dir", "tera token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("401 message missing %q:\n%s", want, err)
		}
	}
}

func TestUnreachableDaemon(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	url := d.URL
	d.Close()
	c, err := client.FromResolved(client.Resolved{Base: url})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Status(context.Background())
	var ue *client.UnreachableError
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), "cannot reach flockd at "+url) {
		t.Fatalf("err = %v", err)
	}
	if r := client.Remedy(err); !strings.Contains(r, "tera up") {
		t.Errorf("remedy = %q", r)
	}
	// Streams and chat fail the same way.
	if _, err := c.Events(context.Background(), false); !errors.As(err, &ue) {
		t.Errorf("Events err = %v", err)
	}
	if _, err := c.Chat(context.Background(), client.ChatRequest{Messages: []client.Message{{Role: "user", Content: "hi"}}}); !errors.As(err, &ue) {
		t.Errorf("Chat err = %v", err)
	}
}

func TestUnsupportedRemedy(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, d.Token)
	err := c.LoadModel(context.Background(), "m")
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != 501 || !strings.Contains(err.Error(), "daemon error (501)") {
		t.Fatalf("err = %v", err)
	}
	if r := client.Remedy(err); !strings.Contains(r, "llamacpp") {
		t.Errorf("remedy = %q", r)
	}
}

func TestEventsStream(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, d.Token)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s, err := c.Events(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	wait := func(name string) client.Event {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-s.C:
				if !ok {
					t.Fatalf("stream closed waiting for %s: %v", name, s.Err())
				}
				if ev.Name == name {
					return ev
				}
			case <-deadline:
				t.Fatalf("no %s event within 5s", name)
			}
		}
	}
	if ev := wait("status"); !strings.Contains(string(ev.Data), `"state":"serving"`) {
		t.Fatalf("status event = %s", ev.Data)
	}
	d.Events.Publish("models_changed", map[string]string{"model": "m1", "change": "loaded"})
	if ev := wait("models_changed"); !strings.Contains(string(ev.Data), `"m1"`) {
		t.Fatalf("models_changed = %s", ev.Data)
	}
	d.Log.Info("hello from the daemon", "model", "m1")
	if ev := wait("log"); !strings.Contains(string(ev.Data), "hello from the daemon") {
		t.Fatalf("log event = %s", ev.Data)
	}
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.C:
			if !ok {
				if s.Err() != nil {
					t.Fatalf("cancel should close cleanly, got %v", s.Err())
				}
				return
			}
		case <-deadline:
			t.Fatal("stream did not close after cancel")
		}
	}
}

func TestEventsRejectsBadToken(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, "wrong")
	_, err := c.Events(context.Background(), false)
	var ae *client.APIError
	if !errors.As(err, &ae) || ae.Status != 401 {
		t.Fatalf("err = %v", err)
	}
}

func TestFollowStopsOnErrStop(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, d.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n := 0
	err := c.Follow(ctx, false, func(ev client.Event) error {
		n++
		return client.ErrStop
	})
	if !errors.Is(err, client.ErrStop) || n != 1 {
		t.Fatalf("err = %v, events = %d", err, n)
	}
}

func TestLogsReadsTheRing(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, d.Token)
	d.Log.Warn("disk almost full", "free_gb", 2)
	logs, err := c.Logs(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range logs {
		if e.Message == "disk almost full" && e.Level == "WARN" && e.Attrs != nil && strings.Contains(*e.Attrs, "free_gb=2") {
			found = true
		}
	}
	if !found {
		t.Fatalf("logs = %+v", logs)
	}
}

func TestChatStreamDeltasAndUsage(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ReasoningTokens: 3})
	c := newClient(t, d, d.Token)
	var content, reasoning strings.Builder
	seed := uint64(7)
	res, err := c.ChatStream(context.Background(), client.ChatRequest{
		Model:     clienttest.Model,
		Messages:  []client.Message{{Role: "user", Content: "hello mesh"}},
		MaxTokens: 12,
		Seed:      &seed,
	}, func(dl client.Delta) error {
		content.WriteString(dl.Content)
		reasoning.WriteString(dl.Reasoning)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if content.String() == "" || content.String() != res.Content {
		t.Errorf("content deltas %q vs result %q", content.String(), res.Content)
	}
	if reasoning.String() == "" || reasoning.String() != res.Reasoning {
		t.Errorf("reasoning deltas %q vs result %q", reasoning.String(), res.Reasoning)
	}
	if res.Usage.CompletionTokens == 0 || res.Usage.TotalTokens == 0 {
		t.Errorf("usage frame missing: %+v", res.Usage)
	}
	if res.TokensPerSec() <= 0 || res.Model != clienttest.Model || res.FinishReason == "" {
		t.Errorf("result = %+v", res)
	}
}

func TestChatNonStreaming(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	c := newClient(t, d, d.Token)
	res, err := c.Chat(context.Background(), client.ChatRequest{
		Messages: []client.Message{{Role: "user", Content: "hi"}}, MaxTokens: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == "" || res.Usage.CompletionTokens == 0 || res.Model != clienttest.Model {
		t.Fatalf("result = %+v", res)
	}
}

func TestChatGovernorRefusal(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ServePolicy: "idle-only"})
	c := newClient(t, d, d.Token)
	req := client.ChatRequest{Messages: []client.Message{{Role: "user", Content: "hi"}}}
	for name, call := range map[string]func() error{
		"chat":   func() error { _, err := c.Chat(context.Background(), req); return err },
		"stream": func() error { _, err := c.ChatStream(context.Background(), req, func(client.Delta) error { return nil }); return err },
	} {
		err := call()
		var ae *client.APIError
		if !errors.As(err, &ae) || !ae.NotServing() {
			t.Fatalf("%s: err = %v, want a 503", name, err)
		}
		if r := client.Remedy(err); !strings.Contains(r, "tera limits --serve always") {
			t.Errorf("%s: remedy = %q", name, r)
		}
	}
}

func TestReadSSEParsesEvents(t *testing.T) {
	body := ": keepalive\n" +
		"event: status\ndata: {\"state\":\"serving\"}\n\n" +
		"event: log\ndata: {\"level\":\"INFO\",\n" +
		"data: \"message\":\"hi\"}\n\n" +
		"event: model_progress\ndata: {\"model\":\"m\"}\n"
	var got []client.Event
	if err := client.ReadSSE(strings.NewReader(body), func(ev client.Event) error {
		got = append(got, ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(got), got)
	}
	if got[0].Name != "status" || string(got[0].Data) != `{"state":"serving"}` {
		t.Errorf("first = %+v", got[0])
	}
	if got[1].Name != "log" || string(got[1].Data) != "{\"level\":\"INFO\",\n\"message\":\"hi\"}" {
		t.Errorf("multi-line data = %q", got[1].Data)
	}
	if got[2].Name != "model_progress" {
		t.Errorf("trailing event without blank line dropped: %+v", got[2])
	}
}

func TestReadSSEStopsOnCallbackError(t *testing.T) {
	body := "event: a\ndata: 1\n\nevent: b\ndata: 2\n\n"
	n := 0
	err := client.ReadSSE(strings.NewReader(body), func(client.Event) error {
		n++
		return client.ErrStop
	})
	if !errors.Is(err, client.ErrStop) || n != 1 {
		t.Fatalf("err = %v, callbacks = %d", err, n)
	}
}

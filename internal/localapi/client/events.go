package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Event is one server-sent event from GET /api/v1/events. Name is the
// event type documented in api/openapi.yaml (status, model_progress,
// models_changed, model_assignment, activity, update_available, log);
// Data is its JSON payload.
type Event struct {
	Name string
	Data json.RawMessage
}

// ErrStop is returned by a Follow callback to end the stream cleanly;
// Follow returns it untouched so callers can tell "done" from "lost".
var ErrStop = errors.New("stop following")

// Follow opens the daemon's event stream (`?logs=1` when logs is set) and
// calls fn for every event until fn returns an error, ctx is cancelled, or
// the daemon closes the stream. The bearer token goes in the Authorization
// header — only EventSource needs the `?token=` form. Matches handleEvents
// in internal/localapi/api.go.
func (c *Client) Follow(ctx context.Context, logs bool, fn func(Event) error) error {
	body, err := c.openEvents(ctx, logs)
	if err != nil {
		return err
	}
	defer body.Close()
	err = ReadSSE(body, fn)
	if ctx.Err() != nil && !errors.Is(err, ErrStop) {
		return ctx.Err()
	}
	return err
}

// Stream is an open event stream: read C until it closes, then Err says
// why (nil when the daemon ended it or ctx was cancelled).
type Stream struct {
	C <-chan Event

	mu  sync.Mutex
	err error
}

// Err is the terminal error once C is closed.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Events opens the event stream and delivers events on a channel. The
// connection (and its auth) is established before Events returns, so a
// 401 or an unreachable daemon is an immediate error; C closes when the
// stream ends for any reason (ctx cancelled, daemon gone). Cancel ctx to
// stop it.
func (c *Client) Events(ctx context.Context, logs bool) (*Stream, error) {
	body, err := c.openEvents(ctx, logs)
	if err != nil {
		return nil, err
	}
	ch := make(chan Event, 16)
	s := &Stream{C: ch}
	go func() {
		defer close(ch)
		defer body.Close()
		rerr := ReadSSE(body, func(ev Event) error {
			select {
			case ch <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if ctx.Err() != nil {
			rerr = nil
		}
		s.mu.Lock()
		s.err = rerr
		s.mu.Unlock()
	}()
	return s, nil
}

func (c *Client) openEvents(ctx context.Context, logs bool) (io.ReadCloser, error) {
	path := "/api/v1/events"
	if logs {
		path += "?logs=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	_ = c.bearer(ctx, req)
	resp, err := doer{c.long, c.Base}.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, c.Check(resp.StatusCode, raw)
	}
	return resp.Body, nil
}

// ReadSSE parses a text/event-stream body: `event:`/`data:` lines, blank
// line dispatches, `:` lines are comments. Multi-line data is joined with
// newlines per the spec. Events without a name (the OpenAI stream) are
// delivered with Name == "".
func ReadSSE(r io.Reader, fn func(Event) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var name string
	var data []string
	flush := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		ev := Event{Name: name, Data: json.RawMessage(strings.Join(data, "\n"))}
		name, data = "", nil
		return fn(ev)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			// comment / keepalive
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return sc.Err()
}

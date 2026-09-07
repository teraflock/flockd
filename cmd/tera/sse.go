package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// sseEvent is one server-sent event from GET /api/v1/events.
type sseEvent struct {
	Name string
	Data json.RawMessage
}

// errStopFollow is returned by a follow callback to end the stream cleanly.
var errStopFollow = errors.New("stop following")

// follow opens the daemon's event stream (`?logs=1` when logs is set) and
// calls fn for every event until fn returns an error, ctx is cancelled, or
// the daemon closes the stream. Returns errStopFollow untouched so callers
// can tell "done" from "lost". The bearer token goes in the Authorization
// header — only EventSource needs the `?token=` form.
//
// This is the minimal SSE reader the CLI needs today; flockd#37/#38 hoist
// the client (including this) into internal/localapi/client.
func (c *apiClient) follow(ctx context.Context, logs bool, fn func(sseEvent) error) error {
	path := "/api/v1/events"
	if logs {
		path += "?logs=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.long.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("cannot reach flockd at %s (is it running? try `tera up` or `flockd --standalone`): %w", c.base, err)
	}
	defer resp.Body.Close()
	if err := c.httpError(resp); err != nil {
		return err
	}
	err = readSSE(resp.Body, fn)
	if ctx.Err() != nil && !errors.Is(err, errStopFollow) {
		return ctx.Err()
	}
	return err
}

// readSSE parses a text/event-stream body: `event:`/`data:` lines, blank
// line dispatches, `:` lines are comments. Multi-line data is joined with
// newlines per the spec.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var name string
	var data []string
	flush := func() error {
		if name == "" && len(data) == 0 {
			return nil
		}
		ev := sseEvent{Name: name, Data: json.RawMessage(strings.Join(data, "\n"))}
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

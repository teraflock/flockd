package main

import (
	"errors"
	"strings"
	"testing"
)

func TestReadSSEParsesEvents(t *testing.T) {
	body := ": keepalive\n" +
		"event: status\ndata: {\"state\":\"serving\"}\n\n" +
		"event: log\ndata: {\"level\":\"INFO\",\n" +
		"data: \"message\":\"hi\"}\n\n" +
		"event: model_progress\ndata: {\"model\":\"m\"}\n"
	var got []sseEvent
	if err := readSSE(strings.NewReader(body), func(ev sseEvent) error {
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
	err := readSSE(strings.NewReader(body), func(sseEvent) error {
		n++
		return errStopFollow
	})
	if !errors.Is(err, errStopFollow) || n != 1 {
		t.Fatalf("err = %v, callbacks = %d", err, n)
	}
}

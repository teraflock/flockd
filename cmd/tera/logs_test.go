package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/localapi/gen"
)

func strp(s string) *string { return &s }

func TestLevelRank(t *testing.T) {
	for in, want := range map[string]int{"debug": 0, "INFO": 1, "warn": 2, "WARNING": 2, "error": 3, "": 1} {
		if got, _ := levelRank(in); got != want {
			t.Errorf("levelRank(%q) = %d, want %d", in, got, want)
		}
	}
	if _, ok := levelRank("loud"); ok {
		t.Error("unknown level accepted")
	}
}

func TestPrintLogFiltersBelowLevel(t *testing.T) {
	var out bytes.Buffer
	e := gen.LogEntry{Time: time.Now(), Level: "INFO", Message: "hello", Attrs: strp("model=m1")}
	printLog(&out, e, 2, false)
	if out.Len() != 0 {
		t.Fatalf("INFO printed under --level warn: %q", out.String())
	}
	printLog(&out, e, 1, false)
	if s := out.String(); !strings.Contains(s, "INFO") || !strings.Contains(s, "hello model=m1") {
		t.Fatalf("line = %q", s)
	}
	out.Reset()
	printLog(&out, e, 1, true)
	if s := out.String(); !strings.HasPrefix(s, `{"time":`) || !strings.Contains(s, `"attrs":"model=m1"`) {
		t.Fatalf("json = %q", s)
	}
}

func TestFormatLogTruncatesForPane(t *testing.T) {
	e := gen.LogEntry{Time: time.Now(), Level: "WARN", Message: strings.Repeat("x", 100)}
	s := formatLog(e, 40)
	if !strings.HasSuffix(s, "…") && !strings.Contains(s, "…") {
		t.Fatalf("not truncated: %q", s)
	}
	if strings.Count(s, "x") > 30 {
		t.Fatalf("too long for width 40: %q", s)
	}
}

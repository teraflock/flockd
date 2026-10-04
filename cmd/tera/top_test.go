package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/localapi/gen"
)

func i64(v int64) *int64 { return &v }

func TestNowLine(t *testing.T) {
	a := gen.NodeActivity{
		Models: []gen.ModelActivity{
			{Model: "laya-q8_0", Kind: gen.ModelActivityKindDecision, Inflight: 2, InflightByKind: map[string]int{"decision": 2}},
			{Model: "llama-3.2-3b", Kind: gen.ModelActivityKindChat, InflightByKind: map[string]int{}, IdleSeconds: i64(40)},
			{Model: "mixed", Kind: gen.ModelActivityKindChat, Inflight: 3, InflightByKind: map[string]int{"completion": 1, "chat": 2}},
		},
		Operations: []gen.ModelOperation{
			{Model: "kev-4b-q4_k_m", Op: gen.Downloading, ReceivedBytes: i64(250), TotalBytes: i64(1000)},
		},
	}
	want := "laya-q8_0: 2 decision in flight · llama-3.2-3b: idle 40s · mixed: 2 chat, 1 completion in flight · kev-4b-q4_k_m: downloading 25%"
	if got := nowLine(a); got != want {
		t.Fatalf("nowLine:\n got %s\nwant %s", got, want)
	}
	if got := nowLine(gen.NodeActivity{}); got != "nothing loaded" {
		t.Fatalf("empty = %q", got)
	}
	st := time.Now().Add(-5 * time.Second)
	if got := nowLine(gen.NodeActivity{Operations: []gen.ModelOperation{{Model: "m", Op: gen.Loading, StartedAt: &st}}}); !strings.HasPrefix(got, "m: loading 5s") && !strings.HasPrefix(got, "m: loading 4s") {
		t.Fatalf("loading = %q", got)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		850 * time.Millisecond: "850ms", 12 * time.Second: "12s", 185 * time.Second: "3m05s", 130 * time.Minute: "2h10m", -time.Second: "0s",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestRenderTop(t *testing.T) {
	now := time.Now()
	ra := gen.RequestActivity{
		Now:    now,
		Models: []gen.ModelActivity{{Model: "laya-q8_0", Inflight: 1, InflightByKind: map[string]int{"decision": 1}}},
		Inflight: []gen.InflightRequest{{Id: "r9", Model: "laya-q8_0", Kind: gen.RequestKindDecision, Origin: gen.Mesh,
			StartedAt: now, ElapsedMs: 120}},
		Recent: []gen.FinishedRequest{
			{Id: "r8", Model: "llama", Kind: gen.RequestKindChat, Origin: gen.Local, StartedAt: now, DurationMs: 2300,
				PromptTokens: 12, CompletionTokens: 64, Outcome: gen.RequestOutcomeOk},
			{Id: "r7", Model: "laya-q8_0", Kind: gen.RequestKindDecision, Origin: gen.Challenge, StartedAt: now, DurationMs: 30,
				PromptTokens: 172, Outcome: gen.RequestOutcomeInvalidInput},
		},
	}
	var b bytes.Buffer
	renderTop(&b, "", ra)
	out := b.String()
	for _, want := range []string{"laya-q8_0: 1 decision in flight", "IN FLIGHT (1)", "120ms", "mesh", "RECENT (2)", "2s", "invalid_input", "challenge", "llama"} {
		if !strings.Contains(out, want) {
			t.Errorf("frame lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	renderTop(&b, "", gen.RequestActivity{Now: now})
	if out := b.String(); !strings.Contains(out, "nothing running") || !strings.Contains(out, "no requests yet") || !strings.Contains(out, "nothing loaded") {
		t.Fatalf("empty frame:\n%s", out)
	}
}

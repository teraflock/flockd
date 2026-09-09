package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/client/clienttest"
)

func chatClient(t *testing.T, d *clienttest.Daemon) *client.Client {
	t.Helper()
	c, err := client.FromResolved(client.Resolved{Base: d.URL, Token: d.Token, TokenSource: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestChatOnceStreamsAndFooters(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ReasoningTokens: 2})
	var out bytes.Buffer
	err := chatOnce(context.Background(), chatClient(t, d), chatOpts{model: clienttest.Model, maxTokens: 10, temperature: -1}, "hello mesh", &out)
	if err != nil {
		t.Fatalf("chatOnce: %v\n%s", err, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "tok/s") || !strings.Contains(s, clienttest.Model) {
		t.Errorf("footer missing:\n%s", s)
	}
	// Content and reasoning both print; reasoning ends before the answer.
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) < 3 {
		t.Fatalf("want reasoning, answer and footer lines:\n%s", s)
	}
}

func TestChatOnceHidesReasoning(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ReasoningTokens: 2})
	var out bytes.Buffer
	if err := chatOnce(context.Background(), chatClient(t, d), chatOpts{maxTokens: 10, temperature: -1, hideReasoning: true}, "hi", &out); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 2 {
		t.Fatalf("want answer + footer only:\n%s", out.String())
	}
}

func TestChatOnceGovernorRefusalCarriesRemedy(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ServePolicy: "idle-only"})
	var out bytes.Buffer
	err := chatOnce(context.Background(), chatClient(t, d), chatOpts{temperature: -1}, "hi", &out)
	if err == nil {
		t.Fatal("expected the 503 to fail the one-shot")
	}
	var ae *client.APIError
	if !errors.As(err, &ae) || !ae.NotServing() {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "tera limits --serve always") {
		t.Fatalf("remedy missing from error:\n%v", err)
	}
}

func TestChatCommandOneShotFromStdin(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	flagAPI, flagToken = d.URL, d.Token
	t.Cleanup(func() { flagAPI, flagToken = "http://127.0.0.1:7777", "" })
	c := cmdChat()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetIn(strings.NewReader("hi from a pipe\n"))
	c.SetArgs([]string{"-m", clienttest.Model, "--max-tokens", "6"})
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "tok/s") || strings.Contains(out.String(), "tera chat") {
		t.Fatalf("one-shot should answer without the REPL banner:\n%s", out.String())
	}
}

func TestChatCommandRefusesEmptyPipe(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	flagAPI, flagToken = d.URL, d.Token
	t.Cleanup(func() { flagAPI, flagToken = "http://127.0.0.1:7777", "" })
	c := cmdChat()
	c.SetOut(&bytes.Buffer{})
	c.SetIn(strings.NewReader("  \n"))
	c.SetArgs(nil)
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "nothing to send") {
		t.Fatalf("err = %v", err)
	}
}

func TestChatREPLCommands(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	var out bytes.Buffer
	in := strings.NewReader(strings.Join([]string{
		"/model",
		"/model " + clienttest.Model,
		"/system be terse",
		"/system",
		"/bogus",
		"first question",
		"/reset",
		"second question",
		"/quit",
		"never sent",
	}, "\n") + "\n")
	if err := chatREPL(context.Background(), chatClient(t, d), chatOpts{maxTokens: 6, temperature: -1}, in, &out); err != nil {
		t.Fatalf("repl: %v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{"tera chat", "model: daemon default", "model: " + clienttest.Model, "system prompt set", "system: be terse", "unknown command", "conversation cleared"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	if n := strings.Count(s, "tok/s"); n != 2 {
		t.Errorf("want two answered turns, got %d:\n%s", n, s)
	}
	if strings.Contains(s, "never sent") {
		t.Errorf("/quit did not stop the loop:\n%s", s)
	}
}

func TestChatREPLKeepsGoingAfterRefusal(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ServePolicy: "idle-only"})
	var out bytes.Buffer
	in := strings.NewReader("hello\n")
	// EOF after the refusal: the loop reports the error and exits cleanly.
	if err := chatREPL(context.Background(), chatClient(t, d), chatOpts{temperature: -1}, in, &out); err != nil {
		t.Fatalf("repl: %v", err)
	}
	if !strings.Contains(out.String(), "tera limits --serve always") {
		t.Fatalf("remedy not shown:\n%s", out.String())
	}
}

func TestChatFooter(t *testing.T) {
	res := client.ChatResult{Model: "m", FinishReason: "length"}
	res.Usage.CompletionTokens = 12
	f := chatFooter(res)
	if !strings.HasPrefix(f, "12 tok, ") || !strings.Contains(f, "· m") || !strings.Contains(f, "max_tokens") {
		t.Fatalf("footer = %q", f)
	}
}

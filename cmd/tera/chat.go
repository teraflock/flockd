package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/teraflock/flockd/internal/localapi/client"
)

// tera chat is a minimal streaming REPL against the node's own
// /v1/chat/completions, for SSH boxes and headless nodes (flockd#43). Text
// in, streamed text out, chain-of-thought dimmed, a tok/s footer. It is not
// the desktop chat: the conversation lives in memory for the session and
// is never written to disk (the desktop owns history, plan 10).
func cmdChat() *cobra.Command {
	var o chatOpts
	c := &cobra.Command{
		Use:   "chat [prompt...]",
		Short: "Chat with a model on this node (streaming REPL; one-shot with a prompt or piped stdin)",
		Long: `Chat with a model served by this node over the local OpenAI-compatible
endpoint. With no prompt and a terminal on stdin it runs a REPL:

  /model <id>     switch model (empty = the daemon's default)
  /system <text>  set the system prompt (empty shows it)
  /reset          forget the conversation
  /quit           leave (ctrl-d works too)

With a prompt argument, or with stdin piped, it answers once and exits —
scriptable: echo "summarise this" | tera chat -m <id>.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			in := cmd.InOrStdin()
			prompt := strings.TrimSpace(strings.Join(args, " "))
			interactive := prompt == "" && stdinIsTerminal(in)
			if prompt == "" && !interactive {
				raw, err := io.ReadAll(in)
				if err != nil {
					return err
				}
				prompt = strings.TrimSpace(string(raw))
				if prompt == "" {
					return errors.New("nothing to send: pass a prompt, pipe one on stdin, or run in a terminal for the REPL")
				}
			}
			if !interactive {
				return chatOnce(cmd.Context(), cl, o, prompt, cmd.OutOrStdout())
			}
			return chatREPL(cmd.Context(), cl, o, in, cmd.OutOrStdout())
		},
	}
	c.Flags().StringVarP(&o.model, "model", "m", "", "model id (default: the daemon's default model)")
	c.Flags().StringVar(&o.system, "system", "", "system prompt")
	c.Flags().IntVar(&o.maxTokens, "max-tokens", 0, "cap the reply length (0 = daemon default)")
	c.Flags().Float64Var(&o.temperature, "temperature", -1, "sampling temperature (-1 = daemon default)")
	c.Flags().BoolVar(&o.hideReasoning, "no-reasoning", false, "hide reasoning_content (chain-of-thought) instead of printing it dimmed")
	return c
}

type chatOpts struct {
	model, system string
	maxTokens     int
	temperature   float64
	hideReasoning bool
}

// stdinIsTerminal reports whether in is the process's terminal stdin
// (a cobra-injected reader in tests is never one).
func stdinIsTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && isTerminal(f)
}

// chatSession is the in-memory conversation.
type chatSession struct {
	cl   *client.Client
	o    chatOpts
	msgs []client.Message
}

func (s *chatSession) reset() {
	s.msgs = nil
	if s.o.system != "" {
		s.msgs = []client.Message{{Role: "system", Content: s.o.system}}
	}
}

// turn sends the user's text, streams the reply to out, and keeps the
// assistant's answer in the conversation. The footer carries the usage
// frame; a governor refusal comes back with its remedy so the caller can
// print it once (REPL) or fail with it (one-shot).
func (s *chatSession) turn(ctx context.Context, out io.Writer, text string) error {
	s.msgs = append(s.msgs, client.Message{Role: "user", Content: text})
	req := client.ChatRequest{Model: s.o.model, Messages: s.msgs, MaxTokens: s.o.maxTokens}
	if s.o.temperature >= 0 {
		t := s.o.temperature
		req.Temperature = &t
	}
	inReasoning := false
	res, err := s.cl.ChatStream(ctx, req, func(d client.Delta) error {
		if d.Reasoning != "" && !s.o.hideReasoning {
			inReasoning = true
			fmt.Fprint(out, styleDim.Render(d.Reasoning))
		}
		if d.Content != "" {
			if inReasoning {
				// Chain-of-thought ended: put the answer on its own line.
				fmt.Fprintln(out)
				inReasoning = false
			}
			fmt.Fprint(out, d.Content)
		}
		return nil
	})
	if err != nil {
		// Drop the failed user turn so a retry does not double it.
		s.msgs = s.msgs[:len(s.msgs)-1]
		if res.Content != "" {
			fmt.Fprintln(out)
		}
		return withRemedy(err)
	}
	if res.Content != "" || res.Reasoning != "" {
		fmt.Fprintln(out)
	}
	s.msgs = append(s.msgs, client.Message{Role: "assistant", Content: res.Content})
	fmt.Fprintln(out, styleDim.Render(chatFooter(res)))
	return nil
}

// chatFooter is the `N tok, X tok/s` line from the usage frame.
func chatFooter(res client.ChatResult) string {
	f := fmt.Sprintf("%d tok, %.1f tok/s", res.Usage.CompletionTokens, res.TokensPerSec())
	if res.Model != "" {
		f += " · " + res.Model
	}
	if res.FinishReason == "length" {
		f += " · cut off at max_tokens"
	}
	return f
}

// withRemedy appends the client's one-line fix (start the daemon, serve
// now, pull the model) under the error.
func withRemedy(err error) error {
	if r := client.Remedy(err); r != "" {
		return fmt.Errorf("%w\n  %s", err, r)
	}
	return err
}

func chatOnce(ctx context.Context, cl *client.Client, o chatOpts, prompt string, out io.Writer) error {
	s := &chatSession{cl: cl, o: o}
	s.reset()
	return s.turn(ctx, out, prompt)
}

func chatREPL(ctx context.Context, cl *client.Client, o chatOpts, in io.Reader, out io.Writer) error {
	s := &chatSession{cl: cl, o: o}
	s.reset()
	model := o.model
	if model == "" {
		model = "daemon default"
	}
	fmt.Fprintln(out, styleTitle.Render("/// tera chat"), styleDim.Render(model+" · /model /system /reset /quit · ctrl-d to leave"))
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for {
		fmt.Fprint(out, styleTitle.Render("> "))
		if !sc.Scan() {
			fmt.Fprintln(out)
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "":
			continue
		case line == "/quit", line == "/exit", line == "/q":
			return nil
		case line == "/reset":
			s.reset()
			fmt.Fprintln(out, styleDim.Render("conversation cleared"))
			continue
		case strings.HasPrefix(line, "/model"):
			s.o.model = strings.TrimSpace(strings.TrimPrefix(line, "/model"))
			if s.o.model == "" {
				fmt.Fprintln(out, styleDim.Render("model: daemon default"))
			} else {
				fmt.Fprintln(out, styleDim.Render("model: "+s.o.model))
			}
			continue
		case strings.HasPrefix(line, "/system"):
			text := strings.TrimSpace(strings.TrimPrefix(line, "/system"))
			if text == "" {
				if s.o.system == "" {
					fmt.Fprintln(out, styleDim.Render("no system prompt"))
				} else {
					fmt.Fprintln(out, styleDim.Render("system: "+s.o.system))
				}
				continue
			}
			s.o.system = text
			s.reset()
			fmt.Fprintln(out, styleDim.Render("system prompt set; conversation cleared"))
			continue
		case strings.HasPrefix(line, "/"):
			fmt.Fprintln(out, styleWarn.Render("unknown command"), styleDim.Render("— /model /system /reset /quit"))
			continue
		}
		if err := s.turn(ctx, out, line); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(out, styleWarn.Render("error:"), err)
		}
	}
}

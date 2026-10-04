package llamacpp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/teraflock/flockd/internal/decision"
	"github.com/teraflock/flockd/internal/memory"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// Adapter implements runtime.Runtime by supervising llama-server and
// translating CompletionRequests to its OpenAI-compatible HTTP API.
type Adapter struct {
	Fetcher *Fetcher
	// Accels is the accelerator preference chain from
	// hardware.AccelPreference (cuda12 > rocm > vulkan > cpu, flockd#21):
	// the Fetcher serves the first lane the manifest publishes. Accel is
	// the head of that chain; either may be set, Accels wins.
	Accels []string
	Accel  string
	// VRAMMB is the detected GPU memory (system RAM on Apple Silicon);
	// with budget.max_vram_percent it bounds GPU offload. 0 = unknown.
	VRAMMB uint64
	Log    *slog.Logger
	// ContextLength override (0 = model/catalog default).
	ContextLength int
	// MaxContext caps the window passed as --ctx-size (0 = no cap); see
	// config.Runtime.MaxContext.
	MaxContext int

	mu       sync.Mutex
	selected string // accel of the build the last Load resolved
	logged   bool   // "runtime accel selected" printed once
}

// gpuAccels are backends where llama-server offloads layers to a device.
var gpuAccels = map[string]bool{"metal": true, "cuda12": true, "rocm": true, "vulkan": true}

// chain is the accelerator preference to hand the Fetcher.
func (a *Adapter) chain() []string {
	if len(a.Accels) > 0 {
		return a.Accels
	}
	if a.Accel != "" {
		return []string{a.Accel}
	}
	return nil
}

// preferred is the head of the chain: what hardware detection wanted.
func (a *Adapter) preferred() string {
	if c := a.chain(); len(c) > 0 {
		return c[0]
	}
	return ""
}

// SelectedAccel is the backend of the llama-server build actually running
// — "vulkan" on an AMD box whose manifest has no rocm lane — so status
// surfaces can show it; before the first Load it is the preferred accel.
// A configured local binary reports the preferred accel too (its backend
// is unknown to the daemon).
func (a *Adapter) SelectedAccel() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.selected != "" {
		return a.selected
	}
	return a.preferred()
}

// Prepare resolves (fetching and verifying if needed) the llama-server
// build for this machine without loading a model, and returns its build
// id. Boot calls it before the tunnel connects so the session Hello
// carries runtime_build_id — fingerprint challenges key on it — even
// while the first model is still on its way (flockd#48). Cached after
// the first boot.
func (a *Adapter) Prepare(ctx context.Context) (string, error) {
	sel, err := a.Fetcher.EnsureSelection(ctx, a.chain()...)
	if err != nil {
		return "", err
	}
	a.record(sel)
	return sel.BuildID, nil
}

// record remembers the Fetcher's pick and logs it the first time (and
// whenever it changes, e.g. a manifest that grew a lane).
func (a *Adapter) record(sel Selection) string {
	accel := sel.Accel
	if accel == "" {
		accel = a.preferred()
	}
	a.mu.Lock()
	changed := !a.logged || a.selected != accel
	a.selected, a.logged = accel, true
	a.mu.Unlock()
	if changed {
		a.logger().Info("runtime accel selected",
			"accel", accel, "preferred", a.preferred(), "chain", strings.Join(a.chain(), ">"),
			"reason", sel.Reason, "runtime_build", sel.BuildID)
	}
	return accel
}

// gpuLayers decides --n-gpu-layers. Full offload when the model fits the
// VRAM budget (the common case; llama.cpp clamps 999 to the real layer
// count). When it doesn't fit, offload a proportional slice — the layer
// count is approximated at 64 until GGUF block_count parsing lands
// (TODO(gguf)), which errs toward offloading slightly less than possible
// rather than blowing the operator's budget.
func (a *Adapter) gpuLayers(m rt.ModelSpec, res rt.ResourceBudget) int {
	if !gpuAccels[a.SelectedAccel()] {
		return 0
	}
	if res.MaxVRAMPercent <= 0 || a.VRAMMB == 0 {
		return 999
	}
	// The whole artifact: a sharded model's Path is only its first part.
	size := m.SizeBytes
	if size == 0 {
		fi, err := os.Stat(m.Path)
		if err != nil {
			return 999
		}
		size = fi.Size()
	}
	budgetMB := float64(a.VRAMMB) * float64(res.MaxVRAMPercent) / 100
	// Weights + KV cache + compute buffers: ~1.15x file size is a safe
	// working floor for quantized GGUFs at moderate context.
	needMB := float64(size) / (1 << 20) * 1.15
	if needMB <= budgetMB {
		return 999
	}
	layers := int(budgetMB / needMB * 64)
	if layers < 0 {
		layers = 0
	}
	return layers
}

func (a *Adapter) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Load fetches/verifies the pinned llama-server build, launches it on an
// ephemeral loopback port with the model, health-gates it and returns the
// serving Instance. A sharded model is passed as its first part
// (<name>-00001-of-0000N.gguf): llama-server opens the siblings itself.
func (a *Adapter) Load(ctx context.Context, m rt.ModelSpec, res rt.ResourceBudget) (rt.Instance, error) {
	sel, err := a.Fetcher.EnsureSelection(ctx, a.chain()...)
	if err != nil {
		return nil, err
	}
	a.record(sel)
	bin, buildID := sel.Path, sel.BuildID
	if err := checkBuildSupports(buildID, m); err != nil {
		return nil, err
	}
	port, err := ephemeralPort()
	if err != nil {
		return nil, err
	}

	args := a.serverArgs(m, res, port)

	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	sup := newSupervisor(bin, args, url, a.logger().With("model", m.ID, "runtime_build", buildID))
	if err := sup.start(ctx); err != nil {
		return nil, err
	}
	return &instance{
		batchIsContext: m.Decision && slices.Contains(args, "--ubatch-size"),

		spec:    m,
		buildID: buildID,
		sup:     sup,
		baseURL: url,
		client:  &http.Client{}, // no timeout: streams are long-lived; ctx governs
	}, nil
}

// serverArgs is llama-server's command line for a model.
func (a *Adapter) serverArgs(m rt.ModelSpec, res rt.ResourceBudget, port int) []string {
	// Admission plans slots and total context together from the memory
	// budget (memory.PlanContext, flockd#46); a caller that did not plan
	// (tests, tera's preflight) gets the pre-plan behaviour.
	ctxLen := memory.ResolveContext(a.ContextLength, m.ContextLength, a.MaxContext)
	if res.ContextTokens > 0 {
		ctxLen = res.ContextTokens
	}
	slots := max(res.MaxConcurrent, 1)
	if res.Slots > 0 {
		slots = res.Slots
	}
	args := []string{
		"-m", m.Path,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--parallel", strconv.Itoa(slots),
		"--no-webui",
		// Reasoning models: llama-server splits chain-of-thought out of
		// `content` into `reasoning_content` deltas (the DeepSeek wire
		// format OpenAI-style clients understand). parseSSE relays BOTH
		// streams as Chunks — reasoning in Chunk.Reasoning, the answer in
		// Chunk.Delta, each counted in TokenCount — so a generation that
		// stops mid-thought still returns every token: the customer is
		// billed for them, and canary comparison (SPEC §2.2) diffs the
		// whole stream. `<think>` tags therefore no longer appear inline
		// for models llama.cpp knows how to parse; models it cannot parse
		// still arrive as plain content and clients keep their splitter.
		"--reasoning-format", "deepseek",
	}
	if ctxLen > 0 {
		args = append(args, "--ctx-size", strconv.Itoa(ctxLen))
	}
	switch {
	case m.Decision:
		// Typed decision model (/v1/systemone). No --embeddings: the
		// server recognises the architecture and enables what it needs
		// itself. Laya-family (Laya, Julia) and Clef evaluate a whole
		// prompt in ONE batch, so it must fit --ubatch-size; llama-server's
		// default (512) answers any longer prompt with a 500. The longest
		// prompt a slot can take is its share of --ctx-size (n_ctx_slot =
		// ctx-size / parallel), so that is the batch size: enough for
		// every admissible prompt, without sizing the batch buffers for
		// the context of all slots together. Causal decision models that
		// take a prompt in several batches (Kev) keep the defaults.
		if ctxLen > 0 && decisionNeedsWholeBatch(m) {
			batch := strconv.Itoa(decisionBatch(ctxLen, slots))
			args = append(args, "--batch-size", batch, "--ubatch-size", batch)
		}
	case m.Embeddings:
		args = append(args, "--embeddings")
	}
	if m.MmprojPath != "" {
		// Vision-language models ship their projector as a sidecar; without
		// it llama-server serves the text weights only.
		args = append(args, "--mmproj", m.MmprojPath)
	}
	// Offload only when the build that will run is a GPU build: a chain
	// that fell through to cpu-avx2 must not pass --n-gpu-layers.
	if gpuAccels[a.SelectedAccel()] {
		ngl := a.gpuLayers(m, res)
		args = append(args, "--n-gpu-layers", strconv.Itoa(ngl))
		if ngl < 999 {
			a.logger().Warn("model exceeds VRAM budget: partial GPU offload",
				"model", m.ID, "n_gpu_layers", ngl, "max_vram_percent", res.MaxVRAMPercent)
		}
	}

	return args
}

// multiBatchDecisionFamilies are the decision model families (catalog
// `family`) llama-server evaluates like any causal model, a batch at a
// time: they need no --ubatch-size and must not be given a context-sized
// one (Kev serves a 65,536-token window). Verified against llama.cpp
// b11382: Kev-4B answers a 3,011-token prompt at the default batch of
// 512, while Laya and Julia-1 answer anything over 512 tokens with a 500.
// A family not listed here gets the whole-prompt batch: over-allocating
// works, under-allocating does not.
var multiBatchDecisionFamilies = map[string]bool{"kev": true}

func decisionNeedsWholeBatch(m rt.ModelSpec) bool {
	return m.Decision && !multiBatchDecisionFamilies[strings.ToLower(m.Family)]
}

// decisionBatch is the batch size for a decision model: one slot's context
// (rounded up, so it is never below llama-server's n_ctx_slot).
func decisionBatch(ctxLen, slots int) int {
	slots = max(slots, 1)
	return (ctxLen + slots - 1) / slots
}

// MinDecisionTag is the first upstream llama.cpp build tag that serves
// decision models (POST /v1/systemone; ggml-org/llama.cpp#29818 and
// #29831 for Clef). The runtimes pin moves from b9892 to at least this.
const MinDecisionTag = 11382

// buildTag extracts the upstream build number from a teraflock/runtimes
// build id (`llamacpp-<tag>-<n>`, tag = `b<number>`: "llamacpp-b9892-4"
// -> 9892). ok is false for anything else — a configured local binary
// ("local-binary"), the mock, a format this daemon does not know.
func buildTag(buildID string) (tag int, ok bool) {
	rest, found := strings.CutPrefix(buildID, "llamacpp-b")
	if !found {
		return 0, false
	}
	digits, _, found := strings.Cut(rest, "-")
	if !found || digits == "" {
		return 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		return 0, false
	}
	return n, true
}

// checkBuildSupports refuses a model the given runtime build cannot
// serve. Today that is one rule: a decision model needs llama.cpp >=
// b11382. A build id whose tag cannot be read (an operator's own
// llama_server_path) is not refused: the daemon cannot know, and the
// load itself fails on a binary that is too old (unknown architecture).
func checkBuildSupports(buildID string, m rt.ModelSpec) error {
	if !m.Decision {
		return nil
	}
	if tag, ok := buildTag(buildID); ok && tag < MinDecisionTag {
		return fmt.Errorf("%w: %s is a decision model and needs llama.cpp b%d or newer, this node runs %s",
			rt.ErrRuntimeTooOld, m.ID, MinDecisionTag, buildID)
	}
	return nil
}

// SupportsModel implements runtime.ModelSupporter: it resolves the
// runtime build this node runs (cached after boot) and reports whether it
// can serve m, so a decision model is refused before anything is
// downloaded when the build predates decision support.
func (a *Adapter) SupportsModel(ctx context.Context, m rt.ModelSpec) error {
	if !m.Decision {
		return nil
	}
	sel, err := a.Fetcher.EnsureSelection(ctx, a.chain()...)
	if err != nil {
		return err
	}
	return checkBuildSupports(sel.BuildID, m)
}

type instance struct {
	// batchIsContext: a decision model started with --ubatch-size equal
	// to a slot's context (see overBatch).
	batchIsContext bool

	spec    rt.ModelSpec
	buildID string
	sup     *supervisor
	baseURL string
	client  *http.Client
}

func (i *instance) Complete(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	switch req.Kind {
	case rt.KindEmbedding:
		return i.embed(ctx, req)
	case rt.KindDecision:
		return i.decide(ctx, req)
	case rt.KindChat, rt.KindCompletion:
		return i.generate(ctx, req)
	default:
		return nil, fmt.Errorf("llamacpp: unsupported request kind %d", req.Kind)
	}
}

// ---- OpenAI-compatible wire shapes (subset llama-server implements) ----

type oaChatRequest struct {
	Model            string       `json:"model"`
	Messages         []rt.Message `json:"messages,omitempty"`
	Prompt           string       `json:"prompt,omitempty"`
	Stream           bool         `json:"stream"`
	Seed             *uint64      `json:"seed,omitempty"`
	Temperature      *float64     `json:"temperature,omitempty"`
	TopP             *float64     `json:"top_p,omitempty"`
	MaxTokens        int          `json:"max_tokens,omitempty"`
	Stop             []string     `json:"stop,omitempty"`
	FrequencyPenalty *float64     `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64     `json:"presence_penalty,omitempty"`
	StreamOptions    *oaStreamOpt `json:"stream_options,omitempty"`
}

type oaStreamOpt struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"delta"`
		Text         string  `json:"text"` // /v1/completions stream
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (i *instance) generate(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	body := oaChatRequest{
		Model:     i.spec.ID,
		Stream:    true,
		Seed:      &req.Params.Seed,
		MaxTokens: req.Params.MaxTokens,
		Stop:      req.Params.Stop,
		StreamOptions: &oaStreamOpt{
			IncludeUsage: true,
		},
	}
	body.Temperature = &req.Params.Temperature
	if req.Params.TopP > 0 {
		body.TopP = &req.Params.TopP
	}
	if req.Params.FrequencyPenalty != 0 {
		body.FrequencyPenalty = &req.Params.FrequencyPenalty
	}
	if req.Params.PresencePenalty != 0 {
		body.PresencePenalty = &req.Params.PresencePenalty
	}

	path := "/v1/chat/completions"
	if req.Kind == rt.KindCompletion {
		path = "/v1/completions"
		body.Prompt = req.Prompt
	} else {
		body.Messages = req.Messages
	}

	genCtx, cancel := context.WithCancel(ctx)
	resp, err := i.postStream(genCtx, path, body)
	if err != nil {
		cancel()
		return nil, err
	}

	ch := make(chan rt.Chunk, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close()
		parseSSE(genCtx, resp, ch)
	}()
	return rt.NewChanStream(ch, cancel), nil
}

// parseSSE reads llama-server's OpenAI-style SSE stream into Chunks.
func parseSSE(ctx context.Context, resp *http.Response, ch chan<- rt.Chunk) {
	var usage *rt.Usage
	finish := ""
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var c oaStreamChunk
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			send(ctx, ch, rt.Chunk{Done: true, FinishReason: "error", Err: fmt.Sprintf("llamacpp: bad stream chunk: %v", err)})
			return
		}
		if c.Usage != nil {
			usage = &rt.Usage{PromptTokens: c.Usage.PromptTokens, CompletionTokens: c.Usage.CompletionTokens}
		}
		for _, choice := range c.Choices {
			delta := choice.Delta.Content
			if delta == "" {
				delta = choice.Text
			}
			// Content and reasoning are both relayed (see the
			// --reasoning-format note in Load): one chunk per SSE delta,
			// one token each, whichever field(s) it carries.
			if delta != "" || choice.Delta.ReasoningContent != "" {
				if !send(ctx, ch, rt.Chunk{Delta: delta, Reasoning: choice.Delta.ReasoningContent, TokenCount: 1}) {
					return
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		send(ctx, ch, rt.Chunk{Done: true, FinishReason: "error", Err: fmt.Sprintf("llamacpp: stream read: %v", err)})
		return
	}
	if ctx.Err() != nil {
		finish = "cancelled"
	} else if finish == "" {
		finish = "stop"
	}
	send(ctx, ch, rt.Chunk{Done: true, FinishReason: finish, Usage: usage})
}

func send(ctx context.Context, ch chan<- rt.Chunk, c rt.Chunk) bool {
	select {
	case ch <- c:
		return true
	case <-ctx.Done():
		return false
	}
}

type oaEmbeddingRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type oaEmbeddingResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

func (i *instance) embed(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	resp, err := i.post(ctx, "/v1/embeddings", oaEmbeddingRequest{Model: i.spec.ID, Input: req.EmbeddingInput})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var er oaEmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("llamacpp: decode embeddings: %w", err)
	}
	vecs := make([][]float32, len(er.Data))
	for n, d := range er.Data {
		vecs[n] = d.Embedding
	}
	ch := make(chan rt.Chunk, 1)
	ch <- rt.Chunk{Done: true, Embeddings: vecs, Usage: &rt.Usage{PromptTokens: er.Usage.PromptTokens}}
	close(ch)
	return rt.NewChanStream(ch, nil), nil
}

// decide answers a typed decision request with llama-server's
// POST /v1/systemone. The body is built in list order (decision.RuntimeBody
// — never a marshalled map) and the answers come back in question order.
// A 400 is the runtime rejecting the input itself (too many options for
// the model, a prompt over the slot's context): *rt.InvalidInputError, so
// nothing retries it on another node. Any other status — 501 for a model
// that is not a decision model, 500s — is an ordinary error.
func (i *instance) decide(ctx context.Context, req rt.CompletionRequest) (rt.TokenStream, error) {
	if !i.spec.Decision {
		return nil, fmt.Errorf("llamacpp: %s is not a decision model", i.spec.ID)
	}
	body, err := decision.RuntimeBody(req.Decision)
	if err != nil {
		return nil, err
	}
	const path = "/v1/systemone"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, i.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llamacpp: build request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("llamacpp: %s: read response: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := runtimeErrorMessage(raw)
		if resp.StatusCode == http.StatusBadRequest {
			return nil, &rt.InvalidInputError{Msg: msg}
		}
		if i.overBatch(resp.StatusCode, msg) {
			// llama-server's advice ("increase the physical batch size")
			// is for whoever runs the server, not for the caller.
			return nil, &rt.InvalidInputError{Msg: "input is larger than the model's context: " + msg}
		}
		return nil, fmt.Errorf("llamacpp: %s: status %s: %s", path, resp.Status, msg)
	}
	answers, usage, err := decision.ParseRuntimeResponse(raw, req.Decision)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: %s: %w", path, err)
	}
	ch := make(chan rt.Chunk, 1)
	ch <- rt.Chunk{Done: true, Decision: answers, Usage: &rt.Usage{PromptTokens: usage.PromptTokens}}
	close(ch)
	return rt.NewChanStream(ch, nil), nil
}

// overBatch recognises llama-server's "input (N tokens) is too large to
// process. increase the physical batch size" — a 500, checked before the
// context-size check that answers 400. The adapter starts a decision
// model with the batch equal to a slot's context (serverArgs), so a
// prompt over the batch is a prompt over the context: the input is too
// long for this model on any node, not a server fault. Only trusted when
// this adapter set the batch (batchIsContext).
func (i *instance) overBatch(status int, msg string) bool {
	return i.batchIsContext && status == http.StatusInternalServerError &&
		strings.Contains(msg, "too large to process") && strings.Contains(msg, "batch size")
}

// runtimeErrorMessage extracts llama-server's `error.message`, falling
// back to the (truncated) body.
func runtimeErrorMessage(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 512 {
		msg = msg[:512]
	}
	return msg
}

// postStream is post for the one caller that hands the response to a
// goroutine (generate's SSE reader closes the body when the stream ends).
// bodyclose cannot follow a Close on another goroutine, so the hand-off
// is declared here instead of exempting every post caller.
//
//bodyclose:handled
func (i *instance) postStream(ctx context.Context, path string, body any) (*http.Response, error) {
	return i.post(ctx, path, body)
}

func (i *instance) post(ctx context.Context, path string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, i.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("llamacpp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := i.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		var msg bytes.Buffer
		_, _ = msg.ReadFrom(bufio.NewReaderSize(resp.Body, 4096))
		return nil, fmt.Errorf("llamacpp: %s: status %s: %s", path, resp.Status, strings.TrimSpace(msg.String()))
	}
	return resp, nil
}

type llamaHealthProps struct {
	SlotsIdle       int `json:"slots_idle"`
	SlotsProcessing int `json:"slots_processing"`
}

func (i *instance) Health(ctx context.Context) (rt.Stats, error) {
	st := rt.Stats{ModelID: i.spec.ID, Restarts: int(i.sup.restarts.Load())}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.baseURL+"/health", nil)
	if err != nil {
		return st, err
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return st, nil // unhealthy, not an error: supervisor is on it
	}
	defer resp.Body.Close()
	st.Healthy = resp.StatusCode == http.StatusOK
	var hp llamaHealthProps
	if json.NewDecoder(resp.Body).Decode(&hp) == nil {
		st.QueueDepth = hp.SlotsProcessing
	}
	// Physical footprint of the child (not RSS: mmap'd weights shared with
	// the page cache would be double counted). Feeds memory admission,
	// /api/v1/status and the heartbeat's ram_used_mb.
	if pid := i.sup.pid(); pid > 0 {
		if mb, err := memory.ProcessFootprintMB(pid); err == nil {
			st.MemUsedMB = mb
		}
	}
	return st, nil
}

func (i *instance) Shutdown(ctx context.Context) error {
	i.sup.stop(ctx)
	return nil
}

// RuntimeBuildID implements runtime.BuildIdentified: the pinned llama.cpp
// build this instance is supervising, as published by teraflock/runtimes.
func (i *instance) RuntimeBuildID() string { return i.buildID }

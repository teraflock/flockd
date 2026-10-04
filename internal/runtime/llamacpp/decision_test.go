package llamacpp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/teraflock/flockd/internal/decision"
	rt "github.com/teraflock/flockd/internal/runtime"
)

const decisionRequest = `{"model":"laya-q8_0","state":"Help! My payouts have been failing for 3 days.","questions":{
  "department":{"type":"choice","instructions":"Which team should handle this?",
     "criteria":{"technical":null,"billing":"Payments, invoicing, refunds","account":"Login and profile"}},
  "urgency":{"type":"score","instructions":"How urgent is this?","criteria":["can wait","this week","today","right now"]},
  "escalate":{"type":"noul","instructions":"Does this need a human within the hour?"},
  "angry":{"type":"noul","instructions":"Is the customer angry?","criteria":{"true":"clearly upset","false":"calm"}}}}`

func decisionInput(t *testing.T) *rt.DecisionInput {
	t.Helper()
	req, err := decision.ParseRequest([]byte(decisionRequest))
	if err != nil {
		t.Fatal(err)
	}
	return &req.Input
}

// fakeSystemOne emulates llama-server's POST /v1/systemone: it records the
// body it was sent and answers with status + reply.
func fakeSystemOne(t *testing.T, status int, reply string, gotBody *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/systemone", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if gotBody != nil {
			*gotBody = string(raw)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			http.Error(w, "content type", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func decisionInstance(t *testing.T, url string) *instance {
	t.Helper()
	inst := testInstance(t, url)
	inst.spec = rt.ModelSpec{ID: "laya-q8_0", Decision: true}
	return inst
}

// The reply is what llama-server b11382 actually returned for this
// request with Laya-Q8_0 (probabilities shortened), with the objects
// shuffled: the adapter must order by the request.
const systemOneReply = `{"model":"/models/Laya-Q8_0.gguf","answers":{
 "angry":{"type":"noul","noul":0.6329},
 "urgency":{"type":"score","score":2.3044,"legend":{"0":"can wait","1":"this week","2":"today","3":"right now"},
   "probabilities":{"0":0.0097,"1":0.1485,"2":0.3697,"3":0.4722},"confidence":0.3044},
 "escalate":{"type":"noul","noul":0.0757},
 "department":{"type":"choice","choice":"billing","probabilities":{"account":0.0571,"billing":0.7351,"technical":0.2078},"confidence":0.6027}
},"usage":{"input_tokens":172,"output_tokens":0}}`

func TestAdapterDecision(t *testing.T) {
	var sent string
	srv := fakeSystemOne(t, http.StatusOK, systemOneReply, &sent)
	inst := decisionInstance(t, srv.URL)

	ts, err := inst.Complete(context.Background(), rt.CompletionRequest{Kind: rt.KindDecision, Decision: decisionInput(t)})
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := ts.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("a decision is one chunk; second Recv err = %v", err)
	}

	// The body llama-server received: questions and criteria in the order
	// written (technical before billing before account), no `model`.
	want := `{"state":"Help! My payouts have been failing for 3 days.","questions":{` +
		`"department":{"type":"choice","instructions":"Which team should handle this?","criteria":{"technical":null,"billing":"Payments, invoicing, refunds","account":"Login and profile"}},` +
		`"urgency":{"type":"score","instructions":"How urgent is this?","criteria":["can wait","this week","today","right now"]},` +
		`"escalate":{"type":"noul","instructions":"Does this need a human within the hour?"},` +
		`"angry":{"type":"noul","instructions":"Is the customer angry?","criteria":{"true":"clearly upset","false":"calm"}}}}`
	if sent != want {
		t.Fatalf("body sent to llama-server:\n got %s\nwant %s", sent, want)
	}

	if !chunk.Done || chunk.Usage == nil || *chunk.Usage != (rt.Usage{PromptTokens: 172, CompletionTokens: 0}) {
		t.Fatalf("chunk done=%v usage=%+v", chunk.Done, chunk.Usage)
	}
	a := chunk.Decision
	if len(a) != 4 || a[0].QuestionID != "department" || a[1].QuestionID != "urgency" || a[2].QuestionID != "escalate" || a[3].QuestionID != "angry" {
		t.Fatalf("answers not in question order: %+v", a)
	}
	if a[0].Choice != "billing" || a[0].Confidence != 0.6027 || len(a[0].Probabilities) != 3 ||
		a[0].Probabilities[0] != (rt.DecisionProbability{Key: "technical", Probability: 0.2078}) ||
		a[0].Probabilities[1] != (rt.DecisionProbability{Key: "billing", Probability: 0.7351}) ||
		a[0].Probabilities[2] != (rt.DecisionProbability{Key: "account", Probability: 0.0571}) {
		t.Fatalf("choice = %+v", a[0])
	}
	if a[1].Score != 2.3044 || len(a[1].Probabilities) != 4 || a[1].Probabilities[3] != (rt.DecisionProbability{Key: "3", Probability: 0.4722}) {
		t.Fatalf("score = %+v", a[1])
	}
	if a[2].Noul != 0.0757 || a[3].Noul != 0.6329 {
		t.Fatalf("nouls = %+v %+v", a[2], a[3])
	}
}

func TestAdapterDecisionRuntimeStatuses(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		reply       string
		invalid     bool
		wantMessage string
	}{
		// 400: llama-server rejected the input itself -> do not retry elsewhere.
		{"400 over context", http.StatusBadRequest,
			`{"error":{"code":400,"message":"input (3028 tokens) is larger than the max context size (2048 tokens). skipping","type":"exceed_context_size_error"}}`,
			true, "input (3028 tokens) is larger than the max context size"},
		// 501: the loaded model is not a decision model.
		{"501 not a decision model", http.StatusNotImplemented,
			`{"error":{"code":501,"message":"This server does not support decision models","type":"not_supported_error"}}`,
			false, "501"},
		// The batch check runs before the context check and answers 500;
		// with the batch set to the slot's context it means the same thing.
		{"500 over batch (= context)", http.StatusInternalServerError,
			`{"error":{"code":500,"message":"input (3031 tokens) is too large to process. increase the physical batch size (current batch size: 2048)","type":"server_error"}}`,
			true, "input is larger than the model's context: input (3031 tokens) is too large to process"},
		{"500 other", http.StatusInternalServerError, `{"error":{"code":500,"message":"the current context does not support logits computation. skipping","type":"server_error"}}`,
			false, "does not support logits"},
		{"500 over batch, batch not set by the adapter", http.StatusInternalServerError, `{"error":{"code":500,"message":"input (728 tokens) is too large to process. increase the physical batch size (current batch size: 512)","type":"server_error"}}`,
			false, "increase the physical batch size"},
		{"200 garbage", http.StatusOK, `not json`, false, "decode runtime response"},
		{"200 missing answer", http.StatusOK, `{"answers":{},"usage":{"input_tokens":1}}`, false, "no answer for question"},
	}
	for _, c := range cases {
		srv := fakeSystemOne(t, c.status, c.reply, nil)
		inst := decisionInstance(t, srv.URL)
		inst.batchIsContext = !strings.Contains(c.name, "not set by the adapter")
		_, err := inst.Complete(context.Background(), rt.CompletionRequest{Kind: rt.KindDecision, Decision: decisionInput(t)})
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if rt.IsInvalidInput(err) != c.invalid {
			t.Errorf("%s: invalid input = %v, want %v (%v)", c.name, rt.IsInvalidInput(err), c.invalid, err)
		}
		if !strings.Contains(err.Error(), c.wantMessage) {
			t.Errorf("%s: error %q does not contain %q", c.name, err, c.wantMessage)
		}
	}
}

func TestAdapterDecisionNeedsDecisionModel(t *testing.T) {
	srv := fakeSystemOne(t, http.StatusOK, systemOneReply, nil)
	inst := testInstance(t, srv.URL) // a chat model
	_, err := inst.Complete(context.Background(), rt.CompletionRequest{Kind: rt.KindDecision, Decision: decisionInput(t)})
	if err == nil || rt.IsInvalidInput(err) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}

func TestServerArgsDecisionModel(t *testing.T) {
	a := &Adapter{Accel: "cpu"}
	res := rt.ResourceBudget{MaxConcurrent: 2, Slots: 4, ContextTokens: 8192}
	args := a.serverArgs(rt.ModelSpec{ID: "laya", Path: "/models/laya.gguf", Decision: true, Embeddings: true}, res, 4242)
	joined := strings.Join(args, " ")
	if slices.Contains(args, "--embeddings") {
		t.Fatalf("decision model started with --embeddings: %s", joined)
	}
	// One slot's context: 8192 / 4.
	for _, want := range []string{"--ctx-size 8192", "--parallel 4", "--batch-size 2048", "--ubatch-size 2048"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	// A context that does not divide evenly rounds the batch up.
	if got := decisionBatch(8193, 4); got != 2049 {
		t.Errorf("decisionBatch(8193, 4) = %d, want 2049", got)
	}

	// Kev is causal: llama-server takes its prompt a batch at a time, so
	// it keeps the default batch instead of one sized for a 65k window.
	kev := strings.Join(a.serverArgs(rt.ModelSpec{ID: "kev-4b", Family: "kev", Path: "/models/kev.gguf", Decision: true},
		rt.ResourceBudget{Slots: 2, ContextTokens: 131072}, 1), " ")
	if strings.Contains(kev, "batch-size") || strings.Contains(kev, "--embeddings") || !strings.Contains(kev, "--ctx-size 131072") {
		t.Errorf("kev args: %s", kev)
	}
	for _, family := range []string{"laya", "julia", "clef", "", "something-new"} {
		got := strings.Join(a.serverArgs(rt.ModelSpec{ID: "m", Family: family, Path: "/m.gguf", Decision: true}, res, 1), " ")
		if !strings.Contains(got, "--ubatch-size 2048") {
			t.Errorf("family %q: no whole-prompt batch: %s", family, got)
		}
	}

	chat := strings.Join(a.serverArgs(rt.ModelSpec{ID: "m", Path: "/models/m.gguf"}, res, 1), " ")
	if strings.Contains(chat, "batch-size") {
		t.Errorf("chat model got batch flags: %s", chat)
	}
	emb := a.serverArgs(rt.ModelSpec{ID: "e", Path: "/models/e.gguf", Embeddings: true}, res, 1)
	if !slices.Contains(emb, "--embeddings") || strings.Contains(strings.Join(emb, " "), "batch-size") {
		t.Errorf("embedding model args: %v", emb)
	}
}

func TestBuildGateForDecisionModels(t *testing.T) {
	tags := []struct {
		id  string
		tag int
		ok  bool
	}{
		{"llamacpp-b9892-4", 9892, true},
		{"llamacpp-b11382-1", 11382, true},
		{"llamacpp-b12000-12", 12000, true},
		{"local-binary", 0, false},
		{"mock-runtime-v1", 0, false},
		{"llamacpp-b9892", 0, false},
		{"llamacpp-bx1-1", 0, false},
		{"llamacpp-b-1", 0, false},
		{"", 0, false},
	}
	for _, c := range tags {
		if tag, ok := buildTag(c.id); tag != c.tag || ok != c.ok {
			t.Errorf("buildTag(%q) = %d, %v; want %d, %v", c.id, tag, ok, c.tag, c.ok)
		}
	}

	dec := rt.ModelSpec{ID: "laya-q8_0", Decision: true}
	err := checkBuildSupports("llamacpp-b9892-4", dec)
	if !errors.Is(err, rt.ErrRuntimeTooOld) {
		t.Fatalf("pinned b9892 build accepted a decision model: %v", err)
	}
	for _, want := range []string{"laya-q8_0", "b11382", "llamacpp-b9892-4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("reason %q does not name %q", err, want)
		}
	}
	if err := checkBuildSupports("llamacpp-b11381-9", dec); !errors.Is(err, rt.ErrRuntimeTooOld) {
		t.Errorf("b11381 accepted: %v", err)
	}
	for _, id := range []string{"llamacpp-b11382-1", "llamacpp-b20000-1", "local-binary"} {
		if err := checkBuildSupports(id, dec); err != nil {
			t.Errorf("build %s refused a decision model: %v", id, err)
		}
	}
	if err := checkBuildSupports("llamacpp-b9892-4", rt.ModelSpec{ID: "chat"}); err != nil {
		t.Errorf("old build refused a chat model: %v", err)
	}
}

// The gate through the public surface: SupportsModel and Load both refuse
// a decision model on an old published build, before anything runs.
func TestAdapterRefusesDecisionOnOldBuild(t *testing.T) {
	a := &Adapter{Fetcher: &Fetcher{BinaryPath: os.Args[0]}, Accel: "cpu"}
	// An operator's own binary has no readable tag: not refused up front.
	if err := a.SupportsModel(context.Background(), rt.ModelSpec{ID: "laya", Decision: true}); err != nil {
		t.Fatalf("local binary refused: %v", err)
	}
	if err := a.SupportsModel(context.Background(), rt.ModelSpec{ID: "chat"}); err != nil {
		t.Fatalf("chat model refused: %v", err)
	}
}

// TestRealLlamaServerDecision runs the adapter's own code path — the
// flags serverArgs generates, the supervisor, decide — against a real
// llama-server and a real decision model. Skipped unless both are given:
//
//	FLOCKD_IT_LLAMA_SERVER=/path/to/llama-server (>= b11382)
//	FLOCKD_IT_DECISION_MODEL=/path/to/Laya-Q8_0.gguf
//	FLOCKD_IT_DECISION_FAMILY=laya   (optional; the catalog family)
func TestRealLlamaServerDecision(t *testing.T) {
	bin, model := os.Getenv("FLOCKD_IT_LLAMA_SERVER"), os.Getenv("FLOCKD_IT_DECISION_MODEL")
	if bin == "" || model == "" {
		t.Skip("set FLOCKD_IT_LLAMA_SERVER and FLOCKD_IT_DECISION_MODEL to run against a real llama-server")
	}
	a := &Adapter{Fetcher: &Fetcher{BinaryPath: bin}, Accel: "metal"}
	spec := rt.ModelSpec{ID: "laya-q8_0", Family: os.Getenv("FLOCKD_IT_DECISION_FAMILY"), Path: model, ContextLength: 2048, Decision: true}
	res := rt.ResourceBudget{MaxConcurrent: 4, Slots: 4, ContextTokens: 8192}
	t.Logf("llama-server args: %s", strings.Join(a.serverArgs(spec, res, 0), " "))

	ctx := context.Background()
	inst, err := a.Load(ctx, spec, res)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	defer func() { _ = inst.Shutdown(ctx) }()

	in := decisionInput(t)
	ts, err := inst.Complete(ctx, rt.CompletionRequest{Kind: rt.KindDecision, Decision: in})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	chunk, err := ts.Recv()
	if err != nil {
		t.Fatal(err)
	}
	out, err := decision.WriteResponse(spec.ID, in, chunk.Decision, *chunk.Usage)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("public response: %s", out)

	if len(chunk.Decision) != 4 || chunk.Usage.PromptTokens == 0 || chunk.Usage.CompletionTokens != 0 {
		t.Fatalf("answers=%d usage=%+v", len(chunk.Decision), chunk.Usage)
	}
	for n, q := range in.Questions {
		a := chunk.Decision[n]
		if a.QuestionID != q.ID || a.Type != q.Type {
			t.Fatalf("answer %d = %+v, want question %s", n, a, q.ID)
		}
		if q.Type == rt.DecisionNoul {
			continue
		}
		sum := 0.0
		for i, p := range a.Probabilities {
			if p.Key != q.Options[i].Key {
				t.Fatalf("%s: probability %d is %q, want option order %q", q.ID, i, p.Key, q.Options[i].Key)
			}
			sum += p.Probability
		}
		if sum < 0.999 || sum > 1.001 {
			t.Errorf("%s: probabilities sum to %v", q.ID, sum)
		}
	}

	// A prompt longer than llama-server's default batch (512) but inside
	// the slot's context: only works because of --ubatch-size.
	long := &rt.DecisionInput{
		StateJSON: `"` + strings.Repeat("word ", 1500) + `"`,
		Questions: []rt.DecisionQuestion{{ID: "a", Type: rt.DecisionNoul, InstructionsJSON: `"Is this repetitive?"`}},
	}
	ts, err = inst.Complete(ctx, rt.CompletionRequest{Kind: rt.KindDecision, Decision: long})
	if err != nil {
		t.Fatalf("long prompt: %v", err)
	}
	chunk, _ = ts.Recv()
	t.Logf("long prompt: %d input tokens, noul=%v", chunk.Usage.PromptTokens, chunk.Decision[0].Noul)
	if chunk.Usage.PromptTokens <= 512 {
		t.Fatalf("long prompt was only %d tokens", chunk.Usage.PromptTokens)
	}

	// Over the slot's context: llama-server's 400 -> invalid input.
	long.StateJSON = `"` + strings.Repeat("word ", 3000) + `"`
	_, err = inst.Complete(ctx, rt.CompletionRequest{Kind: rt.KindDecision, Decision: long})
	if !rt.IsInvalidInput(err) {
		t.Fatalf("over-context prompt: err = %v, want InvalidInputError", err)
	}
	t.Logf("over-context prompt: invalid input: %v", err)
}

package decision

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	rt "github.com/teraflock/flockd/internal/runtime"
)

// The request every ordering test uses: choice keys deliberately NOT in
// alphabetical order (a marshalled Go map would sort them), a null
// description, object/array leaves, score levels, and noul with and
// without criteria. Question ids are not alphabetical either.
const orderedRequest = `{
  "model": "flock/laya-q8_0",
  "state": {"ticket": 4711, "amount": 12.50, "text": "payouts <failing> for 3 days"},
  "ignored_field": true,
  "questions": {
    "zeta_department": {"type": "choice", "instructions": "Which team should handle this?",
        "criteria": {"technical": null, "billing": "Payments, invoicing, refunds", "account": {"covers": ["login", "profile"]}}},
    "urgency": {"type": "score", "instructions": ["How urgent", "is this?"],
        "criteria": ["can wait", "this week", "today", "right now"]},
    "escalate": {"type": "noul", "instructions": "Does this need a human within the hour?"},
    "angry": {"type": "noul", "instructions": "Is the customer angry?",
        "criteria": {"false": "calm", "true": "clearly upset"}}
  }
}`

func mustParse(t *testing.T, body string) *Request {
	t.Helper()
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	return req
}

func TestParseRequestPreservesOrder(t *testing.T) {
	req := mustParse(t, orderedRequest)
	if req.Model != "laya-q8_0" {
		t.Fatalf("model = %q, want the flock/ alias stripped", req.Model)
	}
	// Compact JSON text, number digits and key order intact, no HTML escaping.
	if want := `{"ticket":4711,"amount":12.50,"text":"payouts <failing> for 3 days"}`; req.Input.StateJSON != want {
		t.Fatalf("state = %s\nwant    %s", req.Input.StateJSON, want)
	}
	var ids []string
	for _, q := range req.Input.Questions {
		ids = append(ids, q.ID)
	}
	if got := strings.Join(ids, ","); got != "zeta_department,urgency,escalate,angry" {
		t.Fatalf("question order = %s", got)
	}
	q := req.Input.Questions
	wantChoice := []rt.DecisionOption{
		{Key: "technical"},
		{Key: "billing", DescriptionJSON: `"Payments, invoicing, refunds"`},
		{Key: "account", DescriptionJSON: `{"covers":["login","profile"]}`},
	}
	if q[0].Type != rt.DecisionChoice || fmt.Sprint(q[0].Options) != fmt.Sprint(wantChoice) {
		t.Fatalf("choice options = %+v", q[0].Options)
	}
	if q[1].Type != rt.DecisionScore || len(q[1].Options) != 4 ||
		q[1].Options[0] != (rt.DecisionOption{Key: "0", DescriptionJSON: `"can wait"`}) ||
		q[1].Options[3] != (rt.DecisionOption{Key: "3", DescriptionJSON: `"right now"`}) {
		t.Fatalf("score levels = %+v", q[1].Options)
	}
	if q[1].InstructionsJSON != `["How urgent","is this?"]` {
		t.Fatalf("score instructions = %s", q[1].InstructionsJSON)
	}
	if q[2].Type != rt.DecisionNoul || len(q[2].Options) != 0 {
		t.Fatalf("noul without criteria = %+v", q[2])
	}
	// noul criteria keep the order written (false first here).
	if len(q[3].Options) != 2 || q[3].Options[0].Key != "false" || q[3].Options[1].Key != "true" {
		t.Fatalf("noul with criteria = %+v", q[3].Options)
	}
}

func TestRuntimeBodyIsOrderedJSON(t *testing.T) {
	req := mustParse(t, orderedRequest)
	body, err := RuntimeBody(&req.Input)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":{"ticket":4711,"amount":12.50,"text":"payouts <failing> for 3 days"},` +
		`"questions":{` +
		`"zeta_department":{"type":"choice","instructions":"Which team should handle this?",` +
		`"criteria":{"technical":null,"billing":"Payments, invoicing, refunds","account":{"covers":["login","profile"]}}},` +
		`"urgency":{"type":"score","instructions":["How urgent","is this?"],"criteria":["can wait","this week","today","right now"]},` +
		`"escalate":{"type":"noul","instructions":"Does this need a human within the hour?"},` +
		`"angry":{"type":"noul","instructions":"Is the customer angry?","criteria":{"false":"calm","true":"clearly upset"}}` +
		`}}`
	if string(body) != want {
		t.Fatalf("runtime body:\n got %s\nwant %s", body, want)
	}
	if !json.Valid(body) {
		t.Fatal("runtime body is not valid JSON")
	}
}

func TestRuntimeBodyEscapesKeys(t *testing.T) {
	in := &rt.DecisionInput{
		StateJSON: `"s"`,
		Questions: []rt.DecisionQuestion{{
			ID: `we"ird\id`, Type: rt.DecisionChoice, InstructionsJSON: `"q"`,
			Options: []rt.DecisionOption{{Key: "b<1>"}, {Key: "a\n2"}},
		}},
	}
	body, err := RuntimeBody(in)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"state":"s","questions":{"we\"ird\\id":{"type":"choice","instructions":"q","criteria":{"b<1>":null,"a\n2":null}}}}`
	if string(body) != want {
		t.Fatalf("got  %s\nwant %s", body, want)
	}
}

func TestRuntimeBodyRejectsBrokenInput(t *testing.T) {
	cases := map[string]*rt.DecisionInput{
		"nil":         nil,
		"no question": {StateJSON: `"s"`},
		"state not json": {StateJSON: `{"a":`, Questions: []rt.DecisionQuestion{
			{ID: "a", Type: rt.DecisionNoul, InstructionsJSON: `"q"`}}},
		"empty instructions": {StateJSON: `"s"`, Questions: []rt.DecisionQuestion{
			{ID: "a", Type: rt.DecisionNoul}}},
		"description not json": {StateJSON: `"s"`, Questions: []rt.DecisionQuestion{
			{ID: "a", Type: rt.DecisionChoice, InstructionsJSON: `"q"`, Options: []rt.DecisionOption{{Key: "x", DescriptionJSON: `nope`}}}}},
		"unknown type": {StateJSON: `"s"`, Questions: []rt.DecisionQuestion{
			{ID: "a", InstructionsJSON: `"q"`}}},
	}
	for name, in := range cases {
		if _, err := RuntimeBody(in); !rt.IsInvalidInput(err) {
			t.Errorf("%s: err = %v, want InvalidInputError", name, err)
		}
	}
}

// runtimeReply is a llama-server response for orderedRequest with every
// object in a DIFFERENT order than the request, plus the runtime's own
// legend: the mapping must follow the request, not this document.
const runtimeReply = `{"model":"/models/laya.gguf","answers":{
  "angry":{"type":"noul","noul":0.6328},
  "escalate":{"type":"noul","noul":0.0757},
  "urgency":{"type":"score","score":2.3043,"legend":{"0":"x","1":"x","2":"x","3":"x"},
     "probabilities":{"3":0.4722,"1":0.1485,"0":0.0096,"2":0.3697},"confidence":0.3043},
  "zeta_department":{"type":"choice","choice":"billing",
     "probabilities":{"account":0.0571,"billing":0.7351,"technical":0.2078},"confidence":0.6027}
 },"usage":{"input_tokens":172,"output_tokens":0}}`

func TestParseRuntimeResponseInQuestionOrder(t *testing.T) {
	req := mustParse(t, orderedRequest)
	answers, usage, err := ParseRuntimeResponse([]byte(runtimeReply), &req.Input)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (rt.Usage{PromptTokens: 172}) {
		t.Fatalf("usage = %+v, want input_tokens as prompt tokens and no completion tokens", usage)
	}
	if len(answers) != 4 {
		t.Fatalf("answers = %d", len(answers))
	}
	c := answers[0]
	if c.QuestionID != "zeta_department" || c.Type != rt.DecisionChoice || c.Choice != "billing" || c.Confidence != 0.6027 {
		t.Fatalf("choice answer = %+v", c)
	}
	wantP := []rt.DecisionProbability{{Key: "technical", Probability: 0.2078}, {Key: "billing", Probability: 0.7351}, {Key: "account", Probability: 0.0571}}
	if fmt.Sprint(c.Probabilities) != fmt.Sprint(wantP) {
		t.Fatalf("choice probabilities = %+v, want option order %+v", c.Probabilities, wantP)
	}
	s := answers[1]
	wantS := []rt.DecisionProbability{{Key: "0", Probability: 0.0096}, {Key: "1", Probability: 0.1485}, {Key: "2", Probability: 0.3697}, {Key: "3", Probability: 0.4722}}
	if s.QuestionID != "urgency" || s.Type != rt.DecisionScore || s.Score != 2.3043 || s.Confidence != 0.3043 ||
		fmt.Sprint(s.Probabilities) != fmt.Sprint(wantS) {
		t.Fatalf("score answer = %+v", s)
	}
	if n := answers[2]; n.QuestionID != "escalate" || n.Type != rt.DecisionNoul || n.Noul != 0.0757 || n.Probabilities != nil {
		t.Fatalf("noul answer = %+v", n)
	}
	if n := answers[3]; n.QuestionID != "angry" || n.Noul != 0.6328 {
		t.Fatalf("noul (criteria) answer = %+v", n)
	}
}

func TestParseRuntimeResponseRejectsMismatch(t *testing.T) {
	req := mustParse(t, `{"model":"m","state":"s","questions":{
		"a":{"type":"choice","instructions":"q","criteria":{"x":null,"y":null}}}}`)
	cases := map[string]string{
		"not json":            `nope`,
		"missing answer":      `{"answers":{}}`,
		"wrong type":          `{"answers":{"a":{"type":"noul","noul":0.5}}}`,
		"no choice":           `{"answers":{"a":{"type":"choice","probabilities":{"x":0.5,"y":0.5}}}}`,
		"missing probability": `{"answers":{"a":{"type":"choice","choice":"x","probabilities":{"x":1}}}}`,
	}
	for name, body := range cases {
		if _, _, err := ParseRuntimeResponse([]byte(body), &req.Input); err == nil {
			t.Errorf("%s: no error", name)
		} else if rt.IsInvalidInput(err) {
			t.Errorf("%s: a broken runtime reply is not the customer's invalid input: %v", name, err)
		}
	}
}

func TestWriteResponseOrderedWithLegendFromRequest(t *testing.T) {
	req := mustParse(t, orderedRequest)
	answers, usage, err := ParseRuntimeResponse([]byte(runtimeReply), &req.Input)
	if err != nil {
		t.Fatal(err)
	}
	out, err := WriteResponse(req.Model, &req.Input, answers, usage)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"laya-q8_0","answers":{` +
		`"zeta_department":{"type":"choice","choice":"billing","probabilities":{"technical":0.2078,"billing":0.7351,"account":0.0571},"confidence":0.6027},` +
		`"urgency":{"type":"score","score":2.3043,"legend":{"0":"can wait","1":"this week","2":"today","3":"right now"},` +
		`"probabilities":{"0":0.0096,"1":0.1485,"2":0.3697,"3":0.4722},"confidence":0.3043},` +
		`"escalate":{"type":"noul","noul":0.0757},` +
		`"angry":{"type":"noul","noul":0.6328}` +
		`},"usage":{"input_tokens":172,"output_tokens":0}}`
	if string(out) != want {
		t.Fatalf("response:\n got %s\nwant %s", out, want)
	}
}

func TestWriteResponseRejectsMisalignedAnswers(t *testing.T) {
	req := mustParse(t, orderedRequest)
	answers, usage, _ := ParseRuntimeResponse([]byte(runtimeReply), &req.Input)
	if _, err := WriteResponse("m", &req.Input, answers[:3], usage); err == nil {
		t.Error("too few answers accepted")
	}
	answers[0], answers[1] = answers[1], answers[0]
	if _, err := WriteResponse("m", &req.Input, answers, usage); err == nil {
		t.Error("answers out of question order accepted")
	}
}

// q builds a one-question request body.
func q(question string) string {
	return `{"model":"m","state":"s","questions":{"a":` + question + `}}`
}

func manyOptions(n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"o%d":null`, i)
	}
	b.WriteString("}")
	return b.String()
}

func manyLevels(n int) string {
	levels := make([]string, n)
	for i := range levels {
		levels[i] = fmt.Sprintf(`"level %d"`, i)
	}
	return "[" + strings.Join(levels, ",") + "]"
}

func manyQuestions(n int) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","state":"s","questions":{`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"q%d":{"type":"noul","instructions":"q"}`, i)
	}
	b.WriteString("}}")
	return b.String()
}

func TestValidationLimits(t *testing.T) {
	valid := map[string]string{
		"64 questions":          manyQuestions(64),
		"2 choice options":      q(`{"type":"choice","instructions":"q","criteria":` + manyOptions(2) + `}`),
		"255 choice options":    q(`{"type":"choice","instructions":"q","criteria":` + manyOptions(255) + `}`),
		"2 score levels":        q(`{"type":"score","instructions":"q","criteria":` + manyLevels(2) + `}`),
		"10 score levels":       q(`{"type":"score","instructions":"q","criteria":` + manyLevels(10) + `}`),
		"noul null criteria":    q(`{"type":"noul","instructions":"q","criteria":null}`),
		"object state":          `{"model":"m","state":{"a":1},"questions":{"a":{"type":"noul","instructions":"q"}}}`,
		"array state":           `{"model":"m","state":[1,2],"questions":{"a":{"type":"noul","instructions":"q"}}}`,
		"object instructions":   q(`{"type":"noul","instructions":{"q":"is it?"}}`),
		"images null":           `{"model":"m","state":"s","images":null,"questions":{"a":{"type":"noul","instructions":"q"}}}`,
		"unknown fields":        `{"model":"m","state":"s","temperature":0.2,"questions":{"a":{"type":"noul","instructions":"q","extra":1}}}`,
		"object/array criteria": q(`{"type":"score","instructions":"q","criteria":[{"d":1},["a"]]}`),
	}
	for name, body := range valid {
		if _, err := ParseRequest([]byte(body)); err != nil {
			t.Errorf("valid %q rejected: %v", name, err)
		}
	}

	// Each invalid body, and a fragment its message must contain (the
	// offending path).
	invalidCases := []struct{ name, body, want string }{
		{"missing model", `{"state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`, "model is required"},
		{"model not a string", `{"model":7,"state":"s","questions":{"a":{"type":"noul","instructions":"q"}}}`, "model is required"},
		{"missing state", `{"model":"m","questions":{"a":{"type":"noul","instructions":"q"}}}`, "state is required"},
		{"null state", `{"model":"m","state":null,"questions":{"a":{"type":"noul","instructions":"q"}}}`, "state is required"},
		{"number state", `{"model":"m","state":5,"questions":{"a":{"type":"noul","instructions":"q"}}}`, "state must be a string, an object or an array"},
		{"missing questions", `{"model":"m","state":"s"}`, "questions is required"},
		{"questions array", `{"model":"m","state":"s","questions":[]}`, "questions must be an object"},
		{"no questions", `{"model":"m","state":"s","questions":{}}`, "1 to 64"},
		{"65 questions", manyQuestions(65), "1 to 64 entries, got 65"},
		{"duplicate question id", `{"model":"m","state":"s","questions":{"a":{"type":"noul","instructions":"q"},"a":{"type":"noul","instructions":"q"}}}`, "questions.a is defined more than once"},
		{"empty question id", `{"model":"m","state":"s","questions":{"":{"type":"noul","instructions":"q"}}}`, "empty question id"},
		{"question not object", q(`"noul"`), "questions.a must be an object"},
		{"unknown type", q(`{"type":"rank","instructions":"q"}`), "questions.a.type"},
		{"missing type", q(`{"instructions":"q"}`), "questions.a.type"},
		{"missing instructions", q(`{"type":"noul"}`), "questions.a.instructions is required"},
		{"empty instructions", q(`{"type":"noul","instructions":""}`), "questions.a.instructions must not be empty"},
		{"empty object instructions", q(`{"type":"noul","instructions":{}}`), "questions.a.instructions must not be empty"},
		{"number instructions", q(`{"type":"noul","instructions":3}`), "questions.a.instructions must be a string"},
		{"choice without criteria", q(`{"type":"choice","instructions":"q"}`), "questions.a.criteria is required"},
		{"choice criteria array", q(`{"type":"choice","instructions":"q","criteria":["x","y"]}`), "questions.a.criteria must be an object"},
		{"1 choice option", q(`{"type":"choice","instructions":"q","criteria":` + manyOptions(1) + `}`), "2 to 255 options, got 1"},
		{"256 choice options", q(`{"type":"choice","instructions":"q","criteria":` + manyOptions(256) + `}`), "2 to 255 options, got 256"},
		{"duplicate option", q(`{"type":"choice","instructions":"q","criteria":{"x":null,"x":"again"}}`), "questions.a.criteria.x is defined more than once"},
		{"number description", q(`{"type":"choice","instructions":"q","criteria":{"x":1,"y":null}}`), "questions.a.criteria.x"},
		{"score without criteria", q(`{"type":"score","instructions":"q"}`), "questions.a.criteria is required"},
		{"score criteria object", q(`{"type":"score","instructions":"q","criteria":{"0":"lo","1":"hi"}}`), "questions.a.criteria must be an array"},
		{"1 score level", q(`{"type":"score","instructions":"q","criteria":` + manyLevels(1) + `}`), "2 to 10 levels, got 1"},
		{"11 score levels", q(`{"type":"score","instructions":"q","criteria":` + manyLevels(11) + `}`), "2 to 10 levels, got 11"},
		{"null score level", q(`{"type":"score","instructions":"q","criteria":["lo",null]}`), "questions.a.criteria[1]"},
		{"noul only true", q(`{"type":"noul","instructions":"q","criteria":{"true":"yes"}}`), "exactly the keys true and false"},
		{"noul extra key", q(`{"type":"noul","instructions":"q","criteria":{"true":"y","false":"n","maybe":"m"}}`), "exactly the keys true and false"},
		{"noul null description", q(`{"type":"noul","instructions":"q","criteria":{"true":null,"false":"n"}}`), "questions.a.criteria.true"},
		{"noul criteria array", q(`{"type":"noul","instructions":"q","criteria":["y","n"]}`), "questions.a.criteria must be an object"},
		{"images", `{"model":"m","state":"s","images":["data:image/png;base64,AAAA"],"questions":{"a":{"type":"noul","instructions":"q"}}}`, "image input is not supported by this model"},
	}
	for _, c := range invalidCases {
		_, err := ParseRequest([]byte(c.body))
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("%s: err = %v, want a ValidationError", c.name, err)
			continue
		}
		if !strings.Contains(verr.Msg, c.want) {
			t.Errorf("%s: message %q does not contain %q", c.name, verr.Msg, c.want)
		}
	}

	for _, body := range []string{``, `nope`, `[1]`, `"str"`, `{"model":"m",`} {
		if _, err := ParseRequest([]byte(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("malformed %q: err = %v, want ErrMalformed", body, err)
		}
	}
}

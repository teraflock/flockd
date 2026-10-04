package decision

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	rt "github.com/teraflock/flockd/internal/runtime"
)

// writer builds JSON by hand so object members come out in the order they
// are written. encoding/json sorts map keys, which would reorder
// `questions` and `criteria`.
type writer struct {
	b bytes.Buffer
}

// str writes s as a JSON string. HTML escaping is off: the text is model
// input, not markup, and `<` must not turn into `<` for no reason.
func (w *writer) str(s string) {
	enc := json.NewEncoder(&w.b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)           // a string never fails to encode
	w.b.Truncate(w.b.Len() - 1) // Encode appends a newline
}

func (w *writer) raw(s string) { w.b.WriteString(s) }

// key writes `"k":`, preceded by a comma unless first.
func (w *writer) key(first bool, k string) {
	if !first {
		w.b.WriteByte(',')
	}
	w.str(k)
	w.b.WriteByte(':')
}

// num writes a finite float the way encoding/json does (shortest
// round-trip representation).
func (w *writer) num(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("decision: non-finite number in answer")
	}
	out, err := json.Marshal(f)
	if err != nil {
		return err
	}
	w.b.Write(out)
	return nil
}

// jsonOrNull writes a compact JSON leaf; "" is JSON null.
func (w *writer) jsonOrNull(leaf string) {
	if leaf == "" {
		w.raw("null")
		return
	}
	w.raw(leaf)
}

func checkLeaf(leaf, path string, nullable bool) error {
	if leaf == "" {
		if nullable {
			return nil
		}
		return &rt.InvalidInputError{Msg: path + " is empty"}
	}
	if !json.Valid([]byte(leaf)) {
		return &rt.InvalidInputError{Msg: path + " is not valid JSON"}
	}
	return nil
}

// RuntimeBody builds the llama-server `POST /v1/systemone` body from a
// DecisionInput, writing `questions` and every `criteria` object in list
// order. The input was validated by whoever accepted the request (the
// gateway, or ParseRequest locally); this only refuses what would not be
// JSON at all, as an *rt.InvalidInputError.
func RuntimeBody(in *rt.DecisionInput) ([]byte, error) {
	if in == nil || len(in.Questions) == 0 {
		return nil, &rt.InvalidInputError{Msg: "decision input has no questions"}
	}
	if err := checkLeaf(in.StateJSON, "state", false); err != nil {
		return nil, err
	}
	var w writer
	w.raw(`{"state":`)
	w.raw(in.StateJSON)
	w.raw(`,"questions":{`)
	for n, q := range in.Questions {
		path := "questions." + q.ID
		if err := checkLeaf(q.InstructionsJSON, path+".instructions", false); err != nil {
			return nil, err
		}
		for _, o := range q.Options {
			if err := checkLeaf(o.DescriptionJSON, path+".criteria."+o.Key, true); err != nil {
				return nil, err
			}
		}
		w.key(n == 0, q.ID)
		w.raw(`{"type":`)
		w.str(q.Type.String())
		w.raw(`,"instructions":`)
		w.raw(q.InstructionsJSON)
		switch q.Type {
		case rt.DecisionChoice:
			w.raw(`,"criteria":{`)
			for i, o := range q.Options {
				w.key(i == 0, o.Key)
				w.jsonOrNull(o.DescriptionJSON)
			}
			w.raw(`}`)
		case rt.DecisionScore:
			// An array, lowest level first: the option keys are the
			// indexes and are not written.
			w.raw(`,"criteria":[`)
			for i, o := range q.Options {
				if i > 0 {
					w.raw(",")
				}
				w.jsonOrNull(o.DescriptionJSON)
			}
			w.raw(`]`)
		case rt.DecisionNoul:
			if len(q.Options) > 0 {
				w.raw(`,"criteria":{`)
				for i, o := range q.Options {
					w.key(i == 0, o.Key)
					w.jsonOrNull(o.DescriptionJSON)
				}
				w.raw(`}`)
			}
		default:
			return nil, &rt.InvalidInputError{Msg: path + ".type is not choice, score or noul"}
		}
		w.raw(`}`)
	}
	w.raw(`}}`)
	return w.b.Bytes(), nil
}

// runtimeAnswer is one answer as llama-server writes it. Lookups by key
// are fine here: the order comes from the request, not from this object.
type runtimeAnswer struct {
	Type          string             `json:"type"`
	Choice        *string            `json:"choice"`
	Score         *float64           `json:"score"`
	Noul          *float64           `json:"noul"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    *float64           `json:"confidence"`
}

type runtimeResponse struct {
	Answers map[string]runtimeAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}

// ParseRuntimeResponse maps llama-server's /v1/systemone response onto
// typed answers, one per question in question order, with probabilities
// ordered like the question's options (score: level index "0", "1", …).
// The runtime's `legend` is ignored: it is rebuilt from the request.
// Usage is input_tokens as prompt tokens; nothing is generated.
func ParseRuntimeResponse(body []byte, in *rt.DecisionInput) ([]rt.DecisionAnswer, rt.Usage, error) {
	var resp runtimeResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, rt.Usage{}, fmt.Errorf("decision: decode runtime response: %w", err)
	}
	answers := make([]rt.DecisionAnswer, 0, len(in.Questions))
	for _, q := range in.Questions {
		ra, ok := resp.Answers[q.ID]
		if !ok {
			return nil, rt.Usage{}, fmt.Errorf("decision: runtime returned no answer for question %q", q.ID)
		}
		if ra.Type != q.Type.String() {
			return nil, rt.Usage{}, fmt.Errorf("decision: runtime answered question %q as %q, want %s", q.ID, ra.Type, q.Type)
		}
		a := rt.DecisionAnswer{QuestionID: q.ID, Type: q.Type}
		switch q.Type {
		case rt.DecisionNoul:
			if ra.Noul == nil {
				return nil, rt.Usage{}, fmt.Errorf("decision: runtime answer for %q has no noul", q.ID)
			}
			a.Noul = *ra.Noul
		case rt.DecisionChoice, rt.DecisionScore:
			if q.Type == rt.DecisionChoice {
				if ra.Choice == nil {
					return nil, rt.Usage{}, fmt.Errorf("decision: runtime answer for %q has no choice", q.ID)
				}
				a.Choice = *ra.Choice
			} else {
				if ra.Score == nil {
					return nil, rt.Usage{}, fmt.Errorf("decision: runtime answer for %q has no score", q.ID)
				}
				a.Score = *ra.Score
			}
			if ra.Confidence != nil {
				a.Confidence = *ra.Confidence
			}
			a.Probabilities = make([]rt.DecisionProbability, 0, len(q.Options))
			for _, o := range q.Options {
				p, ok := ra.Probabilities[o.Key]
				if !ok {
					return nil, rt.Usage{}, fmt.Errorf("decision: runtime answer for %q has no probability for %q", q.ID, o.Key)
				}
				a.Probabilities = append(a.Probabilities, rt.DecisionProbability{Key: o.Key, Probability: p})
			}
		}
		answers = append(answers, a)
	}
	return answers, rt.Usage{PromptTokens: resp.Usage.InputTokens}, nil
}

// WriteResponse writes the public /v1/systemone response: `answers` and
// every `probabilities` / `legend` object in request order, `legend`
// rebuilt from the request's score criteria, and TypeSafe's usage names
// (output_tokens is always 0). answers must be one per question, in
// question order.
func WriteResponse(model string, in *rt.DecisionInput, answers []rt.DecisionAnswer, usage rt.Usage) ([]byte, error) {
	if len(answers) != len(in.Questions) {
		return nil, fmt.Errorf("decision: %d answers for %d questions", len(answers), len(in.Questions))
	}
	var w writer
	w.raw(`{"model":`)
	w.str(model)
	w.raw(`,"answers":{`)
	for n, q := range in.Questions {
		a := answers[n]
		if a.QuestionID != q.ID || a.Type != q.Type {
			return nil, fmt.Errorf("decision: answer %d is for %q (%s), want %q (%s)", n, a.QuestionID, a.Type, q.ID, q.Type)
		}
		w.key(n == 0, q.ID)
		w.raw(`{"type":`)
		w.str(q.Type.String())
		var err error
		switch q.Type {
		case rt.DecisionChoice:
			w.raw(`,"choice":`)
			w.str(a.Choice)
			err = w.distribution(a)
		case rt.DecisionScore:
			w.raw(`,"score":`)
			if err = w.num(a.Score); err != nil {
				return nil, err
			}
			w.raw(`,"legend":{`)
			for i, o := range q.Options {
				w.key(i == 0, o.Key)
				w.jsonOrNull(o.DescriptionJSON)
			}
			w.raw(`}`)
			err = w.distribution(a)
		case rt.DecisionNoul:
			w.raw(`,"noul":`)
			err = w.num(a.Noul)
		}
		if err != nil {
			return nil, err
		}
		w.raw(`}`)
	}
	w.raw(`},"usage":{"input_tokens":`)
	w.raw(strconv.Itoa(usage.PromptTokens))
	w.raw(`,"output_tokens":0}}`)
	return w.b.Bytes(), nil
}

// distribution writes `,"probabilities":{…},"confidence":c`.
func (w *writer) distribution(a rt.DecisionAnswer) error {
	w.raw(`,"probabilities":{`)
	for i, p := range a.Probabilities {
		w.key(i == 0, p.Key)
		if err := w.num(p.Probability); err != nil {
			return err
		}
	}
	w.raw(`},"confidence":`)
	return w.num(a.Confidence)
}

// Package decision implements the JSON side of typed decisions (TypeSafe's
// "System One" API, POST /v1/systemone): validating the public request
// into an ordered runtime.DecisionInput, building the llama-server request
// body from it, mapping llama-server's answers back onto typed answers,
// and writing the public response.
//
// Key order is part of the contract (proto design note
// 2026-10-03-decision-models): answers come back in request order and a
// model reads the options in the order the customer wrote them. Nothing
// in this package ever decodes `questions` or `criteria` into a Go map or
// marshals one: objects are read with a token decoder and written with an
// ordered writer.
package decision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	rt "github.com/teraflock/flockd/internal/runtime"
)

// Limits of the public contract.
const (
	MaxQuestions     = 64
	MinChoiceOptions = 2
	MaxChoiceOptions = 255
	MinScoreLevels   = 2
	MaxScoreLevels   = 10
)

// ValidationError is a request the public contract rejects (HTTP 422).
// The message names the offending path (`questions.<id>.criteria`).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// ErrMalformed is returned for a body that is not a JSON object at all
// (HTTP 400, like the other /v1 routes).
var ErrMalformed = errors.New("decision: malformed JSON body")

// member is one key/value of a JSON object, in document order.
type member struct {
	Key   string
	Value json.RawMessage
}

// orderedObject decodes a JSON object into its members in document
// order. It reports ok=false when raw is not an object.
func orderedObject(raw json.RawMessage) (members []member, ok bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, false, err
	}
	if d, isDelim := tok.(json.Delim); !isDelim || d != '{' {
		return nil, false, nil
	}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		key, isString := kt.(string)
		if !isString {
			return nil, false, fmt.Errorf("decision: object key is not a string")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false, err
		}
		members = append(members, member{Key: key, Value: val})
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, false, err
	}
	return members, true, nil
}

// kind returns the JSON type of a raw value by its first byte:
// 's'tring, 'o'bject, 'a'rray, 'n'ull, or 'x' for anything else
// (number, boolean, absent).
func kind(raw json.RawMessage) byte {
	t := bytes.TrimLeft(raw, " \t\r\n")
	if len(t) == 0 {
		return 'x'
	}
	switch t[0] {
	case '"':
		return 's'
	case '{':
		return 'o'
	case '[':
		return 'a'
	case 'n':
		return 'n'
	default:
		return 'x'
	}
}

// compact returns raw as compact JSON text. Numbers keep their digits
// and nested objects keep their key order: the text is what the model is
// given.
func compact(raw json.RawMessage) (string, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "", err
	}
	return b.String(), nil
}

// leaf validates a free-form JSON leaf (string, object or array) and
// compacts it.
func leaf(raw json.RawMessage, path string) (string, error) {
	switch kind(raw) {
	case 's', 'o', 'a':
	default:
		return "", invalid("%s must be a string, an object or an array", path)
	}
	out, err := compact(raw)
	if err != nil {
		return "", invalid("%s is not valid JSON", path)
	}
	return out, nil
}

func isEmptyLeaf(compacted string) bool {
	return compacted == `""` || compacted == `{}` || compacted == `[]`
}

// Request is a validated /v1/systemone request.
type Request struct {
	// Model is the requested model as written (a concrete id, a manifest
	// id or its `flock/<id>` alias); the caller resolves it.
	Model string
	Input rt.DecisionInput
}

// ParseRequest validates a public /v1/systemone body against the contract
// and returns it with questions and options in document order. Errors
// are *ValidationError (422) or ErrMalformed (400).
func ParseRequest(body []byte) (*Request, error) {
	top, ok, err := orderedObject(body)
	if err != nil || !ok {
		return nil, ErrMalformed
	}
	// Unknown top-level fields are ignored, like the chat endpoints do; a
	// repeated field keeps its last value, as encoding/json would.
	var model, state, questions, images json.RawMessage
	for _, m := range top {
		switch m.Key {
		case "model":
			model = m.Value
		case "state":
			state = m.Value
		case "questions":
			questions = m.Value
		case "images":
			images = m.Value
		}
	}

	req := &Request{}
	if kind(model) != 's' {
		return nil, invalid("model is required and must be a string")
	}
	if err := json.Unmarshal(model, &req.Model); err != nil || req.Model == "" {
		return nil, invalid("model is required and must be a string")
	}

	if images != nil && kind(images) != 'n' {
		return nil, invalid("image input is not supported by this model")
	}

	if state == nil || kind(state) == 'n' {
		return nil, invalid("state is required")
	}
	if req.Input.StateJSON, err = leaf(state, "state"); err != nil {
		return nil, err
	}

	if questions == nil {
		return nil, invalid("questions is required")
	}
	qs, ok, err := orderedObject(questions)
	if err != nil || !ok {
		return nil, invalid("questions must be an object that maps a question id to a question")
	}
	if len(qs) < 1 || len(qs) > MaxQuestions {
		return nil, invalid("questions must have 1 to %d entries, got %d", MaxQuestions, len(qs))
	}
	seen := make(map[string]bool, len(qs))
	for _, m := range qs {
		if m.Key == "" {
			return nil, invalid("questions has an empty question id")
		}
		if seen[m.Key] {
			return nil, invalid("questions.%s is defined more than once", m.Key)
		}
		seen[m.Key] = true
		q, err := parseQuestion(m.Key, m.Value)
		if err != nil {
			return nil, err
		}
		req.Input.Questions = append(req.Input.Questions, q)
	}
	return req, nil
}

func parseQuestion(id string, raw json.RawMessage) (rt.DecisionQuestion, error) {
	q := rt.DecisionQuestion{ID: id}
	path := "questions." + id
	fields, ok, err := orderedObject(raw)
	if err != nil || !ok {
		return q, invalid("%s must be an object", path)
	}
	var typ, instructions, criteria json.RawMessage
	for _, f := range fields {
		switch f.Key {
		case "type":
			typ = f.Value
		case "instructions":
			instructions = f.Value
		case "criteria":
			criteria = f.Value
		}
	}

	var typeName string
	if kind(typ) == 's' {
		_ = json.Unmarshal(typ, &typeName)
	}
	switch typeName {
	case "choice":
		q.Type = rt.DecisionChoice
	case "score":
		q.Type = rt.DecisionScore
	case "noul":
		q.Type = rt.DecisionNoul
	default:
		return q, invalid("%s.type must be one of choice, score, noul", path)
	}

	if instructions == nil || kind(instructions) == 'n' {
		return q, invalid("%s.instructions is required", path)
	}
	if q.InstructionsJSON, err = leaf(instructions, path+".instructions"); err != nil {
		return q, err
	}
	if isEmptyLeaf(q.InstructionsJSON) {
		return q, invalid("%s.instructions must not be empty", path)
	}

	absent := criteria == nil || kind(criteria) == 'n'
	switch q.Type {
	case rt.DecisionChoice:
		if absent {
			return q, invalid("%s.criteria is required for a choice question: an object of %d to %d options", path, MinChoiceOptions, MaxChoiceOptions)
		}
		q.Options, err = choiceOptions(criteria, path+".criteria")
	case rt.DecisionScore:
		if absent {
			return q, invalid("%s.criteria is required for a score question: an array of %d to %d level descriptions", path, MinScoreLevels, MaxScoreLevels)
		}
		q.Options, err = scoreLevels(criteria, path+".criteria")
	case rt.DecisionNoul:
		if !absent {
			q.Options, err = noulOptions(criteria, path+".criteria")
		}
	}
	return q, err
}

func choiceOptions(raw json.RawMessage, path string) ([]rt.DecisionOption, error) {
	opts, ok, err := orderedObject(raw)
	if err != nil || !ok {
		return nil, invalid("%s must be an object that maps each option to its description", path)
	}
	if len(opts) < MinChoiceOptions || len(opts) > MaxChoiceOptions {
		return nil, invalid("%s must have %d to %d options, got %d", path, MinChoiceOptions, MaxChoiceOptions, len(opts))
	}
	out := make([]rt.DecisionOption, 0, len(opts))
	seen := make(map[string]bool, len(opts))
	for _, o := range opts {
		if o.Key == "" {
			return nil, invalid("%s has an empty option key", path)
		}
		if seen[o.Key] {
			return nil, invalid("%s.%s is defined more than once", path, o.Key)
		}
		seen[o.Key] = true
		opt := rt.DecisionOption{Key: o.Key}
		if kind(o.Value) != 'n' { // null: an option with no description
			if opt.DescriptionJSON, err = leaf(o.Value, path+"."+o.Key); err != nil {
				return nil, invalid("%s.%s must be a string, an object, an array or null", path, o.Key)
			}
		}
		out = append(out, opt)
	}
	return out, nil
}

func scoreLevels(raw json.RawMessage, path string) ([]rt.DecisionOption, error) {
	if kind(raw) != 'a' {
		return nil, invalid("%s must be an array of level descriptions, lowest first", path)
	}
	var levels []json.RawMessage
	if err := json.Unmarshal(raw, &levels); err != nil {
		return nil, invalid("%s must be an array of level descriptions, lowest first", path)
	}
	if len(levels) < MinScoreLevels || len(levels) > MaxScoreLevels {
		return nil, invalid("%s must have %d to %d levels, got %d", path, MinScoreLevels, MaxScoreLevels, len(levels))
	}
	out := make([]rt.DecisionOption, 0, len(levels))
	for n, l := range levels {
		desc, err := leaf(l, fmt.Sprintf("%s[%d]", path, n))
		if err != nil {
			return nil, err
		}
		out = append(out, rt.DecisionOption{Key: strconv.Itoa(n), DescriptionJSON: desc})
	}
	return out, nil
}

// noulOptions reads the optional noul criteria: exactly the `true` and
// `false` descriptions, kept in the order written.
func noulOptions(raw json.RawMessage, path string) ([]rt.DecisionOption, error) {
	opts, ok, err := orderedObject(raw)
	if err != nil || !ok {
		return nil, invalid("%s must be an object with the descriptions of true and false", path)
	}
	if len(opts) != 2 || !(opts[0].Key == "true" && opts[1].Key == "false" || opts[0].Key == "false" && opts[1].Key == "true") {
		return nil, invalid("%s must have exactly the keys true and false", path)
	}
	out := make([]rt.DecisionOption, 0, 2)
	for _, o := range opts {
		desc, err := leaf(o.Value, path+"."+o.Key)
		if err != nil {
			return nil, err
		}
		out = append(out, rt.DecisionOption{Key: o.Key, DescriptionJSON: desc})
	}
	return out, nil
}

package tunnel

import (
	"context"
	"errors"
	"io"

	rt "github.com/teraflock/flockd/internal/runtime"
	tunnelv1 "github.com/teraflock/proto/gen/go/flock/tunnel/v1"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Typed decisions over the tunnel (kind=DECISION; proto design note
// 2026-10-03-decision-models). The gateway validated the request into a
// DecisionInput whose questions and options are ordered lists; the node
// keeps that order through the runtime and answers with one
// DecisionResult — no stream, like EmbeddingResult.

func decisionTypeFromProto(t typesv1.DecisionQuestionType) rt.DecisionType {
	switch t {
	case typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_CHOICE:
		return rt.DecisionChoice
	case typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_SCORE:
		return rt.DecisionScore
	case typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_NOUL:
		return rt.DecisionNoul
	default:
		return 0 // rejected by the runtime as invalid input
	}
}

func decisionTypeToProto(t rt.DecisionType) typesv1.DecisionQuestionType {
	switch t {
	case rt.DecisionChoice:
		return typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_CHOICE
	case rt.DecisionScore:
		return typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_SCORE
	case rt.DecisionNoul:
		return typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_NOUL
	default:
		return typesv1.DecisionQuestionType_DECISION_QUESTION_TYPE_UNSPECIFIED
	}
}

// decisionInputFromProto converts the wire input, preserving the order of
// questions and of each question's options. A nil input converts to an
// empty one, which the runtime rejects as invalid input.
func decisionInputFromProto(in *typesv1.DecisionInput) *rt.DecisionInput {
	out := &rt.DecisionInput{StateJSON: in.GetStateJson()}
	for _, q := range in.GetQuestions() {
		rq := rt.DecisionQuestion{
			ID:               q.GetId(),
			Type:             decisionTypeFromProto(q.GetType()),
			InstructionsJSON: q.GetInstructionsJson(),
		}
		for _, o := range q.GetOptions() {
			rq.Options = append(rq.Options, rt.DecisionOption{Key: o.GetKey(), DescriptionJSON: o.GetDescriptionJson()})
		}
		out.Questions = append(out.Questions, rq)
	}
	return out
}

// decisionAnswersToProto converts typed answers, in order.
func decisionAnswersToProto(answers []rt.DecisionAnswer) []*typesv1.DecisionAnswer {
	out := make([]*typesv1.DecisionAnswer, 0, len(answers))
	for _, a := range answers {
		pa := &typesv1.DecisionAnswer{
			QuestionId: a.QuestionID,
			Type:       decisionTypeToProto(a.Type),
			Choice:     a.Choice,
			Score:      a.Score,
			Noul:       a.Noul,
			Confidence: a.Confidence,
		}
		for _, p := range a.Probabilities {
			pa.Probabilities = append(pa.Probabilities, &typesv1.DecisionProbability{Key: p.Key, Probability: p.Probability})
		}
		out = append(out, pa)
	}
	return out
}

// drainDecision reads a decision stream (one final chunk) to its answers.
func drainDecision(stream rt.TokenStream) ([]rt.DecisionAnswer, rt.Usage, error) {
	defer stream.Close()
	var (
		answers []rt.DecisionAnswer
		usage   rt.Usage
	)
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, usage, err
		}
		if chunk.Err != "" {
			return nil, usage, errors.New(chunk.Err)
		}
		if chunk.Decision != nil {
			answers = chunk.Decision
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
	}
	if answers == nil {
		return nil, usage, errors.New("tunnel: runtime returned no decision answers")
	}
	return answers, usage, nil
}

// runDecision executes a kind=DECISION dispatch and sends its single
// DecisionResult. Every outcome is a DecisionResult — a failure sets
// `error`, and `invalid_input` when the runtime rejected the input itself
// (llama-server's 400), so the coordinator stops instead of retrying on
// another node and the gateway answers 422.
func (c *Client) runDecision(ctx context.Context, ss *sessionStream, req rt.CompletionRequest) {
	res := &tunnelv1.DecisionResult{RequestId: req.ID}
	answers, usage, err := c.decide(ctx, req)
	if err != nil {
		res.Error = err.Error()
		res.InvalidInput = rt.IsInvalidInput(err)
	} else {
		res.Answers = decisionAnswersToProto(answers)
		// Nothing is generated: completion_tokens stays 0.
		res.Usage = &typesv1.Usage{PromptTokens: uint32(usage.PromptTokens)}
	}
	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_DecisionResult{DecisionResult: res}})
}

func (c *Client) decide(ctx context.Context, req rt.CompletionRequest) ([]rt.DecisionAnswer, rt.Usage, error) {
	stream, err := c.o.Engine.Complete(ctx, req)
	if err != nil {
		return nil, rt.Usage{}, err
	}
	return drainDecision(stream)
}

// decisionChallenge answers a fingerprint probe for a decision model
// (Challenge.decision): the response carries the typed answers, which the
// coordinator compares against the expected probabilities within a
// tolerance; output and output_sha256 stay empty. A failed probe sends
// the bare response, as a failed text challenge does.
func (c *Client) decisionChallenge(ctx context.Context, ss *sessionStream, ch *tunnelv1.Challenge) {
	resp := &tunnelv1.ChallengeResponse{ChallengeId: ch.GetChallengeId()}
	answers, _, err := c.decide(ctx, rt.CompletionRequest{
		ID:       "challenge-" + ch.GetChallengeId(),
		Model:    ch.GetModelId(), // a decision model is rarely the node default
		Kind:     rt.KindDecision,
		Decision: decisionInputFromProto(ch.GetDecision()),
	})
	if err != nil {
		c.o.Log.Warn("decision challenge failed", "challenge_id", ch.GetChallengeId(), "model", ch.GetModelId(), "err", err)
	} else {
		resp.Answers = decisionAnswersToProto(answers)
	}
	_ = ss.send(&tunnelv1.NodeMessage{Msg: &tunnelv1.NodeMessage_ChallengeResponse{ChallengeResponse: resp}})
}

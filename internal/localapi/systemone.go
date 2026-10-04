package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/teraflock/flockd/internal/decision"
	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/governor"
	rt "github.com/teraflock/flockd/internal/runtime"
)

// ---- POST /v1/systemone ----
//
// Typed decisions (TypeSafe's "System One" API): bounded, typed questions
// about a `state`, answered with probabilities by a decision model. The
// contract is the mesh gateway's (proto design note
// 2026-10-03-decision-models) and is typed in api/openapi.yaml
// (SystemOneRequest / SystemOneResponse): same validation limits, answers
// in request order, `legend` rebuilt from the request, 422 for validation
// failures and runtime-rejected input, 404 for an unknown or non-decision
// model. No streaming.

func writeAPIError(w http.ResponseWriter, status int, typ, code, msg string) {
	var e oaError
	e.Error.Message = msg
	e.Error.Type = typ
	e.Error.Code = code
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "unreadable request body: "+err.Error())
		return
	}
	req, err := decision.ParseRequest(body)
	var verr *decision.ValidationError
	switch {
	case errors.As(err, &verr):
		writeOpenAIError(w, http.StatusUnprocessableEntity, "invalid_request_error", verr.Msg)
		return
	case err != nil:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: the request must be a JSON object")
		return
	}

	// `flock/<manifest id>` and the bare manifest id resolve to the quant
	// this node has; the response names the concrete id.
	requested := req.Model
	req.Model = s.resolveModel(r.Context(), req.Model)
	if !s.isDecisionModel(r.Context(), req.Model) {
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("model %q is not a decision model on this node; /v1/systemone serves models with decision: true (use /v1/chat/completions for chat models)", requested))
		return
	}

	stream, err := s.complete(r.Context(), rt.CompletionRequest{
		ID:       newRequestID(),
		Model:    req.Model,
		Kind:     rt.KindDecision,
		Decision: &req.Input,
	})
	if err != nil {
		writeDecisionError(w, req.Model, err)
		return
	}
	defer stream.Close()

	var (
		answers []rt.DecisionAnswer
		usage   rt.Usage
	)
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			writeDecisionError(w, req.Model, rerr)
			return
		}
		if chunk.Err != "" {
			writeOpenAIError(w, http.StatusInternalServerError, "server_error", chunk.Err)
			return
		}
		if chunk.Decision != nil {
			answers = chunk.Decision
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if chunk.Done {
			break
		}
	}

	out, err := decision.WriteResponse(req.Model, &req.Input, answers, usage)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// isDecisionModel reports whether id names a decision model this node can
// serve: one that is loaded, or one the catalog marks `decision` that is
// on disk (complete then loads it on demand). Anything else — unknown
// ids, chat and embedding models — is a 404.
func (s *Server) isDecisionModel(ctx context.Context, id string) bool {
	for _, m := range s.deps.Engine.Models() {
		if m.Spec.ID == id {
			return m.Spec.Decision
		}
	}
	if s.deps.ModelOps == nil || s.deps.Models == nil || !s.deps.Models.Has(id) {
		return false
	}
	entry, ok, err := s.deps.ModelOps.Lookup(ctx, id)
	return err == nil && ok && entry.Decision
}

// writeDecisionError maps a failed decision onto the public statuses.
func writeDecisionError(w http.ResponseWriter, model string, err error) {
	var nse governor.ErrNotServing
	switch {
	case rt.IsInvalidInput(err):
		// The runtime rejected the input itself (too many options for
		// this model, a prompt over its context).
		writeOpenAIError(w, http.StatusUnprocessableEntity, "invalid_request_error", err.Error())
	case errors.As(err, &nse):
		writeOpenAIError(w, http.StatusServiceUnavailable, "server_error",
			fmt.Sprintf("node is not serving right now (%s); retry shortly", nse.State))
	case errors.Is(err, engine.ErrModelNotFound):
		writeAPIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found",
			fmt.Sprintf("model %q is not available: %v", model, err))
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "server_error", err.Error())
	}
}

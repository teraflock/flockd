package localapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// ErrEnrollInProgress is returned by the Enroll dep while another enrollment
// is running; the handler maps it to 409.
var ErrEnrollInProgress = errors.New("enrollment already in progress")

// MeshStatus is the node's mesh membership, surfaced in /api/v1/status.
// Before enrollment NodeID is the key fingerprint; after, the
// coordinator-assigned id.
type MeshStatus struct {
	Enrolled      bool      `json:"enrolled"`
	NodeID        string    `json:"node_id"`
	CertExpiresAt time.Time `json:"cert_expires_at"`
}

// handleEnroll submits a claim code to the running daemon: enrollment (or
// re-enrollment — the one path that works while old credentials exist) plus
// a tunnel (re)start, no restart required.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if s.deps.Enroll == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error",
			"enrollment is not available on this node (standalone mode)")
		return
	}
	var body struct {
		ClaimCode string `json:"claim_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid body: "+err.Error())
		return
	}
	code := strings.TrimSpace(body.ClaimCode)
	if code == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "claim_code is required")
		return
	}
	if err := s.deps.Enroll(r.Context(), code); err != nil {
		if errors.Is(err, ErrEnrollInProgress) {
			writeOpenAIError(w, http.StatusConflict, "invalid_request_error", err.Error())
			return
		}
		// The coordinator rejected the code, or is unreachable — either
		// way the message is operator-actionable as-is.
		writeOpenAIError(w, http.StatusBadGateway, "server_error", err.Error())
		return
	}
	resp := map[string]any{"ok": true}
	if s.deps.Mesh != nil {
		m := s.deps.Mesh()
		resp["node_id"] = m.NodeID
		resp["cert_expires_at"] = m.CertExpiresAt
	}
	writeJSON(w, http.StatusOK, resp)
}

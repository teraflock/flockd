package localapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/models"
	"github.com/teraflock/flockd/internal/modelops"
)

// handleHealth is the one unauthenticated route: liveness for `tera up`,
// service managers, and the desktop app's daemon probe. It reveals nothing
// an unauthenticated local process couldn't learn from the port being open.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": s.deps.Version,
	})
}

// CatalogRow is a catalog entry merged with this node's local state.
type CatalogRow struct {
	models.CatalogModel
	Installed     bool   `json:"installed"`
	State         string `json:"state,omitempty"`
	ReceivedBytes int64  `json:"received_bytes,omitempty"`
	Loaded        bool   `json:"loaded"`
	Default       bool   `json:"default"`
}

func (s *Server) modelOps(w http.ResponseWriter) *modelops.Service {
	if s.deps.ModelOps == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error",
			"model operations need the llamacpp runtime (mock/vllm nodes manage models externally)")
		return nil
	}
	return s.deps.ModelOps
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	refresh, _ := strconv.ParseBool(r.URL.Query().Get("refresh"))
	cat, err := ops.Catalog(r.Context(), refresh)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "server_error", "catalog unavailable: "+err.Error())
		return
	}

	installed := map[string]models.Info{}
	if s.deps.Models != nil {
		for _, i := range s.deps.Models.List() {
			installed[i.ID] = i
		}
	}
	loaded := map[string]bool{}
	for _, m := range s.deps.Engine.Models() {
		loaded[m.Spec.ID] = true
	}
	def := s.deps.Engine.DefaultModel()

	rows := make([]CatalogRow, 0, len(cat.Models))
	for _, m := range cat.Models {
		row := CatalogRow{CatalogModel: m, Loaded: loaded[m.ID], Default: m.ID == def}
		if i, ok := installed[m.ID]; ok {
			row.Installed = i.State == "ready"
			row.State = i.State
			row.ReceivedBytes = i.ReceivedBytes
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": rows})
}

func (s *Server) handleModelDownload(w http.ResponseWriter, r *http.Request) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	id := r.PathValue("id")
	started, err := ops.StartDownload(r.Context(), id)
	switch {
	case errors.Is(err, modelops.ErrDownloadRunning):
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "state": "downloading"})
	case errors.Is(err, modelops.ErrUnknownModel), errors.Is(err, models.ErrInvalidID):
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
	case err != nil:
		writeOpenAIError(w, http.StatusBadGateway, "server_error", err.Error())
	case started:
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "state": "downloading"})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "ready"})
	}
}

func (s *Server) handleModelDownloadCancel(w http.ResponseWriter, r *http.Request) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if !ops.CancelDownload(r.PathValue("id")) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "no download in flight for that model")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleModelLoad(w http.ResponseWriter, r *http.Request) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	// Synchronous: llama-server startup is seconds, and the caller wants
	// the verdict. Downloads triggered implicitly here can be long — the
	// catalog UI downloads first, then loads.
	if err := ops.Load(r.Context(), r.PathValue("id")); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, modelops.ErrUnknownModel) || errors.Is(err, models.ErrInvalidID) {
			status = http.StatusNotFound
		}
		writeOpenAIError(w, status, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleModelUnload(w http.ResponseWriter, r *http.Request) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if err := ops.Unload(r.Context(), r.PathValue("id")); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleModelDefault(w http.ResponseWriter, r *http.Request) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if err := ops.SetDefault(r.PathValue("id")); err != nil {
		status := http.StatusNotFound
		if !errors.Is(err, engine.ErrModelNotFound) {
			status = http.StatusBadRequest
		}
		writeOpenAIError(w, status, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

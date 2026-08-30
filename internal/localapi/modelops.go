package localapi

import (
	"errors"
	"net/http"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
)

func (s *Server) modelOps(w http.ResponseWriter) *modelops.Service {
	if s.deps.ModelOps == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error",
			"model operations need the llamacpp runtime (mock has no model catalog to operate on)")
		return nil
	}
	return s.deps.ModelOps
}

// GetCatalog implements gen.ServerInterface.
func (s *Server) GetCatalog(w http.ResponseWriter, r *http.Request, params gen.GetCatalogParams) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	refresh := params.Refresh != nil && *params.Refresh
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

	// optStr maps "unset in a pre-display_name catalog" to an absent field
	// rather than an empty string the UI would render.
	optStr := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	rows := make([]gen.CatalogEntry, 0, len(cat.Models))
	for _, m := range cat.Models {
		row := gen.CatalogEntry{
			Id:            m.ID,
			Family:        m.Family,
			DisplayName:   optStr(m.DisplayName),
			ParamsB:       m.ParamsB,
			Quant:         m.Quant,
			Sha256:        m.SHA256,
			MinVramMb:     int64(m.MinVRAMMB),
			MinRamMb:      int64(m.MinRAMMB),
			License:       m.License,
			ArtifactUrl:   m.ArtifactURL,
			SizeBytes:     int64(m.SizeBytes),
			PayoutClass:   m.PayoutClass,
			ContextLength: int64(m.ContextLength),
			Embeddings:    m.Embeddings,
			Loaded:        loaded[m.ID],
			Default:       m.ID == def,
		}
		if i, ok := installed[m.ID]; ok {
			row.Installed = i.State == "ready"
			st := i.State
			row.State = &st
			if i.State == "downloading" {
				rb := i.ReceivedBytes
				row.ReceivedBytes = &rb
			}
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, gen.CatalogList{Models: rows})
}

// StartModelDownload implements gen.ServerInterface.
func (s *Server) StartModelDownload(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	started, err := ops.StartDownload(r.Context(), id)
	switch {
	case errors.Is(err, modelops.ErrDownloadRunning):
		writeJSON(w, http.StatusAccepted, gen.DownloadStatus{Ok: true, State: "downloading"})
	case errors.Is(err, modelops.ErrUnknownModel), errors.Is(err, models.ErrInvalidID):
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
	case err != nil:
		writeOpenAIError(w, http.StatusBadGateway, "server_error", err.Error())
	case started:
		writeJSON(w, http.StatusAccepted, gen.DownloadStatus{Ok: true, State: "downloading"})
	default:
		writeJSON(w, http.StatusOK, gen.DownloadStatus{Ok: true, State: "ready"})
	}
}

// CancelModelDownload implements gen.ServerInterface.
func (s *Server) CancelModelDownload(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if !ops.CancelDownload(id) {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "no download in flight for that model")
		return
	}
	writeJSON(w, http.StatusOK, gen.Ok{Ok: true})
}

// LoadModel implements gen.ServerInterface. Synchronous: llama-server
// startup is seconds, and the caller wants the verdict.
func (s *Server) LoadModel(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if err := ops.Load(r.Context(), id); err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, modelops.ErrUnknownModel) || errors.Is(err, models.ErrInvalidID) {
			status = http.StatusNotFound
		}
		writeOpenAIError(w, status, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gen.Ok{Ok: true})
}

// UnloadModel implements gen.ServerInterface.
func (s *Server) UnloadModel(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if err := ops.Unload(r.Context(), id); err != nil {
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gen.Ok{Ok: true})
}

// SetDefaultModel implements gen.ServerInterface.
func (s *Server) SetDefaultModel(w http.ResponseWriter, r *http.Request, id gen.ModelID) {
	ops := s.modelOps(w)
	if ops == nil {
		return
	}
	if err := ops.SetDefault(id); err != nil {
		status := http.StatusNotFound
		if !errors.Is(err, engine.ErrModelNotFound) {
			status = http.StatusBadRequest
		}
		writeOpenAIError(w, status, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, gen.Ok{Ok: true})
}

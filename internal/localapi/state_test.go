package localapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/models"
)

// A node with nothing loaded reports why (flockd#48): starting while the
// default model is on its way, no-model when it has nothing, idle when
// models are on disk but unloaded, serving once one is loaded.
func TestStatusStateWithNothingLoaded(t *testing.T) {
	_, _, deps := newOpsServerDeps(t)
	deps.Engine = engine.New(nil, nil, nil) // the harness pre-loads a mock; start empty
	deps.ModelOps.Eng = deps.Engine
	pending := true
	deps.BootPending = func() bool { return pending }
	srv := httptest.NewServer(New(deps).Handler())
	t.Cleanup(srv.Close)
	state := func() string {
		var st gen.Status
		if code := apiDo(t, srv, http.MethodGet, "/api/v1/status", "", &st); code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		return st.State
	}
	if got := state(); got != "starting" {
		t.Fatalf("pending default: %q, want starting", got)
	}
	pending = false
	if got := state(); got != "no-model" {
		t.Fatalf("nothing anywhere: %q, want no-model", got)
	}
	if err := deps.ModelOps.Fetch(context.Background(), "cat-model", models.OriginOperator); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "idle" {
		t.Fatalf("on disk, unloaded: %q, want idle", got)
	}
	if code := apiDo(t, srv, http.MethodPost, "/api/v1/models/cat-model/load", "", nil); code != http.StatusOK {
		t.Fatalf("load = %d", code)
	}
	if got := state(); got != "serving" {
		t.Fatalf("loaded: %q, want serving", got)
	}
}

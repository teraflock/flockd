package localapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teraflock/flockd/internal/localapi/gen"
)

// A chat request for a model that is on disk but not in memory (idle
// unloaded, or never loaded since startup) loads it and answers, instead of
// failing with "model not loaded". A model that is not on disk still fails.
func TestOpenAILoadsCachedModelOnDemand(t *testing.T) {
	srv, _ := newOpsServer(t)

	resp := apiPost(t, srv, "/api/v1/models/cat-model/download", "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("download = %d, want 202", resp.StatusCode)
	}
	deadline := time.After(5 * time.Second)
	for {
		resp = apiGet(t, srv, "/api/v1/models")
		var list struct {
			Models []gen.ModelRow `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var ready, loaded bool
		for _, m := range list.Models {
			if m.Id == "cat-model" {
				ready, loaded = m.State == "ready", m.Loaded
			}
		}
		if ready {
			if loaded {
				t.Fatal("download must not load the model")
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("model never ready: %+v", list.Models)
		case <-time.After(10 * time.Millisecond):
		}
	}

	chat := func(model string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"max_tokens":4}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp = chat("cat-model")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("chat against cached model = %d, want 200 (load on demand)", resp.StatusCode)
	}
	resp = apiGet(t, srv, "/api/v1/models")
	var list struct {
		Models []gen.ModelRow `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for _, m := range list.Models {
		if m.Id == "cat-model" && !m.Loaded {
			t.Fatal("model not loaded after the on-demand request")
		}
	}

	resp = chat("not-on-disk")
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("chat against unknown model = %d, want 404", resp.StatusCode)
	}
}

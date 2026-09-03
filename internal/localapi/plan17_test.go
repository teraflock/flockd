package localapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/teraflock/flockd/internal/activity"
	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/update"
)

func apiDo(t *testing.T, srv *httptest.Server, method, path, body string, out any) int {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestStatusCarriesMemoryAndDisk(t *testing.T) {
	srv, _ := newOpsServer(t)
	var st gen.Status
	if code := apiDo(t, srv, http.MethodGet, "/api/v1/status", "", &st); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if st.Disk.Dir == "" || st.Disk.FreeBytes <= 0 {
		t.Fatalf("disk = %+v", st.Disk)
	}
	if st.Memory.UsedMb != 0 || st.Update != nil {
		t.Fatalf("memory/update = %+v %+v", st.Memory, st.Update)
	}
	// Load the catalog model: it shows loaded_mb (estimate), idle_since and
	// path on its row, and status memory used follows.
	if code := apiDo(t, srv, http.MethodPost, "/api/v1/models/cat-model/load", "", nil); code != http.StatusOK {
		t.Fatalf("load = %d", code)
	}
	var ml gen.ModelList
	apiDo(t, srv, http.MethodGet, "/api/v1/models", "", &ml)
	var row *gen.ModelRow
	for i := range ml.Models {
		if ml.Models[i].Id == "cat-model" {
			row = &ml.Models[i]
		}
	}
	if row == nil || !row.Loaded || row.LoadedMb == nil || *row.LoadedMb != 4096 || row.IdleSince == nil || row.Path == nil {
		t.Fatalf("row = %+v", row)
	}
	apiDo(t, srv, http.MethodGet, "/api/v1/status", "", &st)
	if st.Memory.UsedMb != 4096 {
		t.Fatalf("memory used = %d", st.Memory.UsedMb)
	}
	// Delete the file underneath: the row turns missing, path gone.
	if err := os.Remove(*row.Path); err != nil {
		t.Fatal(err)
	}
	var after gen.ModelList // fresh: json reuses slice elements otherwise
	apiDo(t, srv, http.MethodGet, "/api/v1/models", "", &after)
	for _, r := range after.Models {
		if r.Id == "cat-model" && (r.State != "missing" || r.Path != nil) {
			t.Fatalf("after delete: %+v", r)
		}
	}
}

func TestLimitsCarryModelAndMemoryKnobs(t *testing.T) {
	srv, dir := newOpsServer(t)
	_ = srv
	// A governor-backed server with the same models/ops deps as newOpsServer
	// is what the daemon runs; build one on the same data dir.
	gsrv := newTestServerWithDataDir(t, dir)
	var lim gen.Limits
	if code := apiDo(t, gsrv, http.MethodPut, "/api/v1/limits",
		`{"serve_policy":"always","max_disk_mb":2048,"retention_days":30,"max_ram_mb":8192,"idle_unload_seconds":120}`, &lim); code != http.StatusOK {
		t.Fatalf("put = %d", code)
	}
	if lim.MaxDiskMb == nil || *lim.MaxDiskMb != 2048 || lim.RetentionDays == nil || *lim.RetentionDays != 30 ||
		lim.MaxRamMb == nil || *lim.MaxRamMb != 8192 || lim.IdleUnloadSeconds == nil || *lim.IdleUnloadSeconds != 120 {
		t.Fatalf("limits = %+v", lim)
	}
	raw, err := os.ReadFile(config.LimitsPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"max_disk_mb = 2048", "retention_days = 30", "idle_unload_s = 120", "max_ram_mb = 8192"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("overlay missing %q:\n%s", want, raw)
		}
	}
	if code := apiDo(t, gsrv, http.MethodPut, "/api/v1/limits", `{"serve_policy":"always","max_disk_mb":-1}`, nil); code != http.StatusBadRequest {
		t.Fatalf("negative budget = %d", code)
	}
}

func TestActivityAndUpdateRoutes(t *testing.T) {
	ring := activity.New(10)
	ring.Record(activity.KindDownloadStarted, activity.ActorMesh, "m", "mesh started downloading m", "")
	ring.Record(activity.KindDeclined, activity.ActorMesh, "n", "declined n", "over budget")
	feedBody := `{"flockd":{"latest":"9.9.9","minimum":"0.1.0","url":"https://example/rel"}}`
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/404" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(feedBody))
	}))
	t.Cleanup(feed.Close)
	upd := &update.Checker{FeedURL: feed.URL + "/404", Current: "0.3.0", Log: quietLog()}
	s := New(Deps{
		Engine: newTestServerDeps(t).Engine, Log: quietLog(), NodeID: "n", Version: "0.3.0", Token: testToken,
		Activity: ring, Update: upd,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	var al gen.ActivityList
	if code := apiDo(t, srv, http.MethodGet, "/api/v1/activity", "", &al); code != http.StatusOK {
		t.Fatalf("activity = %d", code)
	}
	if len(al.Events) != 2 || al.Events[0].Kind != activity.KindDeclined || al.Events[0].Detail == nil || al.Events[1].Model == nil || *al.Events[1].Model != "m" {
		t.Fatalf("activity = %+v", al.Events)
	}
	// Feed missing: the manual check says so; status shows no update.
	if code := apiDo(t, srv, http.MethodPost, "/api/v1/update/check", "", nil); code != http.StatusBadGateway {
		t.Fatalf("check against 404 feed = %d", code)
	}
	var st gen.Status
	apiDo(t, srv, http.MethodGet, "/api/v1/status", "", &st)
	if st.Update != nil {
		t.Fatalf("update after failed check = %+v", st.Update)
	}
	upd.FeedURL = feed.URL
	var u gen.Update
	if code := apiDo(t, srv, http.MethodPost, "/api/v1/update/check", "", &u); code != http.StatusOK {
		t.Fatalf("check = %d", code)
	}
	if !u.Available || u.Latest != "9.9.9" || u.Url == nil || u.BelowMinimum != nil {
		t.Fatalf("update = %+v", u)
	}
	apiDo(t, srv, http.MethodGet, "/api/v1/status", "", &st)
	if st.Update == nil || !st.Update.Available {
		t.Fatalf("status.update = %+v", st.Update)
	}
}

// newTestServerDeps returns minimal deps with a mock-backed engine.
func newTestServerDeps(t *testing.T) Deps {
	t.Helper()
	srv := newTestServer(t, nil)
	_ = srv
	return Deps{Engine: newOpsEngine(t)}
}

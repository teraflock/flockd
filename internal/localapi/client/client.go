// Package client is the loopback client of the daemon's local API on
// localhost:7777 — the one client the tera CLI, the TUI, `tera mcp` and
// any Go tooling share (SPEC §A1.2: CLI, TUI and web dash are all clients
// of one API). The management half is generated from api/openapi.yaml
// (internal/localapi/gen, `make gen`), so it cannot drift from the daemon;
// this package adds what codegen cannot express: token and data-dir
// resolution, user-facing errors, the SSE event stream, and the
// OpenAI-compatible chat helper.
//
// It is the loopback client only — the mesh tunnel client lives in
// internal/tunnel.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teraflock/flockd/internal/config"
	"github.com/teraflock/flockd/internal/localapi/gen"
)

// DefaultBase is where flockd listens unless local_api.listen says otherwise.
const DefaultBase = "http://127.0.0.1:7777"

// TokenFile is the per-install bearer token's file name inside the data dir.
const TokenFile = "local_api_token"

// Options is what a caller can pin explicitly (CLI flags); everything left
// empty is resolved the way `tera` always has.
type Options struct {
	// API is the daemon base URL (default DefaultBase).
	API string
	// DataDir is an explicit data directory (the --data-dir flag). Empty
	// resolves it like flockd does: config.toml -> FLOCKD_DATA_DIR ->
	// ~/.teraflock. The desktop app mirrors that order in Rust; the two
	// must keep agreeing.
	DataDir string
	// Token is an explicit bearer token (the --token flag). Empty falls
	// back to $TERA_TOKEN, then <DataDir>/local_api_token.
	Token string
}

// Resolved is the outcome of Resolve: where the client will talk, with
// which token, and where that token came from (for error messages).
type Resolved struct {
	Base    string
	Token   string
	DataDir string
	// TokenSource says where the token was found — "--token", "$TERA_TOKEN",
	// the file path — or, when none was, where it was looked for. It is
	// user-facing: a 401 quotes it.
	TokenSource string
}

// DataDir resolves the daemon's data directory the same way flockd does
// (defaults <- TOML <- FLOCKD_* env), so that files exchanged between the
// two processes — the claim code, the local API token — land where the
// daemon actually looks. Falling back to the built-in default would
// silently break enrollment for anyone who moved data_dir. An explicit
// value (the --data-dir flag) wins.
func DataDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	cfg, err := config.Load("")
	if err != nil {
		return config.Default().DataDir
	}
	return cfg.DataDir
}

// Resolve applies the precedence rules: base URL from Options.API (default
// DefaultBase); token from Options.Token, then $TERA_TOKEN, then
// <dataDir>/local_api_token. The token is per install and lives beside the
// daemon's other state, so pointing at the wrong data dir is the usual
// cause of a 401 — TokenSource records where we looked so the error can
// say so.
func Resolve(o Options) Resolved {
	r := Resolved{Base: strings.TrimSuffix(o.API, "/"), DataDir: DataDir(o.DataDir)}
	if r.Base == "" {
		r.Base = DefaultBase
	}
	switch {
	case o.Token != "":
		r.Token, r.TokenSource = strings.TrimSpace(o.Token), "--token"
	case os.Getenv("TERA_TOKEN") != "":
		r.Token, r.TokenSource = strings.TrimSpace(os.Getenv("TERA_TOKEN")), "$TERA_TOKEN"
	default:
		path := filepath.Join(r.DataDir, TokenFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			r.TokenSource = fmt.Sprintf("no token found at %s", path)
			break
		}
		r.Token, r.TokenSource = strings.TrimSpace(string(raw)), path
	}
	return r
}

// Client talks to one daemon. Mgmt is the generated management client
// (every /api/v1 route in the spec); the typed helpers below wrap the
// routes tera uses and turn non-2xx responses into user-facing errors.
type Client struct {
	Resolved

	// Mgmt is the generated client with a 10s deadline per request.
	Mgmt *gen.ClientWithResponses
	// MgmtLong is the same client without a deadline: synchronous loads
	// (llama-server startup, download-then-load) outlive any sane fixed
	// timeout, so callers bound those with a context instead.
	MgmtLong *gen.ClientWithResponses

	hc   *http.Client // 10s
	long *http.Client // none; SSE and streaming chat use it
}

// New resolves Options and builds the client. It never fails on a missing
// token — the daemon's 401 says where the token was looked for — so a
// probe of an unauthenticated route still works.
func New(o Options) (*Client, error) {
	return FromResolved(Resolve(o))
}

// FromResolved builds a client from an already-resolved endpoint (tests,
// or callers that resolved once and want several clients).
func FromResolved(r Resolved) (*Client, error) {
	c := &Client{
		Resolved: r,
		hc:       &http.Client{Timeout: 10 * time.Second},
		long:     &http.Client{},
	}
	var err error
	if c.Mgmt, err = gen.NewClientWithResponses(r.Base, gen.WithHTTPClient(doer{c.hc, r.Base}), gen.WithRequestEditorFn(c.bearer)); err != nil {
		return nil, err
	}
	if c.MgmtLong, err = gen.NewClientWithResponses(r.Base, gen.WithHTTPClient(doer{c.long, r.Base}), gen.WithRequestEditorFn(c.bearer)); err != nil {
		return nil, err
	}
	return c, nil
}

// bearer is the RequestEditorFn that authenticates management requests.
func (c *Client) bearer(_ context.Context, req *http.Request) error {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return nil
}

// doer wraps an http.Client so transport failures read as "the daemon is
// not there" rather than a raw dial error, while a cancelled context stays
// a context error (callers distinguish "stopped" from "lost").
type doer struct {
	hc   *http.Client
	base string
}

func (d doer) Do(req *http.Request) (*http.Response, error) {
	resp, err := d.hc.Do(req)
	if err != nil {
		if cerr := req.Context().Err(); cerr != nil {
			return nil, cerr
		}
		return nil, &UnreachableError{Base: d.base, Err: err}
	}
	return resp, nil
}

// UnreachableError means no daemon answered at Base (not running, wrong
// --api, or a request that timed out).
type UnreachableError struct {
	Base string
	Err  error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("cannot reach flockd at %s (is it running? try `tera up` or `flockd --standalone`): %v", e.Base, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// APIError is a non-2xx answer from the daemon: the status and the
// message from its `{"error":{"message","type"}}` body (both halves of
// the API use that shape). Status 401 renders the token-source
// explanation instead — that error is about the client's setup, not the
// request.
type APIError struct {
	Status  int
	Type    string
	Message string

	tokenSource, dataDir string
}

func (e *APIError) Error() string {
	if e.Status == http.StatusUnauthorized {
		return fmt.Sprintf("daemon rejected the auth token (%s).\n"+
			"  The token is per-install and lives in the daemon's data dir.\n"+
			"  If flockd runs with a custom data dir, point tera at the same one:\n"+
			"    tera --data-dir %s <command>   (or set FLOCKD_DATA_DIR / TERA_TOKEN)\n"+
			"  Print the current token with: tera token", e.tokenSource, e.dataDir)
	}
	return fmt.Sprintf("daemon error (%d): %s", e.Status, e.Message)
}

// NotServing reports the governor's refusal (503 on /v1): the node is
// yielded, on battery, too hot, or outside its schedule.
func (e *APIError) NotServing() bool { return e.Status == http.StatusServiceUnavailable }

// Check turns a response status + body into nil (2xx) or an *APIError.
// Every typed helper goes through it; callers using Mgmt directly can too.
func (c *Client) Check(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	e := &APIError{Status: status, tokenSource: c.TokenSource, dataDir: c.DataDir}
	var ge gen.Error
	if json.Unmarshal(body, &ge) == nil && ge.Error.Message != "" {
		e.Message, e.Type = ge.Error.Message, ge.Error.Type
	} else {
		e.Message = strings.TrimSpace(string(body))
	}
	return e
}

// Remedy is the one-line hint tera prints under an error from this
// package: what to run to fix it. Empty when there is nothing to suggest.
func Remedy(err error) string {
	var ue *UnreachableError
	if errors.As(err, &ue) {
		return "start the daemon with `tera up` (or `flockd --standalone --runtime=mock` to try it without an account)"
	}
	var ae *APIError
	if errors.As(err, &ae) {
		switch ae.Status {
		case http.StatusServiceUnavailable:
			return "the governor is holding inference back — under `serve_policy = idle-only` the node only serves while you are away from the keyboard; " +
				"serve now with `tera limits --serve always` (or wait for the node to go idle)"
		case http.StatusNotFound:
			return "install the model first: `tera models pull <id>` (see `tera models list` for what is on this node)"
		case http.StatusNotImplemented:
			return "this node runs the mock runtime; model operations need `runtime.kind = \"llamacpp\"` in config.toml"
		}
	}
	return ""
}

// value is the shared tail of every typed helper: transport error, then
// Check, then the decoded 2xx body.
func value[T any](c *Client, err error, status int, body []byte, v *T) (T, error) {
	var zero T
	if err != nil {
		return zero, err
	}
	if err := c.Check(status, body); err != nil {
		return zero, err
	}
	if v == nil {
		// The generated client only decodes bodies labelled application/json;
		// a daemon behind a proxy that rewrites Content-Type still sends
		// JSON, so try before giving up.
		if len(body) == 0 || json.Unmarshal(body, &zero) != nil {
			return zero, fmt.Errorf("daemon answered %d without the expected JSON body", status)
		}
		return zero, nil
	}
	return *v, nil
}

// ok is value for routes that answer {ok: true}.
func (c *Client) ok(err error, status int, body []byte) error {
	if err != nil {
		return err
	}
	return c.Check(status, body)
}

// ---- typed helpers over the generated management client ----

// Health probes the unauthenticated liveness route.
func (c *Client) Health(ctx context.Context) (gen.Health, error) {
	r, err := c.Mgmt.GetHealthWithResponse(ctx)
	if err != nil {
		return gen.Health{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// Status is GET /api/v1/status.
func (c *Client) Status(ctx context.Context) (gen.Status, error) {
	r, err := c.Mgmt.GetStatusWithResponse(ctx)
	if err != nil {
		return gen.Status{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// Models is GET /api/v1/models: the local cache merged with runtime state.
func (c *Client) Models(ctx context.Context) (gen.ModelList, error) {
	r, err := c.Mgmt.ListModelsWithResponse(ctx)
	if err != nil {
		return gen.ModelList{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// FindModel returns the row for id from the model list (ok=false when the
// daemon does not know it).
func (c *Client) FindModel(ctx context.Context, id string) (gen.ModelRow, bool, error) {
	ml, err := c.Models(ctx)
	if err != nil {
		return gen.ModelRow{}, false, err
	}
	for _, m := range ml.Models {
		if m.Id == id {
			return m, true, nil
		}
	}
	return gen.ModelRow{}, false, nil
}

// Catalog is GET /api/v1/catalog (refresh bypasses the daemon's cache).
func (c *Client) Catalog(ctx context.Context, refresh bool) (gen.CatalogList, error) {
	var params gen.GetCatalogParams
	if refresh {
		params.Refresh = &refresh
	}
	r, err := c.Mgmt.GetCatalogWithResponse(ctx, &params)
	if err != nil {
		return gen.CatalogList{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// Earnings is GET /api/v1/earnings.
func (c *Client) Earnings(ctx context.Context) (gen.Earnings, error) {
	r, err := c.Mgmt.GetEarningsWithResponse(ctx)
	if err != nil {
		return gen.Earnings{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// Limits is GET /api/v1/limits.
func (c *Client) Limits(ctx context.Context) (gen.Limits, error) {
	r, err := c.Mgmt.GetLimitsWithResponse(ctx)
	if err != nil {
		return gen.Limits{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// UpdateLimits is PUT /api/v1/limits; returns the applied limits.
func (c *Client) UpdateLimits(ctx context.Context, lim gen.Limits) (gen.Limits, error) {
	r, err := c.Mgmt.UpdateLimitsWithResponse(ctx, lim)
	if err != nil {
		return gen.Limits{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// Logs is GET /api/v1/logs?n=: the last n entries of the ring, oldest
// first (n <= 0 leaves the daemon's default).
func (c *Client) Logs(ctx context.Context, n int) ([]gen.LogEntry, error) {
	var params gen.GetLogsParams
	if n > 0 {
		params.N = &n
	}
	r, err := c.Mgmt.GetLogsWithResponse(ctx, &params)
	if err != nil {
		return nil, err
	}
	ll, err := value(c, nil, r.StatusCode(), r.Body, r.JSON200)
	return ll.Logs, err
}

// Activity is GET /api/v1/activity.
func (c *Client) Activity(ctx context.Context) (gen.ActivityList, error) {
	r, err := c.Mgmt.GetActivityWithResponse(ctx)
	if err != nil {
		return gen.ActivityList{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// PinModel is POST /api/v1/models/{id}/pin.
func (c *Client) PinModel(ctx context.Context, id string, pinned bool) error {
	r, err := c.Mgmt.PinModelWithResponse(ctx, id, gen.PinRequest{Pinned: pinned})
	if err != nil {
		return err
	}
	return c.ok(nil, r.StatusCode(), r.Body)
}

// DeleteModel is DELETE /api/v1/models/{id}.
func (c *Client) DeleteModel(ctx context.Context, id string) error {
	r, err := c.Mgmt.DeleteModelWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return c.ok(nil, r.StatusCode(), r.Body)
}

// LoadModel is POST /api/v1/models/{id}/load. Synchronous on the daemon
// side and legitimately slow: no deadline, bound it with ctx.
func (c *Client) LoadModel(ctx context.Context, id string) error {
	r, err := c.MgmtLong.LoadModelWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return c.ok(nil, r.StatusCode(), r.Body)
}

// UnloadModel is POST /api/v1/models/{id}/unload.
func (c *Client) UnloadModel(ctx context.Context, id string) error {
	r, err := c.Mgmt.UnloadModelWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return c.ok(nil, r.StatusCode(), r.Body)
}

// SetDefaultModel is POST /api/v1/models/{id}/default.
func (c *Client) SetDefaultModel(ctx context.Context, id string) error {
	r, err := c.Mgmt.SetDefaultModelWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return c.ok(nil, r.StatusCode(), r.Body)
}

// StartDownload is POST /api/v1/models/{id}/download: State is
// "downloading" (202, started or already running) or "ready" (200).
func (c *Client) StartDownload(ctx context.Context, id string) (gen.DownloadStatus, error) {
	r, err := c.Mgmt.StartModelDownloadWithResponse(ctx, id)
	if err != nil {
		return gen.DownloadStatus{}, err
	}
	v := r.JSON200
	if r.JSON202 != nil {
		v = r.JSON202
	}
	return value(c, nil, r.StatusCode(), r.Body, v)
}

// CancelDownload is DELETE /api/v1/models/{id}/download.
func (c *Client) CancelDownload(ctx context.Context, id string) error {
	r, err := c.Mgmt.CancelModelDownloadWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return c.ok(nil, r.StatusCode(), r.Body)
}

// Enroll is POST /api/v1/enroll: enrollment plus a live tunnel (re)start.
func (c *Client) Enroll(ctx context.Context, claimCode string) (gen.EnrollResponse, error) {
	r, err := c.MgmtLong.EnrollNodeWithResponse(ctx, gen.EnrollRequest{ClaimCode: claimCode})
	if err != nil {
		return gen.EnrollResponse{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// CheckUpdate is POST /api/v1/update/check.
func (c *Client) CheckUpdate(ctx context.Context) (gen.Update, error) {
	r, err := c.Mgmt.CheckUpdateWithResponse(ctx)
	if err != nil {
		return gen.Update{}, err
	}
	return value(c, nil, r.StatusCode(), r.Body, r.JSON200)
}

// Package localapi is the daemon's loopback HTTP surface on
// localhost:7777: the OpenAI-compatible /v1 endpoints (Phase 0 exit
// criterion) and the management /api/v1 REST+SSE API that the CLI, TUI and
// embedded web dashboard all consume (SPEC §A1.2: one source of truth).
package localapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teraflock/flockd/internal/engine"
	"github.com/teraflock/flockd/internal/events"
	"github.com/teraflock/flockd/internal/governor"
	"github.com/teraflock/flockd/internal/localapi/gen"
	"github.com/teraflock/flockd/internal/logging"
	"github.com/teraflock/flockd/internal/modelops"
	"github.com/teraflock/flockd/internal/models"
	typesv1 "github.com/teraflock/proto/gen/go/flock/types/v1"
)

// Deps wires the server to the daemon internals.
type Deps struct {
	Engine     *engine.Engine
	Governor   *governor.Governor // may be nil
	Models     *models.Manager    // may be nil
	ModelOps   *modelops.Service  // may be nil (mock runtime)
	Events     *events.Hub        // may be nil (SSE then ticks status only)
	LogRing    *logging.Ring      // may be nil
	Hardware   *typesv1.CapabilityProfile
	Log        *slog.Logger
	WebFS      fs.FS // embedded dashboard dist; may be nil
	NodeID     string
	Version    string
	Standalone bool
	// DataDir persists live limit edits (limits.toml overlay); empty
	// disables persistence.
	DataDir string
	// Mesh reports live mesh membership (enrolled, node id, cert expiry).
	// Nil falls back to the static NodeID with enrolled=false.
	Mesh func() MeshStatus
	// Enroll submits a claim code to the running daemon: enrollment plus
	// tunnel (re)start. Nil (standalone) answers 501.
	Enroll func(ctx context.Context, claimCode string) error
	// RequireAuthV1 extends bearer auth to the OpenAI /v1 endpoints.
	RequireAuthV1 bool
	// Token authenticates /api/v1 (and /v1 when RequireAuthV1).
	Token string
}

// Server is the loopback HTTP server.
type Server struct {
	deps  Deps
	mux   *http.ServeMux
	srv   *http.Server
	start time.Time
}

// New assembles routes.
func New(deps Deps) *Server {
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	s := &Server{deps: deps, mux: http.NewServeMux(), start: time.Now()}

	// OpenAI-compatible surface.
	s.mux.HandleFunc("GET /v1/models", s.authV1(s.handleListModels))
	s.mux.HandleFunc("POST /v1/chat/completions", s.authV1(s.handleChatCompletions))
	s.mux.HandleFunc("POST /v1/completions", s.authV1(s.handleCompletions))
	s.mux.HandleFunc("POST /v1/embeddings", s.authV1(s.handleEmbeddings))

	// Daemon management API: routes come from api/openapi.yaml via
	// oapi-codegen (make gen) — the spec is the router, so the two cannot
	// drift. Auth is a middleware over every generated route; /health is
	// exempted there (probes run before anyone holds a token).
	gen.HandlerWithOptions(s, gen.StdHTTPServerOptions{
		BaseRouter:  s.mux,
		Middlewares: []gen.MiddlewareFunc{s.authManagement},
	})

	// SSE is hand-written (streaming + ?token= auth for EventSource);
	// documented in the spec under the `events` tag.
	s.mux.HandleFunc("GET /api/v1/events", s.authSSE(s.handleEvents))

	// Embedded web dashboard at the root.
	if deps.WebFS != nil {
		s.mux.Handle("GET /", http.FileServerFS(deps.WebFS))
	}
	return s
}

// Handler exposes the mux (tests).
func (s *Server) Handler() http.Handler { return s.mux }

// ListenAndServe binds addr (must be loopback unless the operator opted
// out in config, which docs warn about) and serves until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	if !isLoopback(addr) {
		s.deps.Log.Warn("local API bound to a non-loopback address — the management API and OpenAI endpoints are now reachable from your network", "addr", addr)
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("localapi: listen %s: %w", addr, err)
	}
	s.srv = &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.Serve(lis) }()
	s.deps.Log.Info("local API listening", "addr", lis.Addr().String())
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---- auth ----

// presentedToken extracts the bearer token from the request. Surrounding
// whitespace is trimmed: operators paste this token by hand out of a file or
// a terminal, and a stray leading space produced an indistinguishable
// "invalid token" before. The `token` query parameter is accepted only for
// SSE, where the browser's EventSource cannot set headers at all.
func presentedToken(r *http.Request, allowQuery bool) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		rest, ok := cutPrefixFold(h, "bearer")
		// The scheme must be followed by whitespace, so "Bearerxyz" is not
		// read as the token "xyz"; any amount of it is then tolerated.
		if !ok || rest == "" || !isSpace(rest[0]) {
			return "" // an Authorization header that is not Bearer is not ours
		}
		return strings.TrimSpace(rest)
	}
	if allowQuery {
		return strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return ""
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' }

// cutPrefixFold is strings.CutPrefix with an ASCII case-insensitive prefix
// match ("Bearer", "bearer" and "BEARER" are all valid per RFC 6750).
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

func (s *Server) checkToken(r *http.Request) bool {
	return s.checkTokenOpts(r, false)
}

func (s *Server) checkTokenOpts(r *http.Request, allowQuery bool) bool {
	got := presentedToken(r, allowQuery)
	return s.deps.Token != "" &&
		subtle.ConstantTimeCompare([]byte(got), []byte(s.deps.Token)) == 1
}

// authAPI always requires the bearer token.
func (s *Server) authAPI(next http.HandlerFunc) http.HandlerFunc {
	return s.authAPIOpts(next, false)
}

// authManagement is the middleware over every generated management route.
// /health is exempt: probes (`tera up`, the desktop app, service managers)
// run before anyone holds a token, and it reveals nothing an unauthenticated
// local process couldn't learn from the port being open.
func (s *Server) authManagement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		s.authAPI(next.ServeHTTP)(w, r)
	})
}

// authSSE additionally accepts `?token=`, because EventSource cannot send an
// Authorization header. Loopback-only, and the token is the same per-install
// secret — but it does land in browser history, so it stays limited to the
// one endpoint that needs it.
func (s *Server) authSSE(next http.HandlerFunc) http.HandlerFunc {
	return s.authAPIOpts(next, true)
}

func (s *Server) authAPIOpts(next http.HandlerFunc, allowQuery bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkTokenOpts(r, allowQuery) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error",
				"missing or invalid bearer token (run `tera token` to print it, or read local_api_token in the daemon's data dir)")
			return
		}
		next(w, r)
	}
}

// authV1 requires the token only when configured (loopback-keyless default
// so OPENAI_BASE_URL works with any placeholder api key).
func (s *Server) authV1(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.deps.RequireAuthV1 && !s.checkToken(r) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

// LoadOrCreateToken returns the per-install bearer token, generating it
// with 0600 on first run.
//
// TODO(keychain): store in macOS Keychain / Windows DPAPI / secret service
// instead of a file (SPEC §A1.2); file+0600 is the Phase 0 baseline.
func LoadOrCreateToken(dataDir string) (string, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("localapi: mkdir: %w", err)
	}
	path := filepath.Join(dataDir, "local_api_token")
	raw, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(raw)), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("localapi: read token: %w", err)
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("localapi: entropy: %w", err)
	}
	tok := "flock_" + hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("localapi: write token: %w", err)
	}
	return tok, nil
}

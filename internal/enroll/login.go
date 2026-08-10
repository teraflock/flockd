package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// LoginFlow implements the `hive login` browser handoff: a PKCE-style
// loopback callback that receives the claim code from the signup page.
//
//  1. generate code_verifier + S256 challenge
//  2. open <login_url>?redirect_uri=http://127.0.0.1:<port>/callback&code_challenge=...
//  3. the web page (after auth) redirects to the loopback with
//     ?claim_code=...&state=...
//  4. daemon uses the claim code in the Enroll RPC.
type LoginFlow struct {
	// LoginURL is the browser page (config enroll.login_url).
	LoginURL string
	// OpenBrowser launches the URL; overridable in tests. Nil = xdg-open/open.
	OpenBrowser func(url string) error
}

// Result of a completed login flow.
type Result struct {
	ClaimCode string
}

// randB64 returns n random bytes, base64url without padding.
func randB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("enroll: entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Run executes the flow, blocking until the callback arrives or ctx ends.
// It returns the URL it opened (for "visit this URL" fallback output).
func (f *LoginFlow) Run(ctx context.Context) (*Result, string, error) {
	verifier, err := randB64(32)
	if err != nil {
		return nil, "", err
	}
	state, err := randB64(16)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("enroll: loopback listener: %w", err)
	}
	defer lis.Close()

	redirect := fmt.Sprintf("http://%s/callback", lis.Addr().String())

	u, err := url.Parse(f.LoginURL)
	if err != nil {
		return nil, "", fmt.Errorf("enroll: bad login url: %w", err)
	}
	q := u.Query()
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	u.RawQuery = q.Encode()
	openURL := u.String()

	type callback struct {
		code string
		err  error
	}
	got := make(chan callback, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			got <- callback{err: fmt.Errorf("enroll: state mismatch in callback")}
			return
		}
		code := r.URL.Query().Get("claim_code")
		if code == "" {
			http.Error(w, "missing claim_code", http.StatusBadRequest)
			got <- callback{err: fmt.Errorf("enroll: callback missing claim_code")}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body><h2>HiveGrid: node claimed.</h2>You can close this tab and return to the terminal.</body></html>"))
		got <- callback{code: code}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(lis) }()
	defer srv.Close()

	if f.OpenBrowser != nil {
		if err := f.OpenBrowser(openURL); err != nil {
			// Non-fatal: caller prints the URL for manual visiting.
			_ = err
		}
	} else if err := openBrowser(openURL); err != nil {
		_ = err
	}

	select {
	case cb := <-got:
		if cb.err != nil {
			return nil, openURL, cb.err
		}
		// The verifier would be sent alongside the claim code at enrollment
		// so the control plane can bind challenge->verifier. The fake
		// coordinator accepts any code; wiring verifier passthrough is part
		// of the Phase 1 control-plane work.
		return &Result{ClaimCode: cb.code}, openURL, nil
	case <-ctx.Done():
		return nil, openURL, fmt.Errorf("enroll: login cancelled: %w", ctx.Err())
	}
}

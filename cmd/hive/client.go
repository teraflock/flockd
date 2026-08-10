package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// apiClient talks to the daemon's local API. Every hive command is a client
// of localhost:7777/api/v1 — one source of truth (SPEC §A1.2).
type apiClient struct {
	base  string
	token string
	hc    *http.Client
}

func newAPIClient(base, dataDir string) (*apiClient, error) {
	token := ""
	raw, err := os.ReadFile(filepath.Join(dataDir, "local_api_token"))
	if err == nil {
		token = strings.TrimSpace(string(raw))
	}
	return &apiClient{
		base:  strings.TrimSuffix(base, "/"),
		token: token,
		hc:    &http.Client{Timeout: 10 * time.Second},
	}, nil
}

func (c *apiClient) do(method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach hived at %s (is it running? try `hive up` or `hived --standalone`): %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		raw, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, e.Error.Message)
		}
		return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *apiClient) get(path string, out any) error { return c.do(http.MethodGet, path, nil, out) }
func (c *apiClient) put(path string, body, out any) error {
	return c.do(http.MethodPut, path, body, out)
}
func (c *apiClient) post(path string, body, out any) error {
	return c.do(http.MethodPost, path, body, out)
}
func (c *apiClient) del(path string) error { return c.do(http.MethodDelete, path, nil, nil) }

// ---- shared response shapes (mirror internal/localapi) ----

type statusResp struct {
	NodeID        string  `json:"node_id"`
	Version       string  `json:"version"`
	Standalone    bool    `json:"standalone"`
	State         string  `json:"state"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	DefaultModel  string  `json:"default_model"`
	ModelsLoaded  int     `json:"models_loaded"`
	Inflight      int     `json:"inflight"`
	OnBattery     bool    `json:"on_battery"`
	TempCelsius   float64 `json:"temp_celsius"`
	Hardware      *struct {
		OS       string `json:"os"`
		Arch     string `json:"arch"`
		CPUModel string `json:"cpu_model"`
		CPUCores uint32 `json:"cpu_cores"`
		RAMMB    uint64 `json:"ram_mb"`
		GPUs     []struct {
			Vendor  string `json:"vendor"`
			Model   string `json:"model"`
			VRAMMB  uint64 `json:"vram_mb"`
			Accel   string `json:"accel"`
			Unified bool   `json:"unified_memory"`
		} `json:"gpus"`
	} `json:"hardware"`
	Stats struct {
		TokensPerSec1m  float64 `json:"tokens_per_sec_1m"`
		RequestsPerMin  float64 `json:"requests_per_min"`
		TotalRequests   int64   `json:"total_requests"`
		TotalTokens     int64   `json:"total_tokens"`
		Inflight        int     `json:"inflight"`
		EarnedMicrocred int64   `json:"earned_microcredits"`
	} `json:"stats"`
}

type earningsResp struct {
	EarnedMicrocredits int64   `json:"earned_microcredits"`
	EarnedCredits      float64 `json:"earned_credits"`
	EstUSD             float64 `json:"est_usd"`
	EstUSDPerDay       float64 `json:"est_usd_per_day"`
	LifetimeTokens     int64   `json:"lifetime_tokens"`
	Note               string  `json:"note"`
}

type modelsResp struct {
	Models []struct {
		ID        string    `json:"id"`
		SizeBytes int64     `json:"size_bytes"`
		Pinned    bool      `json:"pinned"`
		LastUsed  time.Time `json:"last_used"`
		State     string    `json:"state"`
		Loaded    bool      `json:"loaded"`
		Default   bool      `json:"default"`
	} `json:"models"`
}

type limitsResp struct {
	ServePolicy    string   `json:"serve_policy"`
	IdleAfterSec   int      `json:"idle_after_seconds"`
	YieldGraceSec  int      `json:"yield_grace_seconds"`
	ServeOnBattery bool     `json:"serve_on_battery"`
	MaxTempCelsius float64  `json:"max_temp_celsius"`
	Schedule       []string `json:"schedule"`
}

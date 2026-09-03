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

// apiClient talks to the daemon's local API. Every tera command is a client
// of localhost:7777/api/v1 — one source of truth (SPEC §A1.2).
type apiClient struct {
	base        string
	token       string
	tokenSource string // where the token came from, for error messages
	dataDir     string
	hc          *http.Client
}

// newAPIClient resolves the bearer token, in precedence order: the --token
// flag, $TERA_TOKEN, then <dataDir>/local_api_token. The token is per
// install and lives beside the daemon's other state, so pointing at the
// wrong data dir is the usual cause of a 401 — tokenSource records where we
// looked so the error can say so.
func newAPIClient(base, dataDir string) (*apiClient, error) {
	c := &apiClient{
		base:    strings.TrimSuffix(base, "/"),
		dataDir: dataDir,
		hc:      &http.Client{Timeout: 10 * time.Second},
	}
	switch {
	case flagToken != "":
		c.token, c.tokenSource = strings.TrimSpace(flagToken), "--token"
	case os.Getenv("TERA_TOKEN") != "":
		c.token, c.tokenSource = strings.TrimSpace(os.Getenv("TERA_TOKEN")), "$TERA_TOKEN"
	default:
		path := filepath.Join(dataDir, "local_api_token")
		raw, err := os.ReadFile(path)
		if err != nil {
			c.tokenSource = fmt.Sprintf("no token found at %s", path)
			break
		}
		c.token, c.tokenSource = strings.TrimSpace(string(raw)), path
	}
	return c, nil
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
		return fmt.Errorf("cannot reach flockd at %s (is it running? try `tera up` or `flockd --standalone`): %w", c.base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("daemon rejected the auth token (%s).\n"+
			"  The token is per-install and lives in the daemon's data dir.\n"+
			"  If flockd runs with a custom data dir, point tera at the same one:\n"+
			"    tera --data-dir %s <command>   (or set FLOCKD_DATA_DIR / TERA_TOKEN)\n"+
			"  Print the current token with: tera token", c.tokenSource, c.dataDir)
	}
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
	Memory struct {
		UsedMB   int64 `json:"used_mb"`
		BudgetMB int64 `json:"budget_mb"`
		TotalMB  int64 `json:"total_mb"`
	} `json:"memory"`
	Disk struct {
		ModelsBytes  int64  `json:"models_bytes"`
		PartialBytes int64  `json:"partial_bytes"`
		BudgetBytes  int64  `json:"budget_bytes"`
		FreeBytes    int64  `json:"free_bytes"`
		Dir          string `json:"dir"`
	} `json:"disk"`
	Update *updateResp `json:"update"`
}

type updateResp struct {
	Available    bool   `json:"available"`
	Current      string `json:"current"`
	Latest       string `json:"latest"`
	Minimum      string `json:"minimum"`
	BelowMinimum bool   `json:"below_minimum"`
	URL          string `json:"url"`
}

// updateLine renders the one-line update notice for status/TUI ("" = none).
func updateLine(u *updateResp) string {
	if u == nil || !u.Available {
		return ""
	}
	line := "flockd " + u.Latest + " available"
	if u.BelowMinimum {
		line += " (REQUIRED: below the mesh minimum " + u.Minimum + "; the node is drained until updated)"
	}
	if u.URL != "" {
		line += " — " + u.URL
	}
	return line
}

// gb renders bytes as GB with one decimal.
func gb(b int64) string { return fmt.Sprintf("%.1fGB", float64(b)/(1<<30)) }

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
		ID        string     `json:"id"`
		SizeBytes int64      `json:"size_bytes"`
		Pinned    bool       `json:"pinned"`
		LastUsed  time.Time  `json:"last_used"`
		State     string     `json:"state"`
		Loaded    bool       `json:"loaded"`
		Default   bool       `json:"default"`
		Origin    string     `json:"origin"`
		LoadedMB  *int64     `json:"loaded_mb"`
		IdleSince *time.Time `json:"idle_since"`
	} `json:"models"`
}

type limitsResp struct {
	ServePolicy    string   `json:"serve_policy"`
	IdleAfterSec   int      `json:"idle_after_seconds"`
	YieldGraceSec  int      `json:"yield_grace_seconds"`
	ServeOnBattery bool     `json:"serve_on_battery"`
	MaxTempCelsius float64  `json:"max_temp_celsius"`
	Schedule       []string `json:"schedule"`
	MeshManaged    *bool    `json:"mesh_managed,omitempty"`
	MaxDiskMB      *int64   `json:"max_disk_mb,omitempty"`
	RetentionDays  *int     `json:"retention_days,omitempty"`
	MaxRAMMB       *int64   `json:"max_ram_mb,omitempty"`
	IdleUnloadSec  *int     `json:"idle_unload_seconds,omitempty"`
}

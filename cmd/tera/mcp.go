package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/gen"
)

// tera mcp is a local MCP server over stdio that exposes the operator's own
// node to any MCP client on the machine — Claude Code, Claude Desktop,
// Cursor (flockd#34). It is a thin wrapper over the loopback API that
// already exists: it talks to 127.0.0.1:7777 with the same token as every
// other tera command, is spawned by the client as the same user, and adds
// no listener of its own. Tool inputs and outputs are built from the
// generated types in internal/localapi/gen, so the surface is verified
// against the one API contract by construction (mcpPaths + the parity
// test keep the list honest).

// mcpPaths are the spec paths the tools and resources wrap. A test checks
// every one exists in api/openapi.yaml.
var mcpPaths = []string{
	"/v1/chat/completions",
	"/api/v1/status",
	"/api/v1/models",
	"/api/v1/models/{id}/load",
	"/api/v1/models/{id}/unload",
	"/api/v1/models/{id}/download",
	"/api/v1/catalog",
	"/api/v1/logs",
}

const (
	resCatalog = "teraflock://catalog"
	resStatus  = "teraflock://status"
	resLogs    = "teraflock://logs"
	// mcpLogLines is how much of the ring the logs resource returns.
	mcpLogLines = 100
)

func cmdMCP() *cobra.Command {
	var describe bool
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve this node to MCP clients over stdio (Claude Code, Claude Desktop, Cursor)",
		Long: `Run a Model Context Protocol server on stdin/stdout that exposes the
node on this machine: chat with its models, list and manage them, read its
status and logs. Point an MCP client at it and it spawns tera as you:

  claude mcp add teraflock -- tera mcp

Loopback only — it talks to the daemon at --api with the same token as
every other tera command and opens no port. Runs until stdin closes.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := newClient()
			if err != nil {
				return err
			}
			srv := newMCPServer(cl, version)
			if describe {
				return describeMCP(cmd.OutOrStdout(), srv)
			}
			// stdout is the wire: nothing else may print there. The client
			// ends the session by closing stdin; that is a clean exit, not
			// an error — even when it lands mid-request, which the SDK's
			// jsonrpc2 layer reports as "server is closing: EOF".
			err = srv.Run(cmd.Context(), &mcp.StdioTransport{})
			if err != nil && (errors.Is(err, io.EOF) || strings.Contains(err.Error(), "server is closing")) {
				return nil
			}
			return err
		},
	}
	c.Flags().BoolVar(&describe, "describe", false, "print the tools and resources this server exposes, then exit")
	return c
}

// ---- tool inputs / outputs ----

type mcpMessage struct {
	Role    string `json:"role" jsonschema:"system, user or assistant"`
	Content string `json:"content"`
}

type chatIn struct {
	Model       string       `json:"model,omitempty" jsonschema:"model id from list_models; empty uses the node's default model"`
	Prompt      string       `json:"prompt,omitempty" jsonschema:"a single user message (use this or messages)"`
	Messages    []mcpMessage `json:"messages,omitempty" jsonschema:"full conversation in OpenAI chat format (use this or prompt)"`
	System      string       `json:"system,omitempty" jsonschema:"system prompt, prepended to the conversation"`
	MaxTokens   int          `json:"max_tokens,omitempty" jsonschema:"cap on completion tokens (0 = daemon default, 256)"`
	Temperature *float64     `json:"temperature,omitempty" jsonschema:"sampling temperature (omit for the daemon default)"`
}

type chatOut struct {
	Model         string       `json:"model"`
	Content       string       `json:"content"`
	Reasoning     string       `json:"reasoning,omitempty" jsonschema:"chain-of-thought when the model produced any (reasoning_content)"`
	FinishReason  string       `json:"finish_reason"`
	Usage         client.Usage `json:"usage"`
	TokensPerSec  float64      `json:"tokens_per_sec"`
	ElapsedMillis int64        `json:"elapsed_ms"`
}

type modelIDIn struct {
	ID string `json:"id" jsonschema:"model id (catalog id or cache id), as shown by list_models"`
}

type modelOut struct {
	ID            string `json:"id"`
	State         string `json:"state" jsonschema:"assigned | downloading | ready | missing"`
	Loaded        bool   `json:"loaded"`
	Default       bool   `json:"default"`
	Pinned        bool   `json:"pinned"`
	Origin        string `json:"origin" jsonschema:"operator (you) or mesh (coordinator placement)"`
	SizeBytes     int64  `json:"size_bytes"`
	LoadedMB      *int64 `json:"loaded_mb,omitempty"`
	ReceivedBytes *int64 `json:"received_bytes,omitempty" jsonschema:"download progress; present only while downloading"`
}

type listModelsOut struct {
	Models       []modelOut `json:"models"`
	DefaultModel string     `json:"default_model,omitempty"`
}

type memoryOut struct {
	UsedMB   int64 `json:"used_mb"`
	BudgetMB int64 `json:"budget_mb"`
	TotalMB  int64 `json:"total_mb"`
}

type diskOut struct {
	ModelsBytes int64  `json:"models_bytes"`
	FreeBytes   int64  `json:"free_bytes"`
	BudgetBytes int64  `json:"budget_bytes" jsonschema:"0 = unlimited"`
	Dir         string `json:"dir"`
}

type updateOut struct {
	Available    bool   `json:"available"`
	Latest       string `json:"latest"`
	Minimum      string `json:"minimum,omitempty"`
	BelowMinimum bool   `json:"below_minimum"`
}

type statusOut struct {
	State          string     `json:"state" jsonschema:"serving | yielded | paused-battery | paused-thermal | outside-schedule"`
	Summary        string     `json:"summary" jsonschema:"one plain-words sentence about what the node is doing and why"`
	Version        string     `json:"version"`
	Standalone     bool       `json:"standalone"`
	Enrolled       bool       `json:"enrolled"`
	DefaultModel   string     `json:"default_model"`
	ModelsLoaded   int        `json:"models_loaded"`
	Inflight       int        `json:"inflight"`
	TokensPerSec1m float64    `json:"tokens_per_sec_1m"`
	OnBattery      bool       `json:"on_battery"`
	Memory         memoryOut  `json:"memory"`
	Disk           diskOut    `json:"disk"`
	Update         *updateOut `json:"update,omitempty"`
	Downloads      []modelOut `json:"downloads,omitempty" jsonschema:"models currently downloading, with received_bytes"`
}

type downloadOut struct {
	ID      string `json:"id"`
	State   string `json:"state" jsonschema:"downloading | ready"`
	Message string `json:"message"`
}

type okOut struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// ---- server ----

func newMCPServer(cl *client.Client, ver string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:       "teraflock",
		Title:      "Teraflock node",
		Version:    ver,
		WebsiteURL: "https://teraflock.com",
	}, &mcp.ServerOptions{
		Instructions: "This server is the Teraflock node running on this machine (flockd, loopback only). " +
			"Use list_models to see what is installed, chat to run inference on it, node_status for state. " +
			"The node may refuse inference while its operator is active (serve policy idle-only); " +
			"the error says how to change that. A cold model takes a few seconds on the first chat.",
	})
	t := &mcpTools{cl: cl}
	ro := true
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "chat",
		Description: "Run a chat completion on a model served by this node (local, private, no API key). Non-streaming; returns the text, usage and tok/s.",
		Annotations: &mcp.ToolAnnotations{Title: "Chat with the local model", ReadOnlyHint: true, OpenWorldHint: &[]bool{false}[0]},
	}, t.chat)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_models",
		Description: "List the models installed on this node with state (downloading/ready/missing), whether they are loaded, and which is the default.",
		Annotations: &mcp.ToolAnnotations{Title: "List installed models", ReadOnlyHint: true, OpenWorldHint: &[]bool{false}[0]},
	}, t.listModels)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "node_status",
		Description: "This node's state (serving or why not), loaded models, memory and disk budgets, enrollment, update status, and downloads in flight.",
		Annotations: &mcp.ToolAnnotations{Title: "Node status", ReadOnlyHint: true, OpenWorldHint: &[]bool{false}[0]},
	}, t.nodeStatus)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "load_model",
		Description: "Load an installed model into the serving runtime (synchronous; downloads first if it is not cached). Needs the llama.cpp runtime.",
		Annotations: &mcp.ToolAnnotations{Title: "Load a model", IdempotentHint: true, DestructiveHint: &[]bool{false}[0], OpenWorldHint: &[]bool{false}[0]},
	}, t.loadModel)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "unload_model",
		Description: "Unload a model from the runtime; the file stays cached on disk.",
		Annotations: &mcp.ToolAnnotations{Title: "Unload a model", IdempotentHint: true, DestructiveHint: &[]bool{false}[0], OpenWorldHint: &[]bool{false}[0]},
	}, t.unloadModel)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "download_model",
		Description: "Start downloading a catalog model in the background (see the teraflock://catalog resource for ids). Returns immediately; progress shows in node_status and list_models.",
		Annotations: &mcp.ToolAnnotations{Title: "Download a model", IdempotentHint: true, DestructiveHint: &[]bool{false}[0], OpenWorldHint: &ro},
	}, t.downloadModel)

	srv.AddResource(&mcp.Resource{
		URI:         resCatalog,
		Name:        "catalog",
		Title:       "Model catalog",
		Description: "The Teraflock model catalog merged with this node's local state (installed, loaded, default). JSON.",
		MIMEType:    "application/json",
	}, t.readCatalog)
	srv.AddResource(&mcp.Resource{
		URI:         resStatus,
		Name:        "status",
		Title:       "Node status (full)",
		Description: "The full /api/v1/status snapshot: hardware, stats, memory, disk, update. JSON.",
		MIMEType:    "application/json",
	}, t.readStatus)
	srv.AddResource(&mcp.Resource{
		URI:         resLogs,
		Name:        "logs",
		Title:       "Recent daemon logs",
		Description: fmt.Sprintf("The last %d lines of the daemon's log ring (request content is never logged). Text.", mcpLogLines),
		MIMEType:    "text/plain",
	}, t.readLogs)
	return srv
}

type mcpTools struct {
	cl *client.Client
}

// toolErr is the readable tool error: the daemon's message plus the fix
// (daemon not running -> tera up; governor refusal -> tera limits;
// unknown model -> download_model). Returned errors become IsError tool
// results the model can read and act on.
func toolErr(err error) error {
	if r := client.Remedy(err); r != "" {
		return fmt.Errorf("%v — %s", err, strings.ReplaceAll(r, "`tera models pull <id>`", "download_model (or `tera models pull <id>`)"))
	}
	return err
}

func (t *mcpTools) chat(ctx context.Context, _ *mcp.CallToolRequest, in chatIn) (*mcp.CallToolResult, chatOut, error) {
	var msgs []client.Message
	if in.System != "" {
		msgs = append(msgs, client.Message{Role: "system", Content: in.System})
	}
	for _, m := range in.Messages {
		msgs = append(msgs, client.Message{Role: m.Role, Content: m.Content})
	}
	if in.Prompt != "" {
		msgs = append(msgs, client.Message{Role: "user", Content: in.Prompt})
	}
	if len(msgs) == 0 || (len(msgs) == 1 && in.System != "") {
		return nil, chatOut{}, fmt.Errorf("nothing to send: set prompt or messages")
	}
	res, err := t.cl.Chat(ctx, client.ChatRequest{Model: in.Model, Messages: msgs, MaxTokens: in.MaxTokens, Temperature: in.Temperature})
	if err != nil {
		return nil, chatOut{}, toolErr(err)
	}
	out := chatOut{
		Model: res.Model, Content: res.Content, Reasoning: res.Reasoning, FinishReason: res.FinishReason,
		Usage: res.Usage, TokensPerSec: round1(res.TokensPerSec()), ElapsedMillis: res.Elapsed.Milliseconds(),
	}
	// The text the model reads is the answer itself; the numbers ride in
	// structuredContent.
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: res.Content}}}, out, nil
}

func (t *mcpTools) listModels(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listModelsOut, error) {
	ml, err := t.cl.Models(ctx)
	if err != nil {
		return nil, listModelsOut{}, toolErr(err)
	}
	out := listModelsOut{Models: make([]modelOut, 0, len(ml.Models))}
	for _, m := range ml.Models {
		out.Models = append(out.Models, modelToOut(m))
		if m.Default {
			out.DefaultModel = m.Id
		}
	}
	if out.DefaultModel == "" {
		if st, err := t.cl.Status(ctx); err == nil {
			out.DefaultModel = st.DefaultModel
		}
	}
	return nil, out, nil
}

func modelToOut(m gen.ModelRow) modelOut {
	return modelOut{
		ID: m.Id, State: m.State, Loaded: m.Loaded, Default: m.Default, Pinned: m.Pinned,
		Origin: m.Origin, SizeBytes: m.SizeBytes, LoadedMB: m.LoadedMb, ReceivedBytes: m.ReceivedBytes,
	}
}

func (t *mcpTools) nodeStatus(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, statusOut, error) {
	st, err := t.cl.Status(ctx)
	if err != nil {
		return nil, statusOut{}, toolErr(err)
	}
	out := statusOut{
		State: st.State, Summary: statusSummary(st), Version: st.Version, Standalone: st.Standalone,
		Enrolled: st.Enrolled, DefaultModel: st.DefaultModel, ModelsLoaded: st.ModelsLoaded,
		Inflight: st.Inflight, TokensPerSec1m: round1(st.Stats.TokensPerSec1m), OnBattery: st.OnBattery,
		Memory: memoryOut{UsedMB: st.Memory.UsedMb, BudgetMB: st.Memory.BudgetMb, TotalMB: st.Memory.TotalMb},
		Disk:   diskOut{ModelsBytes: st.Disk.ModelsBytes, FreeBytes: st.Disk.FreeBytes, BudgetBytes: st.Disk.BudgetBytes, Dir: st.Disk.Dir},
	}
	if u := st.Update; u != nil {
		out.Update = &updateOut{Available: u.Available, Latest: u.Latest}
		if u.Minimum != nil {
			out.Update.Minimum = *u.Minimum
		}
		if u.BelowMinimum != nil {
			out.Update.BelowMinimum = *u.BelowMinimum
		}
	}
	// Downloads in flight: the progress the download_model tool points at.
	if ml, err := t.cl.Models(ctx); err == nil {
		for _, m := range ml.Models {
			if m.State == "downloading" {
				out.Downloads = append(out.Downloads, modelToOut(m))
			}
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: out.Summary}}}, out, nil
}

// statusSummary is the plain-words sentence for the state (the desktop
// shows the same explanations).
func statusSummary(st gen.Status) string {
	switch st.State {
	case "serving":
		return fmt.Sprintf("Serving: %d model(s) loaded, %d request(s) in flight, %.1f tok/s over the last minute; default model %q.",
			st.ModelsLoaded, st.Inflight, st.Stats.TokensPerSec1m, st.DefaultModel)
	case "starting":
		return fmt.Sprintf("Starting: the default model is still being downloaded or loaded; %d model(s) on the way. Chat works once it is ready.", 1)
	case "idle":
		return "Idle: nothing loaded right now (models are on disk and load on the first request or placement)."
	case "no-model":
		return "No model: nothing loaded and nothing on disk yet. Download one with `tera models pull <id>` or let the mesh place one."
	case "yielded":
		return "Not serving: the operator is using the machine and the serve policy is idle-only, so inference is refused until the node goes idle. `tera limits --serve always` serves now."
	case "paused-battery":
		return "Not serving: on battery power and serve_on_battery is off (`tera limits --serve-on-battery`)."
	case "paused-thermal":
		return "Not serving: the machine is above its temperature limit; it resumes when it cools."
	case "outside-schedule":
		return "Not serving: outside the configured serving schedule (`tera limits --schedule`)."
	}
	return "State: " + st.State + "."
}

func (t *mcpTools) loadModel(ctx context.Context, _ *mcp.CallToolRequest, in modelIDIn) (*mcp.CallToolResult, okOut, error) {
	if err := t.cl.LoadModel(ctx, in.ID); err != nil {
		return nil, okOut{}, toolErr(err)
	}
	return nil, okOut{ID: in.ID, Message: "loaded " + in.ID + "; it is now available to chat"}, nil
}

func (t *mcpTools) unloadModel(ctx context.Context, _ *mcp.CallToolRequest, in modelIDIn) (*mcp.CallToolResult, okOut, error) {
	if err := t.cl.UnloadModel(ctx, in.ID); err != nil {
		return nil, okOut{}, toolErr(err)
	}
	return nil, okOut{ID: in.ID, Message: "unloaded " + in.ID + "; the file stays cached"}, nil
}

func (t *mcpTools) downloadModel(ctx context.Context, _ *mcp.CallToolRequest, in modelIDIn) (*mcp.CallToolResult, downloadOut, error) {
	ds, err := t.cl.StartDownload(ctx, in.ID)
	if err != nil {
		return nil, downloadOut{}, toolErr(err)
	}
	out := downloadOut{ID: in.ID, State: ds.State}
	if ds.State == "ready" {
		out.Message = in.ID + " is already downloaded and verified"
	} else {
		out.Message = "download started for " + in.ID + "; watch received_bytes in node_status or list_models, then load_model"
	}
	return nil, out, nil
}

// ---- resources ----

func (t *mcpTools) readCatalog(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	cat, err := t.cl.Catalog(ctx, false)
	if err != nil {
		return nil, toolErr(err)
	}
	return jsonResource(req.Params.URI, cat)
}

func (t *mcpTools) readStatus(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	st, err := t.cl.Status(ctx)
	if err != nil {
		return nil, toolErr(err)
	}
	return jsonResource(req.Params.URI, st)
}

func (t *mcpTools) readLogs(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	logs, err := t.cl.Logs(ctx, mcpLogLines)
	if err != nil {
		return nil, toolErr(err)
	}
	var b strings.Builder
	for _, e := range logs {
		b.WriteString(e.Time.Local().Format("15:04:05") + " " + strings.ToUpper(e.Level) + " " + e.Message)
		if e.Attrs != nil && *e.Attrs != "" {
			b.WriteString(" " + *e.Attrs)
		}
		b.WriteString("\n")
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: b.String()}}}, nil
}

func jsonResource(uri string, v any) (*mcp.ReadResourceResult, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(raw)}}}, nil
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }

// ---- --describe ----

// describeMCP prints the surface the way docs/mcp.md quotes it, by asking
// the server itself over an in-memory session so the list cannot drift
// from what clients see.
func describeMCP(w io.Writer, srv *mcp.Server) error {
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		return err
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "tera-describe", Version: version}, nil).Connect(ctx, ct, nil)
	if err != nil {
		return err
	}
	defer cs.Close()

	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		return err
	}
	sort.Slice(tools.Tools, func(i, j int) bool { return tools.Tools[i].Name < tools.Tools[j].Name })
	fmt.Fprintln(w, "Tools:")
	for _, t := range tools.Tools {
		fmt.Fprintf(w, "  %-16s %s\n", t.Name, t.Description)
	}
	res, err := cs.ListResources(ctx, nil)
	if err != nil {
		return err
	}
	sort.Slice(res.Resources, func(i, j int) bool { return res.Resources[i].URI < res.Resources[j].URI })
	fmt.Fprintln(w, "Resources:")
	for _, r := range res.Resources {
		fmt.Fprintf(w, "  %-22s %s\n", r.URI, r.Description)
	}
	return nil
}

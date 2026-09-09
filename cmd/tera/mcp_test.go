package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/teraflock/flockd/internal/localapi/client"
	"github.com/teraflock/flockd/internal/localapi/client/clienttest"
	"gopkg.in/yaml.v3"
)

// mcpSession connects an in-memory MCP client to the server over the
// given daemon client.
func mcpSession(t *testing.T, cl *client.Client) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv := newMCPServer(cl, "test")
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args any) (*mcp.CallToolResult, string) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	return res, text.String()
}

func structured(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("structured content %s: %v", raw, err)
	}
}

var wantTools = []string{"chat", "download_model", "list_models", "load_model", "node_status", "unload_model"}
var wantResources = []string{resCatalog, resLogs, resStatus}

func TestMCPListsSixToolsAndThreeResources(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	cs := mcpSession(t, chatClient(t, d))
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
		if tl.Description == "" || tl.InputSchema == nil {
			t.Errorf("tool %s lacks a description or schema", tl.Name)
		}
	}
	if got := sorted(names); strings.Join(got, ",") != strings.Join(wantTools, ",") {
		t.Fatalf("tools = %v, want %v", got, wantTools)
	}
	res, err := cs.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, r := range res.Resources {
		uris = append(uris, r.URI)
	}
	if got := sorted(uris); strings.Join(got, ",") != strings.Join(wantResources, ",") {
		t.Fatalf("resources = %v, want %v", got, wantResources)
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestMCPChatReturnsTextAndUsage(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ReasoningTokens: 2})
	cs := mcpSession(t, chatClient(t, d))
	res, text := callTool(t, cs, "chat", map[string]any{"prompt": "hello mesh", "max_tokens": 8, "system": "be brief"})
	if res.IsError || text == "" {
		t.Fatalf("chat: isError=%v text=%q", res.IsError, text)
	}
	var out chatOut
	structured(t, res, &out)
	if out.Content != text || out.Usage.CompletionTokens == 0 || out.TokensPerSec <= 0 || out.Model != clienttest.Model || out.Reasoning == "" {
		t.Fatalf("out = %+v", out)
	}

	// Full-conversation form works too; an empty call is a tool error.
	res, text = callTool(t, cs, "chat", map[string]any{"messages": []map[string]string{{"role": "user", "content": "again"}}, "max_tokens": 4})
	if res.IsError || text == "" {
		t.Fatalf("messages form: isError=%v text=%q", res.IsError, text)
	}
	res, text = callTool(t, cs, "chat", map[string]any{"system": "only a system prompt"})
	if !res.IsError || !strings.Contains(text, "nothing to send") {
		t.Fatalf("empty chat: isError=%v text=%q", res.IsError, text)
	}
}

func TestMCPChatGovernorRefusalIsReadableToolError(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{ServePolicy: "idle-only"})
	cs := mcpSession(t, chatClient(t, d))
	res, text := callTool(t, cs, "chat", map[string]any{"prompt": "hi"})
	if !res.IsError {
		t.Fatalf("expected a tool error, got %q", text)
	}
	if !strings.Contains(text, "503") || !strings.Contains(text, "tera limits --serve always") {
		t.Fatalf("error should carry the daemon's message and the remedy:\n%s", text)
	}
	// node_status explains the same thing in words.
	res, text = callTool(t, cs, "node_status", nil)
	var st statusOut
	structured(t, res, &st)
	if st.State != "yielded" || !strings.Contains(st.Summary, "idle-only") || !strings.Contains(text, "idle-only") {
		t.Fatalf("status = %+v / %q", st, text)
	}
}

func TestMCPNodeStatusAndListModels(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	cs := mcpSession(t, chatClient(t, d))
	res, text := callTool(t, cs, "node_status", nil)
	var st statusOut
	structured(t, res, &st)
	if res.IsError || st.State != "serving" || !st.Standalone || st.Version != "test" || !strings.HasPrefix(text, "Serving:") {
		t.Fatalf("status = %+v / %q", st, text)
	}
	res, _ = callTool(t, cs, "list_models", nil)
	var lm listModelsOut
	structured(t, res, &lm)
	// The mock runtime has no model store, so the list is empty but the
	// default model still comes from status.
	if res.IsError || lm.Models == nil || lm.DefaultModel != clienttest.Model {
		t.Fatalf("list_models = %+v", lm)
	}
}

func TestMCPModelOpsOnMockRuntimeExplain(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	cs := mcpSession(t, chatClient(t, d))
	for _, name := range []string{"load_model", "unload_model", "download_model"} {
		res, text := callTool(t, cs, name, map[string]any{"id": "m"})
		if !res.IsError || !strings.Contains(text, "501") || !strings.Contains(text, "llamacpp") {
			t.Errorf("%s: isError=%v text=%q", name, res.IsError, text)
		}
	}
	// Missing required argument is rejected before the handler runs.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "load_model", Arguments: map[string]any{}})
	if err == nil && !res.IsError {
		t.Fatal("load_model without id should fail")
	}
}

func TestMCPDaemonDownSaysTeraUp(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	cl := chatClient(t, d)
	d.Close()
	cs := mcpSession(t, cl)
	for _, name := range []string{"chat", "list_models", "node_status"} {
		args := map[string]any{}
		if name == "chat" {
			args["prompt"] = "hi"
		}
		res, text := callTool(t, cs, name, args)
		if !res.IsError || !strings.Contains(text, "cannot reach flockd") || !strings.Contains(text, "tera up") {
			t.Errorf("%s: isError=%v text=%q", name, res.IsError, text)
		}
	}
}

func TestMCPResources(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	cs := mcpSession(t, chatClient(t, d))
	d.Log.Info("mcp resource line", "k", "v")
	ctx := context.Background()

	st, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: resStatus})
	if err != nil || len(st.Contents) != 1 || st.Contents[0].MIMEType != "application/json" || !strings.Contains(st.Contents[0].Text, `"node_id": "node-test"`) {
		t.Fatalf("status resource: %v %+v", err, st)
	}
	logs, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: resLogs})
	if err != nil || len(logs.Contents) != 1 || !strings.Contains(logs.Contents[0].Text, "INFO mcp resource line k=v") {
		t.Fatalf("logs resource: %v %+v", err, logs)
	}
	// The catalog needs the llamacpp runtime: the mock answers 501 and the
	// read fails with that explanation rather than an empty document.
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: resCatalog}); err == nil || !strings.Contains(err.Error(), "llamacpp") {
		t.Fatalf("catalog on mock: err = %v", err)
	}
}

// Contract parity: every path the MCP surface wraps is in the spec.
func TestMCPPathsExistInOpenAPISpec(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	for _, p := range mcpPaths {
		if _, ok := spec.Paths[p]; !ok {
			t.Errorf("mcp wraps %s, which is not in api/openapi.yaml", p)
		}
	}
}

func TestMCPDescribeListsTheSurface(t *testing.T) {
	d := clienttest.New(t, clienttest.Options{})
	var out bytes.Buffer
	if err := describeMCP(&out, newMCPServer(chatClient(t, d), "test")); err != nil {
		t.Fatal(err)
	}
	for _, want := range append(append([]string{}, wantTools...), wantResources...) {
		if !strings.Contains(out.String(), want) {
			t.Errorf("--describe missing %s:\n%s", want, out.String())
		}
	}
}

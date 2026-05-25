package ai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withStubAPI redirects claudeAPIURL at a local httptest server for one test.
func withStubAPI(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	prev := claudeAPIURL
	claudeAPIURL = srv.URL
	t.Cleanup(func() { claudeAPIURL = prev })
}

// ---- Wire shape: ContentBlock must round-trip text, tool_use, tool_result ----

func TestContentBlock_JSONRoundtrip_Text(t *testing.T) {
	in := ContentBlock{Type: "text", Text: "hello"}
	b, _ := json.Marshal(in)
	var out ContentBlock
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.Text != in.Text {
		t.Errorf("roundtrip mismatch: %+v vs %+v", out, in)
	}
}

func TestContentBlock_JSONRoundtrip_ToolUse(t *testing.T) {
	in := ContentBlock{
		Type:  "tool_use",
		ID:    "toolu_abc",
		Name:  "list_pods",
		Input: json.RawMessage(`{"namespace":"default"}`),
	}
	b, _ := json.Marshal(in)
	if !strings.Contains(string(b), `"type":"tool_use"`) {
		t.Errorf("missing type in JSON: %s", b)
	}
	var out ContentBlock
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Name != in.Name {
		t.Errorf("tool_use roundtrip lost fields: %+v", out)
	}
	if string(out.Input) != string(in.Input) {
		t.Errorf("input roundtrip: got=%s want=%s", out.Input, in.Input)
	}
}

func TestContentBlock_JSONRoundtrip_ToolResult(t *testing.T) {
	in := ContentBlock{
		Type:      "tool_result",
		ToolUseID: "toolu_abc",
		Content:   "pod listing here",
		IsError:   false,
	}
	b, _ := json.Marshal(in)
	var out ContentBlock
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.ToolUseID != in.ToolUseID || out.Content != in.Content {
		t.Errorf("tool_result roundtrip: %+v", out)
	}
}

// Empty optional fields must not surface as null/empty-string in the body —
// omitempty preserves the wire contract (some Anthropic versions reject
// "input": null on tool_result blocks).
func TestContentBlock_OmitsEmpty(t *testing.T) {
	in := ContentBlock{Type: "text", Text: "x"}
	b, _ := json.Marshal(in)
	s := string(b)
	for _, banned := range []string{`"id"`, `"name"`, `"tool_use_id"`, `"input"`, `"content"`, `"is_error"`} {
		if strings.Contains(s, banned) {
			t.Errorf("text block leaked %s in JSON: %s", banned, s)
		}
	}
}

// ---- AgentTurn: happy path against an httptest stub ----

func TestAgentTurn_ParsesAssistantTextAndStopReason(t *testing.T) {
	withStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing x-api-key header")
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing anthropic-version header")
		}
		body, _ := io.ReadAll(r.Body)
		var req AgentRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("body not valid AgentRequest: %v\nbody=%s", err, body)
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`))
	})

	c := NewClaudeClient("test-key")
	resp, err := c.AgentTurn(context.Background(), AgentRequest{
		Messages: []AgentMessage{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "done" {
		t.Errorf("content = %+v", resp.Content)
	}
}

func TestAgentTurn_ParsesToolUseBlock(t *testing.T) {
	withStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{
		  "content":[
		    {"type":"text","text":"I'll check"},
		    {"type":"tool_use","id":"toolu_1","name":"list_pods","input":{"namespace":"default"}}
		  ],
		  "stop_reason":"tool_use"
		}`))
	})

	c := NewClaudeClient("k")
	resp, err := c.AgentTurn(context.Background(), AgentRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("want 2 blocks, got %d", len(resp.Content))
	}
	if resp.Content[1].Type != "tool_use" || resp.Content[1].Name != "list_pods" {
		t.Errorf("tool_use block: %+v", resp.Content[1])
	}
	// Input must be the raw JSON object, not a string.
	var args map[string]string
	if err := json.Unmarshal(resp.Content[1].Input, &args); err != nil {
		t.Errorf("input not valid JSON object: %v", err)
	}
	if args["namespace"] != "default" {
		t.Errorf("input.namespace = %q", args["namespace"])
	}
}

func TestAgentTurn_HTTPErrorSurfaces(t *testing.T) {
	withStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"boom"}}`))
	})
	c := NewClaudeClient("k")
	_, err := c.AgentTurn(context.Background(), AgentRequest{})
	if err == nil {
		t.Error("expected error on 500")
	}
}

func TestAgentTurn_APIErrorBlockSurfaces(t *testing.T) {
	withStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"error":{"type":"overloaded_error","message":"slow down"}}`))
	})
	c := NewClaudeClient("k")
	_, err := c.AgentTurn(context.Background(), AgentRequest{})
	if err == nil || !strings.Contains(err.Error(), "overloaded_error") {
		t.Errorf("expected overloaded_error in message, got: %v", err)
	}
}

func TestAgentTurn_DefaultsModelAndMaxTokens(t *testing.T) {
	var seen AgentRequest
	withStubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":[],"stop_reason":"end_turn"}`))
	})
	c := NewClaudeClient("k")
	if _, err := c.AgentTurn(context.Background(), AgentRequest{}); err != nil {
		t.Fatal(err)
	}
	if seen.Model == "" {
		t.Error("Model not defaulted")
	}
	if seen.MaxTokens == 0 {
		t.Error("MaxTokens not defaulted")
	}
}

// Public Model() accessor must report a non-empty id so telemetry doesn't
// log "" as the model. (claudeModel is a const but this guards future drift.)
func TestModelAccessor(t *testing.T) {
	if NewClaudeClient("k").Model() == "" {
		t.Error("Model() returned empty")
	}
}

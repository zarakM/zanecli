package agent

// Agent-layer unit tests. The end-to-end Step loop is exercised by the
// manual testdata/ scenarios in CI; here we cover the pure helpers and the
// Session's session-state contract (Clear / Messages / LoadMessages /
// MarkFeedback) so a refactor can't quietly break those guarantees.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/zarakM/zanecli/pkg/ai"
	"github.com/zarakM/zanecli/pkg/config"
	"github.com/zarakM/zanecli/pkg/k8s"
	"github.com/zarakM/zanecli/pkg/tools"
)

// ---- classifyStepKind ----

func TestClassifyStepKind(t *testing.T) {
	cases := []struct {
		name string
		seq  []string
		want string
	}{
		{"empty", nil, "chat"},
		{"chat-only", []string{"list_pods", "describe_pod"}, "chat"},
		{"diagnostic", []string{"diagnose_pod"}, "diagnostic"},
		{"write", []string{"restart_deployment"}, "write"},
		{"mixed", []string{"diagnose_pod", "delete_pod"}, "mixed"},
		{"mixed-with-reads", []string{"list_pods", "diagnose_pod", "delete_pod"}, "mixed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyStepKind(c.seq); got != c.want {
				t.Errorf("classifyStepKind(%v) = %q, want %q", c.seq, got, c.want)
			}
		})
	}
}

// ---- extractConfidenceClient (mirrors telemetry.extractConfidence) ----

func TestExtractConfidenceClient(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"## Confidence\nHigh — clear", "High"},
		{"## Confidence\nMedium — partial", "Medium"},
		{"## Confidence\nLow — guess", "Low"},
		{"## 📊 Confidence\nHigh — emoji header", "High"},
		{"no confidence line", ""},
		{"## Confidence", ""},
	}
	for _, c := range cases {
		if got := extractConfidenceClient(c.in); got != c.want {
			t.Errorf("extractConfidenceClient(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// ---- concatText / hasAnyText ----

func TestConcatTextAndHasAnyText(t *testing.T) {
	blocks := []ai.ContentBlock{
		{Type: "text", Text: "hello "},
		{Type: "tool_use", Name: "list_pods"},
		{Type: "text", Text: "world"},
	}
	if got := concatText(blocks); got != "hello world" {
		t.Errorf("concatText = %q", got)
	}
	if !hasAnyText(blocks) {
		t.Error("hasAnyText should be true")
	}
	if hasAnyText([]ai.ContentBlock{{Type: "tool_use"}, {Type: "text", Text: ""}}) {
		t.Error("hasAnyText should be false for only-empty-text + tool_use")
	}
}

// ---- humanReadableAction ----

func TestHumanReadableAction(t *testing.T) {
	r := humanReadableAction("restart_deployment",
		json.RawMessage(`{"namespace":"prod","deployment":"api"}`))
	if !strings.Contains(r, "restart deployment") || !strings.Contains(r, "prod/api") {
		t.Errorf("restart phrasing: %q", r)
	}
	r = humanReadableAction("delete_pod",
		json.RawMessage(`{"namespace":"x","pod":"y-z"}`))
	if !strings.Contains(r, "delete pod") || !strings.Contains(r, "x/y-z") {
		t.Errorf("delete phrasing: %q", r)
	}
	r = humanReadableAction("unknown_tool", nil)
	if !strings.Contains(r, "unknown_tool") {
		t.Errorf("fallback phrasing: %q", r)
	}
}

// ---- toolResult / toolResultErr ----

func TestToolResultHelpers(t *testing.T) {
	ok := toolResult("id1", "yay", nil)
	if ok.IsError || ok.Content != "yay" || ok.ToolUseID != "id1" || ok.Type != "tool_result" {
		t.Errorf("ok block: %+v", ok)
	}

	withErr := toolResult("id2", "", errStub("boom"))
	if !withErr.IsError || !strings.Contains(withErr.Content, "boom") {
		t.Errorf("err block: %+v", withErr)
	}

	plainErr := toolResultErr("id3", "refused")
	if !plainErr.IsError || plainErr.Content != "refused" {
		t.Errorf("plain err: %+v", plainErr)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

// ---- Session state: Clear / Messages / LoadMessages / MarkFeedback ----

// newTestSession builds a Session that won't reach the network: telemetry off,
// fake clientset, empty registry, nil confirmer. Safe for state-only tests.
func newTestSession(t *testing.T) *Session {
	t.Helper()
	cfg := &config.Config{
		AnthropicAPIKey:  "test-key",
		TelemetryEnabled: false,
		AutoExec:         false,
	}
	client := k8s.NewClientFromInterface(fake.NewSimpleClientset(), "https://test:6443")
	reg := tools.NewRegistry(client)
	return NewSession(cfg, client, reg, nil, "test-build")
}

func TestSession_MessagesRoundtrip(t *testing.T) {
	s := newTestSession(t)
	if len(s.Messages()) != 0 {
		t.Errorf("fresh session messages = %d, want 0", len(s.Messages()))
	}

	msgs := []ai.AgentMessage{
		{Role: "user", Content: []ai.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []ai.ContentBlock{{Type: "text", Text: "hello"}}},
	}
	s.LoadMessages(msgs)
	if len(s.Messages()) != 2 {
		t.Errorf("after LoadMessages: %d, want 2", len(s.Messages()))
	}
}

func TestSession_ClearWipesMessagesButPreservesSessionID(t *testing.T) {
	s := newTestSession(t)
	originalSessionID := s.sessionID

	s.LoadMessages([]ai.AgentMessage{
		{Role: "user", Content: []ai.ContentBlock{{Type: "text", Text: "x"}}},
	})
	s.turnCount = 5
	s.autoExecCount = 2

	s.Clear()

	if len(s.Messages()) != 0 {
		t.Errorf("messages not cleared: %d", len(s.Messages()))
	}
	if s.turnCount != 0 || s.autoExecCount != 0 {
		t.Errorf("counters not reset: turn=%d auto=%d", s.turnCount, s.autoExecCount)
	}
	if s.sessionID != originalSessionID {
		t.Errorf("sessionID changed: %q -> %q", originalSessionID, s.sessionID)
	}
}

func TestSession_MarkFeedback_NoPriorRowReturnsFalse(t *testing.T) {
	s := newTestSession(t)
	if s.MarkFeedback(+1) {
		t.Error("MarkFeedback should return false when lastRagEventID is 0")
	}
}

// ---- Step (smoke): one round-trip against a stub Anthropic returning end_turn ----

// Drives the full Step loop once. Uses claudeAPIURL override (a var in
// pkg/ai) — we set it via a helper that lives in the same module. The stub
// returns immediately with end_turn so no tool calls happen.
func TestStep_SmokeOneTurn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi back\n## Confidence\nLow — smoke"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()
	ai.SetAPIURLForTesting(srv.URL)
	defer ai.SetAPIURLForTesting("https://api.anthropic.com/v1/messages")

	s := newTestSession(t)
	if err := s.Step(context.Background(), "hello", io.Discard); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if len(s.Messages()) < 2 {
		t.Fatalf("Step should append user+assistant; got %d messages", len(s.Messages()))
	}
	if s.stepIndex != 1 {
		t.Errorf("stepIndex after one Step = %d, want 1", s.stepIndex)
	}
}

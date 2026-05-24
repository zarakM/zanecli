package tools

// Tool registry. Each tool is a small Go struct that implements Tool;
// the agent loop dispatches Anthropic tool_use blocks here by name.
//
// Phase 3 ships read-only tools. Phase 4 adds writes (restart_deployment,
// delete_pod) that the registry routes through pkg/safety guards.

import (
	"context"
	"encoding/json"
	"fmt"
	"zanecli/pkg/ai"
	"zanecli/pkg/k8s"
)

// Tool is the contract every tool implements.
// InputSchema is JSON-schema in map form (sent verbatim to Anthropic).
type Tool interface {
	Name() string
	Description() string
	InputSchema() map[string]any
	Run(ctx context.Context, input json.RawMessage) (string, error)
}

// DiagnosticSink receives the structured *k8s.*DiagnosticData captured by
// diagnose_pod and diagnose_rollout so the agent layer can fire telemetry
// at end-of-Step. The diagnose tools also return their rendered text to the
// LLM; the sink is a side-channel for the structured payload, not a
// replacement for the tool result. A nil sink is a no-op.
type DiagnosticSink interface {
	RecordCrash(*k8s.DiagnosticData)
	RecordPending(*k8s.PendingDiagnosticData)
	RecordRollout(*k8s.RolloutDiagnosticData)
}

// Registry holds all tools available to the agent.
type Registry struct {
	tools  []Tool
	byName map[string]Tool

	// sink is settable after construction because the Session (which
	// implements DiagnosticSink) is built after the Registry. Only the
	// two diagnose tools read it, via pointer-back to the registry.
	sink DiagnosticSink
}

// NewRegistry builds the full tool set: read tools + write tools.
// The agent loop applies pkg/safety guards before executing any write,
// so the tools themselves are unguarded — safety lives at the call site.
func NewRegistry(client *k8s.Client) *Registry {
	r := &Registry{byName: map[string]Tool{}}

	// Reads
	r.Add(&ListNamespacesTool{Client: client})
	r.Add(&ListPodsTool{Client: client})
	r.Add(&ListDeploymentsTool{Client: client})
	r.Add(&DescribePodTool{Client: client})
	r.Add(&DescribeDeploymentTool{Client: client})
	r.Add(&GetPodLogsTool{Client: client})
	r.Add(&GetEventsTool{Client: client})
	r.Add(&DiagnosePodTool{Client: client, Registry: r})
	r.Add(&DiagnoseRolloutTool{Client: client, Registry: r})
	r.Add(&ListPVCsTool{Client: client})
	r.Add(&ListStorageClassesTool{Client: client})
	r.Add(&GetResourceTool{Client: client})

	// Writes
	r.Add(&RestartDeploymentTool{Client: client})
	r.Add(&DeletePodTool{Client: client})

	return r
}

// SetDiagnosticSink wires the end-of-Step telemetry sink. Called from main
// after the Session is constructed (Session satisfies DiagnosticSink).
func (r *Registry) SetDiagnosticSink(sink DiagnosticSink) {
	r.sink = sink
}

// Sink returns the registered DiagnosticSink, or nil if none is set.
// Diagnose tools call this at run time and nil-check before recording.
func (r *Registry) Sink() DiagnosticSink {
	return r.sink
}

// Add registers a tool. Last write wins on name collision (intentional —
// lets Phase 4 swap the read-only stub for a real write impl if needed).
func (r *Registry) Add(t Tool) {
	if _, exists := r.byName[t.Name()]; !exists {
		r.tools = append(r.tools, t)
	}
	r.byName[t.Name()] = t
}

// Run dispatches a tool call. Errors are returned as strings so the agent
// can feed them back to Claude as tool_result content (with IsError=true
// set by the agent layer); we return Go errors only on protocol-level
// problems (unknown tool name).
func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, ok := r.byName[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return t.Run(ctx, input)
}

// AnthropicSchema returns the tools[] payload to send on each agent turn.
func (r *Registry) AnthropicSchema() []ai.ToolDef {
	out := make([]ai.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, ai.ToolDef{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	return out
}

// IsDiagnosticTool reports whether a tool name is one of the heavy
// diagnose_* bundles. Used by the agent's RAG telemetry to classify a
// Step's step_kind alongside safety.IsWriteTool.
func IsDiagnosticTool(name string) bool {
	switch name {
	case "diagnose_pod", "diagnose_rollout":
		return true
	}
	return false
}

// --- shared input-parsing helpers used by the individual tool files ---

// stringField returns a string field from the parsed input, or fallback.
func stringField(in map[string]any, key, fallback string) string {
	if v, ok := in[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// intField returns an int field from the parsed input, or fallback.
// JSON numbers decode as float64 in Go; we narrow to int.
func intField(in map[string]any, key string, fallback int) int {
	if v, ok := in[key].(float64); ok {
		return int(v)
	}
	return fallback
}

// boolField returns a bool field from the parsed input, or fallback.
func boolField(in map[string]any, key string, fallback bool) bool {
	if v, ok := in[key].(bool); ok {
		return v
	}
	return fallback
}

// parseInput unmarshals raw JSON into a generic map for stringField/intField/boolField.
// Returns an empty map on nil input (Anthropic sometimes omits "input" entirely).
func parseInput(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("could not parse tool input: %w", err)
	}
	return m, nil
}

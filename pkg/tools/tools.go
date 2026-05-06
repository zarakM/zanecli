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

// Registry holds all tools available to the agent.
type Registry struct {
	tools  []Tool
	byName map[string]Tool
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
	r.Add(&DiagnosePodTool{Client: client})
	r.Add(&DiagnoseRolloutTool{Client: client})

	// Writes
	r.Add(&RestartDeploymentTool{Client: client})
	r.Add(&DeletePodTool{Client: client})

	return r
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

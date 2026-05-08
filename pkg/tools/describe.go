package tools

// describe_pod and describe_deployment — formatted spec dumps for the agent.
// These reuse the existing k8s.Client gather strategies so secrets are
// already redacted (see formatPodSpec in pkg/k8s/client.go).

import (
	"context"
	"encoding/json"
	"fmt"

	"zanecli/pkg/k8s"
)

// --- describe_pod ---

type DescribePodTool struct {
	Client *k8s.Client
}

func (t *DescribePodTool) Name() string { return "describe_pod" }

func (t *DescribePodTool) Description() string {
	return "Show a pod's spec (image, resources, env vars with secrets redacted, probes) and runtime container statuses. Use this for context before running diagnose_pod."
}

func (t *DescribePodTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string"},
			"pod":       map[string]any{"type": "string"},
		},
		"required": []string{"namespace", "pod"},
	}
}

func (t *DescribePodTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	pod := stringField(in, "pod", "")
	if pod == "" {
		return "error: 'pod' is required", nil
	}

	// GatherDiagnostics returns full crash data including PodSpec + Containers.
	// We just keep the spec and runtime status sections — no logs, no events.
	data, err := t.Client.GatherDiagnostics(ctx, ns, pod, 0)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return data.PodSpec + "\n" + formatContainers(data.Containers), nil
}

// --- describe_deployment ---

type DescribeDeploymentTool struct {
	Client *k8s.Client
}

func (t *DescribeDeploymentTool) Name() string { return "describe_deployment" }

func (t *DescribeDeploymentTool) Description() string {
	return "Show a deployment's spec (strategy, replicas, selector, progressDeadline) and current .status (Progressing, Available conditions). The status conditions are the canonical 'is this rollout stuck' signal."
}

func (t *DescribeDeploymentTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace":  map[string]any{"type": "string"},
			"deployment": map[string]any{"type": "string"},
		},
		"required": []string{"namespace", "deployment"},
	}
}

func (t *DescribeDeploymentTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	dep := stringField(in, "deployment", "")
	if dep == "" {
		return "error: 'deployment' is required", nil
	}

	data, err := t.Client.GatherRolloutDiagnostics(ctx, ns, dep, 0)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return data.DeploymentSpec + "\n" + data.Status, nil
}

// formatContainers turns the structured ContainerStatus slice into a short
// agent-friendly text block (state, restarts, ready, last-crash details).
func formatContainers(cs []k8s.ContainerStatus) string {
	if len(cs) == 0 {
		return "(no container statuses yet)"
	}
	var out string
	out += "Container statuses:\n"
	for _, c := range cs {
		out += fmt.Sprintf("  %s: state=%s restarts=%d ready=%v",
			c.Name, c.State, c.RestartCount, c.Ready)
		if c.LastState != "" {
			out += fmt.Sprintf(" last=%s", c.LastState)
		}
		out += "\n"
	}
	return out
}

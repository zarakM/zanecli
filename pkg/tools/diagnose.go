package tools

// diagnose_pod and diagnose_rollout — heavy-weight diagnostic gathers.
// Both wrap the existing pkg/k8s gather strategies and return their full
// formatted output as a single string for the agent to reason over.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/zarakM/zane/pkg/k8s"
)

// --- diagnose_pod ---

type DiagnosePodTool struct {
	Client *k8s.Client
	// Registry is a back-pointer used at Run time to find the current
	// DiagnosticSink. The sink is set after registry construction, so a
	// pointer-back lets us see the latest value without re-wiring tools.
	Registry *Registry
}

func (t *DiagnosePodTool) Name() string { return "diagnose_pod" }

func (t *DiagnosePodTool) Description() string {
	return "Run a full diagnostic on a pod: pod spec, container statuses, events, and logs (current + previous). Auto-detects pending vs crashing — pending pods get scheduler/node/quota/PVC context instead of logs. Use this when you need to explain why a pod is broken."
}

func (t *DiagnosePodTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string"},
			"pod":       map[string]any{"type": "string"},
			"lines": map[string]any{
				"type":        "integer",
				"description": "Number of log lines to tail per container (default 50). Ignored for pending pods.",
			},
		},
		"required": []string{"namespace", "pod"},
	}
}

func (t *DiagnosePodTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	pod := stringField(in, "pod", "")
	if pod == "" {
		return "error: 'pod' is required", nil
	}
	lines := intField(in, "lines", 50)

	// Phase decides crash vs pending path — same routing diagnose.go used to do.
	phase, err := t.Client.GetPodPhase(ctx, ns, pod)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}

	if phase == "Pending" {
		data, err := t.Client.GatherPendingDiagnostics(ctx, ns, pod)
		if err != nil {
			return fmt.Sprintf("error: %v", err), nil
		}
		if t.Registry != nil {
			if sink := t.Registry.Sink(); sink != nil {
				sink.RecordPending(data)
			}
		}
		return renderPendingData(data), nil
	}

	data, err := t.Client.GatherDiagnostics(ctx, ns, pod, lines)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if t.Registry != nil {
		if sink := t.Registry.Sink(); sink != nil {
			sink.RecordCrash(data)
		}
	}
	return renderCrashData(data), nil
}

// --- diagnose_rollout ---

type DiagnoseRolloutTool struct {
	Client   *k8s.Client
	Registry *Registry
}

func (t *DiagnoseRolloutTool) Name() string { return "diagnose_rollout" }

func (t *DiagnoseRolloutTool) Description() string {
	return "Run a full diagnostic on a Deployment rollout: deployment spec/status/conditions, owned ReplicaSets, pod summary, the worst replica's spec + logs, combined events, and matching PodDisruptionBudgets. Use this when a rollout is stuck, slow, or partially unavailable."
}

func (t *DiagnoseRolloutTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace":  map[string]any{"type": "string"},
			"deployment": map[string]any{"type": "string"},
			"lines": map[string]any{
				"type":        "integer",
				"description": "Number of log lines to tail from the worst pod (default 50).",
			},
		},
		"required": []string{"namespace", "deployment"},
	}
}

func (t *DiagnoseRolloutTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	dep := stringField(in, "deployment", "")
	if dep == "" {
		return "error: 'deployment' is required", nil
	}
	lines := intField(in, "lines", 50)

	data, err := t.Client.GatherRolloutDiagnostics(ctx, ns, dep, lines)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if t.Registry != nil {
		if sink := t.Registry.Sink(); sink != nil {
			sink.RecordRollout(data)
		}
	}
	return renderRolloutData(data), nil
}

// --- render helpers ---
//
// These reassemble the formatted-string fields that pkg/k8s already builds
// into a single agent-readable text block. We deliberately exclude the
// structured side-fields (EventInfos, ReplicaCounts, etc.) — those are for
// telemetry, not for the agent's view.

func renderCrashData(data *k8s.DiagnosticData) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pod: %s/%s (crash diagnostic)\n\n", data.Namespace, data.PodName)

	sb.WriteString("## Pod Spec\n")
	sb.WriteString(data.PodSpec)
	sb.WriteString("\n")

	if len(data.Containers) > 0 {
		sb.WriteString("## Container Status\n")
		for _, c := range data.Containers {
			fmt.Fprintf(&sb, "  %s: %s | restarts=%d | ready=%v",
				c.Name, c.State, c.RestartCount, c.Ready)
			if c.LastState != "" {
				fmt.Fprintf(&sb, " | last=%s", c.LastState)
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Events\n")
	sb.WriteString(data.Events)
	sb.WriteString("\n")

	if data.Logs != "" {
		fmt.Fprintf(&sb, "## Logs (%d lines)\n", data.LogLineCount)
		sb.WriteString(data.Logs)
	} else {
		sb.WriteString("## Logs\n(no logs available — pod may not have started)\n")
	}
	return sb.String()
}

func renderPendingData(data *k8s.PendingDiagnosticData) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pod: %s/%s (pending diagnostic)\n\n", data.Namespace, data.PodName)

	sb.WriteString("## Pod Spec\n")
	sb.WriteString(data.PodSpec)
	sb.WriteString("\n## Events\n")
	sb.WriteString(data.Events)
	sb.WriteString("\n## Nodes\n")
	sb.WriteString(data.NodeSummary)
	sb.WriteString("\n## ResourceQuotas\n")
	sb.WriteString(data.QuotaSummary)
	sb.WriteString("\n## PVCs\n")
	sb.WriteString(data.PVCSummary)
	return sb.String()
}

func renderRolloutData(data *k8s.RolloutDiagnosticData) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Deployment: %s/%s (rollout diagnostic)\n\n", data.Namespace, data.DeploymentName)

	sb.WriteString("## Spec\n")
	sb.WriteString(data.DeploymentSpec)
	sb.WriteString("\n## Status & Conditions\n")
	sb.WriteString(data.Status)
	sb.WriteString("\n## ReplicaSets\n")
	sb.WriteString(data.ReplicaSets)
	sb.WriteString("\n## Pods\n")
	sb.WriteString(data.PodSummary)

	if data.WorstPodName != "" {
		fmt.Fprintf(&sb, "\n## Worst Pod: %s\n", data.WorstPodName)
		sb.WriteString(data.WorstPodSpec)
		if data.WorstPodLogs != "" {
			sb.WriteString("\n### Worst Pod Logs\n")
			sb.WriteString(data.WorstPodLogs)
		}
	}

	sb.WriteString("\n## Events\n")
	sb.WriteString(data.Events)
	sb.WriteString("\n## Matching PDBs\n")
	sb.WriteString(data.PDBs)
	return sb.String()
}

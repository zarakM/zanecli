package tools

// get_pod_logs and get_events — narrow read tools the agent uses to peek
// at fresh logs / events without firing a full diagnose chain.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"zanecli/pkg/k8s"
)

// --- get_pod_logs ---

type GetPodLogsTool struct {
	Client *k8s.Client
}

func (t *GetPodLogsTool) Name() string { return "get_pod_logs" }

func (t *GetPodLogsTool) Description() string {
	return "Fetch recent log lines from a pod's container. Optionally pull logs from the previous (crashed) container instance — useful for CrashLoopBackOff. Defaults: container=first, lines=50, previous=false."
}

func (t *GetPodLogsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string"},
			"pod":       map[string]any{"type": "string"},
			"container": map[string]any{
				"type":        "string",
				"description": "Container name; if omitted, the first container in the pod is used.",
			},
			"lines": map[string]any{
				"type":        "integer",
				"description": "Number of trailing log lines (default 50).",
			},
			"previous": map[string]any{
				"type":        "boolean",
				"description": "Fetch logs from the previous (crashed) container instance instead of the current one.",
			},
		},
		"required": []string{"namespace", "pod"},
	}
}

func (t *GetPodLogsTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	pod := stringField(in, "pod", "")
	if pod == "" {
		return "error: 'pod' is required", nil
	}
	container := stringField(in, "container", "")
	lines := int64(intField(in, "lines", 50))
	previous := boolField(in, "previous", false)

	// Resolve default container name (first in spec) if the agent didn't pass one.
	if container == "" {
		p, err := t.Client.Clientset().CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
		if err != nil {
			return fmt.Sprintf("error fetching pod: %v", err), nil
		}
		if len(p.Spec.Containers) == 0 {
			return "(pod has no containers)", nil
		}
		container = p.Spec.Containers[0].Name
	}

	opts := &corev1.PodLogOptions{
		Container: container,
		TailLines: &lines,
		Previous:  previous,
	}
	logBytes, err := t.Client.Clientset().CoreV1().Pods(ns).GetLogs(pod, opts).DoRaw(ctx)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(logBytes) == 0 {
		hint := ""
		if previous {
			hint = " (no previous instance — container hasn't restarted yet)"
		}
		return "(no log output" + hint + ")", nil
	}
	return fmt.Sprintf("=== %s/%s container=%s previous=%v ===\n%s",
		ns, pod, container, previous, string(logBytes)), nil
}

// --- get_events ---

type GetEventsTool struct {
	Client *k8s.Client
}

func (t *GetEventsTool) Name() string { return "get_events" }

func (t *GetEventsTool) Description() string {
	return "List recent Kubernetes events in a namespace, optionally filtered to a specific resource by name. Events often carry the most direct explanation of why something failed (e.g. 'FailedScheduling: insufficient memory')."
}

func (t *GetEventsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string"},
			"name": map[string]any{
				"type":        "string",
				"description": "Optional resource name to filter events to. If omitted, returns all events in the namespace.",
			},
		},
		"required": []string{"namespace"},
	}
}

func (t *GetEventsTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	name := stringField(in, "name", "")

	listOpts := metav1.ListOptions{}
	if name != "" {
		listOpts.FieldSelector = fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=%s", name, ns)
	}

	events, err := t.Client.Clientset().CoreV1().Events(ns).List(ctx, listOpts)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(events.Items) == 0 {
		return "(no events)", nil
	}

	var sb strings.Builder
	for _, e := range events.Items {
		fmt.Fprintf(&sb, "[%s] %s %s/%s: %s\n",
			e.Type, e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
	}
	return sb.String(), nil
}

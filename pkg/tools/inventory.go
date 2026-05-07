package tools

// Inventory tools: list_namespaces, list_pods, list_deployments.
// All three are cheap discovery calls the agent uses to anchor a question
// to a concrete resource before running heavier diagnostics.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"zanecli/pkg/k8s"
)

// --- list_namespaces ---

type ListNamespacesTool struct {
	Client *k8s.Client
}

func (t *ListNamespacesTool) Name() string { return "list_namespaces" }

func (t *ListNamespacesTool) Description() string {
	return "List all namespaces in the cluster. Use this when the user asks about cluster-wide state without specifying a namespace."
}

func (t *ListNamespacesTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ListNamespacesTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	nsList, err := t.Client.Clientset().CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(nsList.Items) == 0 {
		return "(no namespaces visible)", nil
	}
	var sb strings.Builder
	for _, ns := range nsList.Items {
		sb.WriteString(ns.Name)
		sb.WriteByte('\n')
	}
	return sb.String(), nil
}

// --- list_pods ---

type ListPodsTool struct {
	Client *k8s.Client
}

func (t *ListPodsTool) Name() string { return "list_pods" }

func (t *ListPodsTool) Description() string {
	return "List pods in a namespace, with phase and ready/restart counts. Use this to find which pods are unhealthy before drilling into a specific one."
}

func (t *ListPodsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{
				"type":        "string",
				"description": "Kubernetes namespace. Defaults to 'default' if omitted.",
			},
		},
	}
}

func (t *ListPodsTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")

	pods, err := t.Client.Clientset().CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(pods.Items) == 0 {
		return fmt.Sprintf("(no pods in namespace %q)", ns), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Pods in namespace %q (%d total):\n", ns, len(pods.Items))
	for _, p := range pods.Items {
		ready, total := 0, len(p.Spec.Containers)
		var restarts int32
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
		}
		age := shortDuration(time.Since(p.CreationTimestamp.Time))
		fmt.Fprintf(&sb, "  %-50s phase=%s ready=%d/%d restarts=%d age=%s\n",
			p.Name, p.Status.Phase, ready, total, restarts, age)
	}
	return sb.String(), nil
}

// --- list_deployments ---

type ListDeploymentsTool struct {
	Client *k8s.Client
}

func (t *ListDeploymentsTool) Name() string { return "list_deployments" }

func (t *ListDeploymentsTool) Description() string {
	return "List deployments in a namespace with desired/ready replica counts. Use this to find which deployments are stuck or under-replicated."
}

func (t *ListDeploymentsTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{
				"type":        "string",
				"description": "Kubernetes namespace. Defaults to 'default' if omitted.",
			},
		},
	}
}

func (t *ListDeploymentsTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")

	deps, err := t.Client.Clientset().AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(deps.Items) == 0 {
		return fmt.Sprintf("(no deployments in namespace %q)", ns), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "Deployments in namespace %q (%d total):\n", ns, len(deps.Items))
	for _, d := range deps.Items {
		desired := int32(0)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		fmt.Fprintf(&sb, "  %-50s desired=%d ready=%d available=%d unavailable=%d\n",
			d.Name, desired, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UnavailableReplicas)
	}
	return sb.String(), nil
}

// shortDuration renders a duration in the same compact style as `kubectl get`
// (e.g. "45s", "12m", "3h", "5d"). Used so the agent can apply age-based
// health heuristics from list_pods output without parsing RFC3339 timestamps.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
}

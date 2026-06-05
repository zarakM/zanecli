package tools

// Write tools — restart_deployment, delete_pod. These produce side effects
// on the cluster. The agent loop must consult pkg/safety before invoking
// them; the tools themselves do NOT enforce safety (single responsibility).

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/zarakM/zane/pkg/k8s"
)

// --- restart_deployment ---

type RestartDeploymentTool struct {
	Client *k8s.Client
}

func (t *RestartDeploymentTool) Name() string { return "restart_deployment" }

func (t *RestartDeploymentTool) Description() string {
	return "Trigger a rolling restart of a Deployment by patching the pod-template's annotations. Equivalent to `kubectl rollout restart`. Safe for stateless workloads; use this to clear stuck rollouts caused by transient state. Eligible for auto-exec when the deployment is visibly stuck (Progressing=False or ready<desired) and the namespace is not production."
}

func (t *RestartDeploymentTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace":  map[string]any{"type": "string"},
			"deployment": map[string]any{"type": "string"},
		},
		"required": []string{"namespace", "deployment"},
	}
}

func (t *RestartDeploymentTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	dep := stringField(in, "deployment", "")
	if dep == "" {
		return "error: 'deployment' is required", nil
	}

	// Patching the pod-template annotations is how `kubectl rollout restart`
	// works under the hood — it forces a new ReplicaSet without changing image.
	timestamp := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"zane.dev/restartedAt":"%s"}}}}}`, timestamp)

	_, err = t.Client.Clientset().AppsV1().Deployments(ns).Patch(
		ctx, dep, types.StrategicMergePatchType, []byte(patch), metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("Triggered restart of deployment %s/%s at %s. New pods will roll in over the next ~30s.", ns, dep, timestamp), nil
}

// --- delete_pod ---

type DeletePodTool struct {
	Client *k8s.Client
}

func (t *DeletePodTool) Name() string { return "delete_pod" }

func (t *DeletePodTool) Description() string {
	return "Delete a controller-managed pod. The owning ReplicaSet/Deployment/StatefulSet will recreate it. Use this to clear a pod stuck in CrashLoopBackOff or ImagePullBackOff after the underlying issue is fixed (or as a last-resort kick for a wedged container). Eligible for auto-exec when the pod is in CrashLoopBackOff/ImagePullBackOff, has an owner, and the namespace is not production."
}

func (t *DeletePodTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"namespace": map[string]any{"type": "string"},
			"pod":       map[string]any{"type": "string"},
		},
		"required": []string{"namespace", "pod"},
	}
}

func (t *DeletePodTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")
	pod := stringField(in, "pod", "")
	if pod == "" {
		return "error: 'pod' is required", nil
	}

	if err := t.Client.Clientset().CoreV1().Pods(ns).Delete(ctx, pod, metav1.DeleteOptions{}); err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	return fmt.Sprintf("Deleted pod %s/%s. The owning controller will recreate it.", ns, pod), nil
}

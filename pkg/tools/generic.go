package tools

// get_resource: the catch-all read tool for kinds without a dedicated tool
// (StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, PersistentVolume,
// Service, Endpoints, Ingress, ConfigMap, Secret, HPA, Node, CRDs).
//
// It returns sanitized object YAML (a `kubectl get -o yaml` equivalent) — read
// only; Secret values are redacted in pkg/k8s before they reach this layer.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zarakM/zane/pkg/k8s"
)

type GetResourceTool struct {
	Client *k8s.Client
}

func (t *GetResourceTool) Name() string { return "get_resource" }

func (t *GetResourceTool) Description() string {
	return "Fetch any Kubernetes resource as YAML when no dedicated tool exists for its kind " +
		"(e.g. StatefulSet, DaemonSet, ReplicaSet, Job, CronJob, PersistentVolume, Service, " +
		"Endpoints, Ingress, ConfigMap, Secret, HPA, Node). Provide 'kind' (singular, plural, " +
		"or shortname like 'sts'/'ds'/'pv'), an optional 'name' (omit to list the kind), and " +
		"'namespace' for namespaced kinds (defaults to 'default'; ignored for cluster-scoped " +
		"kinds like Node and PersistentVolume). Secret values are redacted. Read-only. " +
		"Prefer the typed tools (diagnose_pod, diagnose_rollout, list_pvcs, ...) for kinds they cover."
}

func (t *GetResourceTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"description": "Resource kind: singular, plural, or shortname (e.g. 'StatefulSet', 'statefulsets', 'sts').",
			},
			"name": map[string]any{
				"type":        "string",
				"description": "Resource name. Omit to list all of this kind in the namespace.",
			},
			"namespace": map[string]any{
				"type":        "string",
				"description": "Namespace for namespaced kinds. Defaults to 'default'. Ignored for cluster-scoped kinds.",
			},
		},
		"required": []any{"kind"},
	}
}

func (t *GetResourceTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	kind := stringField(in, "kind", "")
	if kind == "" {
		return "error: 'kind' is required", nil
	}
	name := stringField(in, "name", "")
	ns := stringField(in, "namespace", "default")

	out, err := t.Client.GetResource(ctx, kind, ns, name)
	if err != nil {
		// Surface as tool-result text (not a Go error) so the agent can adjust
		// the kind/name and retry rather than aborting the turn.
		return fmt.Sprintf("error: %v", err), nil
	}
	return out, nil
}

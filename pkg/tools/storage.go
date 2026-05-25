package tools

// Storage tools: list_pvcs, list_storageclasses.
// Used when investigating Pending pods with storage problems — the agent
// enumerates the namespace's existing claims and the cluster's StorageClasses
// so it can ask the user which one to use rather than guessing a SC name.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/zarakM/zanecli/pkg/k8s"
)

// --- list_pvcs ---

type ListPVCsTool struct {
	Client *k8s.Client
}

func (t *ListPVCsTool) Name() string { return "list_pvcs" }

func (t *ListPVCsTool) Description() string {
	return "List PersistentVolumeClaims in a namespace with phase, StorageClass, size, and access modes. Use when investigating a Pending pod with PVC binding problems, or when picking a reference PVC the user can model a new claim after."
}

func (t *ListPVCsTool) InputSchema() map[string]any {
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

func (t *ListPVCsTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	in, err := parseInput(raw)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	ns := stringField(in, "namespace", "default")

	pvcs, err := t.Client.Clientset().CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(pvcs.Items) == 0 {
		return fmt.Sprintf("(no PVCs in namespace %q)", ns), nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "PVCs in namespace %q (%d total):\n", ns, len(pvcs.Items))
	const max = 30
	shown := 0
	for _, p := range pvcs.Items {
		if shown >= max {
			fmt.Fprintf(&sb, "  (%d more)\n", len(pvcs.Items)-shown)
			break
		}
		sc := "(none)"
		if p.Spec.StorageClassName != nil && *p.Spec.StorageClassName != "" {
			sc = *p.Spec.StorageClassName
		}
		size := "(unset)"
		if q, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			size = q.String()
		}
		modes := accessModesString(p.Spec.AccessModes)
		age := shortDuration(time.Since(p.CreationTimestamp.Time))
		fmt.Fprintf(&sb, "  %-40s phase=%s sc=%s size=%s modes=%s age=%s\n",
			p.Name, p.Status.Phase, sc, size, modes, age)
		shown++
	}
	return sb.String(), nil
}

func accessModesString(modes []corev1.PersistentVolumeAccessMode) string {
	if len(modes) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, string(m))
	}
	return strings.Join(out, ",")
}

// --- list_storageclasses ---

type ListStorageClassesTool struct {
	Client *k8s.Client
}

func (t *ListStorageClassesTool) Name() string { return "list_storageclasses" }

func (t *ListStorageClassesTool) Description() string {
	return "List cluster StorageClasses with provisioner, default flag, reclaim policy, and volume binding mode. Use to show the user which StorageClasses are available before drafting a PVC."
}

func (t *ListStorageClassesTool) InputSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t *ListStorageClassesTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	scList, err := t.Client.Clientset().StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	if len(scList.Items) == 0 {
		return "(no StorageClasses in cluster)", nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "StorageClasses (%d total):\n", len(scList.Items))
	for _, sc := range scList.Items {
		def := ""
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			def = " (default)"
		}
		reclaim := "(unset)"
		if sc.ReclaimPolicy != nil {
			reclaim = string(*sc.ReclaimPolicy)
		}
		bind := "(unset)"
		if sc.VolumeBindingMode != nil {
			bind = string(*sc.VolumeBindingMode)
		}
		fmt.Fprintf(&sb, "  %-30s provisioner=%s reclaim=%s binding=%s%s\n",
			sc.Name, sc.Provisioner, reclaim, bind, def)
	}
	return sb.String(), nil
}

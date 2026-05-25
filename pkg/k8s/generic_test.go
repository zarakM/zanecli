package k8s

// Secret redaction invariant for get_resource. The agent's get_resource tool
// returns sanitized YAML; any leak of Secret data or stringData would put
// raw credentials into Anthropic context. CLAUDE.md calls this out: "do not
// let get_resource issue anything but Get/List, and keep its Secret-value
// redaction."

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSanitize_SecretDataIsRedacted(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "db", "namespace": "default"},
		"data": map[string]any{
			"password": "c3VwZXJzZWNyZXQ=", // base64 "supersecret"
			"token":    "YWJjZGVm",
		},
	}}
	sanitize(obj)

	data, ok, _ := unstructured.NestedMap(obj.Object, "data")
	if !ok {
		t.Fatal("data map disappeared")
	}
	for k, v := range data {
		if v != "**REDACTED**" {
			t.Errorf("data[%s] = %v, want **REDACTED**", k, v)
		}
	}
	y, err := marshalYAML(obj)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(y, "c3VwZXJzZWNyZXQ") || strings.Contains(y, "YWJjZGVm") {
		t.Errorf("raw secret value leaked into YAML:\n%s", y)
	}
}

func TestSanitize_SecretStringDataIsRedacted(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "db"},
		"stringData": map[string]any{
			"password": "plaintext-password",
		},
	}}
	sanitize(obj)
	sd, _, _ := unstructured.NestedMap(obj.Object, "stringData")
	if sd["password"] != "**REDACTED**" {
		t.Errorf("stringData not redacted: %v", sd["password"])
	}
}

// ConfigMap data is NOT redacted — that's intentional, only Secrets carry
// the redaction obligation. This test pins the behavior so a future
// "redact all maps" change can't quietly happen.
func TestSanitize_ConfigMapDataNotRedacted(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "app-config"},
		"data": map[string]any{
			"LOG_LEVEL": "debug",
		},
	}}
	sanitize(obj)
	data, _, _ := unstructured.NestedMap(obj.Object, "data")
	if data["LOG_LEVEL"] != "debug" {
		t.Errorf("ConfigMap data mutated: %v", data["LOG_LEVEL"])
	}
}

func TestSanitize_StripsManagedFieldsAndLastApplied(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":          "api-x",
			"managedFields": []any{map[string]any{"manager": "kubectl"}},
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{...big blob...}",
				"other-annotation": "kept",
			},
		},
	}}
	sanitize(obj)

	if _, ok, _ := unstructured.NestedSlice(obj.Object, "metadata", "managedFields"); ok {
		t.Error("managedFields not stripped")
	}
	ann, _, _ := unstructured.NestedMap(obj.Object, "metadata", "annotations")
	if _, exists := ann["kubectl.kubernetes.io/last-applied-configuration"]; exists {
		t.Error("last-applied-configuration not stripped")
	}
	if ann["other-annotation"] != "kept" {
		t.Errorf("unrelated annotation was clobbered: %v", ann)
	}
}

func TestSanitize_NonSecretNonPodNoOp(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]any{"name": "api"},
		"spec":       map[string]any{"clusterIP": "10.0.0.1"},
	}}
	sanitize(obj)
	if ip, _, _ := unstructured.NestedString(obj.Object, "spec", "clusterIP"); ip != "10.0.0.1" {
		t.Errorf("Service spec mutated: clusterIP=%q", ip)
	}
}

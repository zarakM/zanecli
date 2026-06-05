package tools

// Tool-layer tests. The reads exercise the inventory tools against a fake
// clientset; the writes verify Run does the expected mutation. Schema /
// helper tests cover the small parsing utilities. We do not test the
// diagnose_* tools here — they're heavy and exercised by integration runs.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zarakM/zane/pkg/k8s"
)

func fakeK8s(objs ...runtime.Object) *k8s.Client {
	return k8s.NewClientFromInterface(fake.NewSimpleClientset(objs...), "https://test:6443")
}

// ---- helpers (parseInput / stringField / intField / boolField) ----

func TestParseInput_EmptyAndMalformed(t *testing.T) {
	m, err := parseInput(nil)
	if err != nil || len(m) != 0 {
		t.Errorf("nil input: m=%v err=%v", m, err)
	}
	m, err = parseInput(json.RawMessage(""))
	if err != nil || len(m) != 0 {
		t.Errorf("empty input: m=%v err=%v", m, err)
	}
	if _, err := parseInput(json.RawMessage("not json")); err == nil {
		t.Error("malformed input: want error")
	}
}

func TestFieldHelpers(t *testing.T) {
	in := map[string]any{
		"s":     "hello",
		"empty": "",
		"n":     float64(7),
		"b":     true,
	}
	if got := stringField(in, "s", "fb"); got != "hello" {
		t.Errorf("stringField present: %q", got)
	}
	if got := stringField(in, "empty", "fb"); got != "fb" {
		t.Errorf("stringField empty should fall back: %q", got)
	}
	if got := stringField(in, "missing", "fb"); got != "fb" {
		t.Errorf("stringField missing: %q", got)
	}
	if got := intField(in, "n", 0); got != 7 {
		t.Errorf("intField: %d", got)
	}
	if got := intField(in, "missing", 42); got != 42 {
		t.Errorf("intField missing: %d", got)
	}
	if got := boolField(in, "b", false); got != true {
		t.Errorf("boolField: %v", got)
	}
	if got := boolField(in, "missing", true); got != true {
		t.Errorf("boolField missing: %v", got)
	}
}

// ---- Registry dispatch ----

type stubTool struct {
	name string
	out  string
}

func (s *stubTool) Name() string                { return s.name }
func (s *stubTool) Description() string         { return "stub" }
func (s *stubTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (s *stubTool) Run(_ context.Context, _ json.RawMessage) (string, error) {
	return s.out, nil
}

func TestRegistry_RunDispatchAndUnknown(t *testing.T) {
	r := &Registry{byName: map[string]Tool{}}
	r.Add(&stubTool{name: "a", out: "result-a"})

	got, err := r.Run(context.Background(), "a", nil)
	if err != nil || got != "result-a" {
		t.Errorf("dispatch: got=%q err=%v", got, err)
	}

	if _, err := r.Run(context.Background(), "missing", nil); err == nil {
		t.Error("unknown tool: want error")
	}
}

func TestRegistry_AnthropicSchema(t *testing.T) {
	r := &Registry{byName: map[string]Tool{}}
	r.Add(&stubTool{name: "a"})
	r.Add(&stubTool{name: "b"})
	schema := r.AnthropicSchema()
	if len(schema) != 2 {
		t.Errorf("schema len = %d, want 2", len(schema))
	}
}

func TestIsDiagnosticTool(t *testing.T) {
	if !IsDiagnosticTool("diagnose_pod") || !IsDiagnosticTool("diagnose_rollout") {
		t.Error("diagnose tools should classify as diagnostic")
	}
	if IsDiagnosticTool("list_pods") {
		t.Error("list_pods should not be diagnostic")
	}
}

// ---- list_namespaces ----

func TestListNamespacesTool(t *testing.T) {
	client := fakeK8s(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}},
	)
	tool := &ListNamespacesTool{Client: client}
	out, err := tool.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "default") || !strings.Contains(out, "kube-system") {
		t.Errorf("missing namespace in output: %q", out)
	}
}

func TestListNamespacesTool_Empty(t *testing.T) {
	client := fakeK8s()
	tool := &ListNamespacesTool{Client: client}
	out, _ := tool.Run(context.Background(), nil)
	if !strings.Contains(strings.ToLower(out), "no namespaces") {
		t.Errorf("empty cluster output: %q", out)
	}
}

// ---- list_pods ----

func TestListPodsTool(t *testing.T) {
	client := fakeK8s(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "default"},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}},
			},
		},
	)
	tool := &ListPodsTool{Client: client}
	out, _ := tool.Run(context.Background(), json.RawMessage(`{"namespace":"default"}`))
	if !strings.Contains(out, "api-1") {
		t.Errorf("pod name missing: %q", out)
	}
}

// ---- restart_deployment patches the pod-template annotation ----

func TestRestartDeploymentTool_PatchesAnnotation(t *testing.T) {
	replicas := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}},
			},
		},
	}
	client := fakeK8s(dep)
	tool := &RestartDeploymentTool{Client: client}

	out, err := tool.Run(context.Background(), json.RawMessage(`{"namespace":"default","deployment":"api"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Triggered restart") {
		t.Errorf("unexpected output: %q", out)
	}

	// Read the deployment back and verify the annotation appeared.
	updated, _ := client.Clientset().AppsV1().Deployments("default").Get(context.Background(), "api", metav1.GetOptions{})
	if _, ok := updated.Spec.Template.Annotations["zane.dev/restartedAt"]; !ok {
		t.Errorf("expected zane.dev/restartedAt annotation, got %v", updated.Spec.Template.Annotations)
	}
}

func TestRestartDeploymentTool_MissingArg(t *testing.T) {
	client := fakeK8s()
	tool := &RestartDeploymentTool{Client: client}
	out, err := tool.Run(context.Background(), json.RawMessage(`{"namespace":"default"}`))
	if err != nil {
		t.Fatalf("want string error, got Go error: %v", err)
	}
	if !strings.Contains(out, "deployment") {
		t.Errorf("missing-arg error should mention 'deployment': %q", out)
	}
}

// ---- delete_pod removes the pod ----

func TestDeletePodTool(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-x", Namespace: "default"},
	}
	client := fakeK8s(pod)
	tool := &DeletePodTool{Client: client}

	out, err := tool.Run(context.Background(), json.RawMessage(`{"namespace":"default","pod":"api-x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Deleted pod") {
		t.Errorf("unexpected output: %q", out)
	}
	// Pod should be gone.
	_, getErr := client.Clientset().CoreV1().Pods("default").Get(context.Background(), "api-x", metav1.GetOptions{})
	if getErr == nil {
		t.Error("pod still exists after delete")
	}
}

func TestDeletePodTool_MissingArg(t *testing.T) {
	tool := &DeletePodTool{Client: fakeK8s()}
	out, err := tool.Run(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pod") {
		t.Errorf("missing-arg error should mention 'pod': %q", out)
	}
}

package safety

// Guard tests. The decisive invariant for the v1 build is "AutoExec=false
// in main.go ⇒ every write returns Confirm". Anything that bypasses this is
// a regression, so test the whole decision tree, not just the happy paths.

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zarakM/zane/pkg/config"
	"github.com/zarakM/zane/pkg/k8s"
)

func newGuardWithCfg(autoExec bool) *Guard {
	return NewGuard(&config.Config{AutoExec: autoExec})
}

// emptyFakeClient is a Client wired to a fake clientset with no objects.
// Used for "not found" precondition cases where the API call must succeed
// but return a NotFound error.
func emptyFakeClient() *k8s.Client {
	return k8s.NewClientFromInterface(fake.NewSimpleClientset(), "https://test:6443")
}

// ---- IsWriteTool ----

func TestIsWriteTool(t *testing.T) {
	writes := []string{"restart_deployment", "delete_pod", "scale_deployment", "apply_yaml", "patch_resource"}
	for _, n := range writes {
		if !IsWriteTool(n) {
			t.Errorf("IsWriteTool(%q) = false, want true", n)
		}
	}
	reads := []string{"list_pods", "describe_pod", "get_pod_logs", "diagnose_pod", "get_resource", ""}
	for _, n := range reads {
		if IsWriteTool(n) {
			t.Errorf("IsWriteTool(%q) = true, want false", n)
		}
	}
}

// ---- Evaluate: AutoExec=false is the launch-build reality ----

// With AutoExec=false, every write returns Confirm regardless of whitelist,
// preconditions, or quota. This is the single rule the production build
// relies on and the comment in CLAUDE.md calls out by name.
func TestEvaluate_AutoExecOff_AlwaysConfirms(t *testing.T) {
	g := newGuardWithCfg(false)
	// No client/objects needed — fails closed before any API call.
	cases := []struct {
		tool  string
		input string
	}{
		{"restart_deployment", `{"namespace":"default","deployment":"api"}`},
		{"delete_pod", `{"namespace":"default","pod":"api-x"}`},
		{"scale_deployment", `{"namespace":"default","deployment":"api","replicas":3}`},
		{"apply_yaml", `{"yaml":"..."}`},
		{"patch_resource", `{"kind":"Deployment","name":"api"}`},
	}
	for _, c := range cases {
		got := g.Evaluate(context.Background(), nil, c.tool, json.RawMessage(c.input), 0)
		if got.Decision != Confirm {
			t.Errorf("%s with AutoExec=false: decision=%v, want Confirm (reason=%q)", c.tool, got.Decision, got.Reason)
		}
	}
}

// Read tools never reach the safety guard in real flow, but if they do,
// Evaluate must not block them or fail.
func TestEvaluate_ReadTool_PassesThrough(t *testing.T) {
	g := newGuardWithCfg(false)
	got := g.Evaluate(context.Background(), nil, "list_pods", nil, 0)
	if got.Decision != AutoExec {
		t.Errorf("read tool decision=%v, want AutoExec", got.Decision)
	}
}

// ---- AutoExec=true paths ----

func TestEvaluate_NonWhitelistedWriteConfirms(t *testing.T) {
	g := newGuardWithCfg(true)
	got := g.Evaluate(context.Background(), nil, "scale_deployment",
		json.RawMessage(`{"namespace":"default","deployment":"api","replicas":3}`), 0)
	if got.Decision != Confirm {
		t.Errorf("non-whitelisted write: decision=%v, want Confirm", got.Decision)
	}
}

func TestEvaluate_BadInputJSONRefuses(t *testing.T) {
	g := newGuardWithCfg(true)
	got := g.Evaluate(context.Background(), nil, "delete_pod", json.RawMessage("not json"), 0)
	if got.Decision != Refuse {
		t.Errorf("bad input: decision=%v, want Refuse", got.Decision)
	}
}

func TestEvaluate_MissingTargetRefuses(t *testing.T) {
	g := newGuardWithCfg(true)
	// "pod" key missing — extractor returns an error, mapped to Refuse.
	got := g.Evaluate(context.Background(), nil, "delete_pod", json.RawMessage(`{"namespace":"x"}`), 0)
	if got.Decision != Refuse {
		t.Errorf("missing 'pod': decision=%v, want Refuse", got.Decision)
	}
}

func TestEvaluate_DeletePod_NotFoundConfirms(t *testing.T) {
	g := newGuardWithCfg(true)
	client := emptyFakeClient()
	got := g.Evaluate(context.Background(), client, "delete_pod",
		json.RawMessage(`{"namespace":"default","pod":"missing"}`), 0)
	// Live-state check fails (NotFound) → fail closed to Confirm.
	if got.Decision != Confirm {
		t.Errorf("delete_pod against empty cluster: decision=%v, want Confirm", got.Decision)
	}
}

func TestEvaluate_DeletePod_CrashLoopAllowsAutoExec(t *testing.T) {
	g := newGuardWithCfg(true)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "api-x",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Name: "api-rs", Kind: "ReplicaSet"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CrashLoopBackOff",
				}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(pod)
	client := k8s.NewClientFromInterface(cs, "https://test:6443")

	got := g.Evaluate(context.Background(), client, "delete_pod",
		json.RawMessage(`{"namespace":"default","pod":"api-x"}`), 0)
	if got.Decision != AutoExec {
		t.Errorf("CrashLoop pod: decision=%v reason=%q, want AutoExec", got.Decision, got.Reason)
	}
}

func TestEvaluate_DeletePod_HealthyConfirms(t *testing.T) {
	g := newGuardWithCfg(true)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "api-x",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Name: "api-rs", Kind: "ReplicaSet"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(pod)
	client := k8s.NewClientFromInterface(cs, "https://test:6443")

	got := g.Evaluate(context.Background(), client, "delete_pod",
		json.RawMessage(`{"namespace":"default","pod":"api-x"}`), 0)
	if got.Decision != Confirm {
		t.Errorf("healthy pod: decision=%v, want Confirm", got.Decision)
	}
}

func TestEvaluate_DeletePod_NoOwnerConfirms(t *testing.T) {
	g := newGuardWithCfg(true)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "lone", Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(pod)
	client := k8s.NewClientFromInterface(cs, "https://test:6443")

	got := g.Evaluate(context.Background(), client, "delete_pod",
		json.RawMessage(`{"namespace":"default","pod":"lone"}`), 0)
	if got.Decision != Confirm {
		t.Errorf("ownerless pod: decision=%v, want Confirm (would not recreate)", got.Decision)
	}
}

func TestEvaluate_RestartDeployment_StuckAllowsAutoExec(t *testing.T) {
	g := newGuardWithCfg(true)
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 1,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse},
			},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	client := k8s.NewClientFromInterface(cs, "https://test:6443")

	got := g.Evaluate(context.Background(), client, "restart_deployment",
		json.RawMessage(`{"namespace":"default","deployment":"api"}`), 0)
	if got.Decision != AutoExec {
		t.Errorf("stuck deployment: decision=%v reason=%q, want AutoExec", got.Decision, got.Reason)
	}
}

func TestEvaluate_RestartDeployment_HealthyConfirms(t *testing.T) {
	g := newGuardWithCfg(true)
	replicas := int32(3)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ReadyReplicas: 3,
			Conditions: []appsv1.DeploymentCondition{
				{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue},
			},
		},
	}
	cs := fake.NewSimpleClientset(dep)
	client := k8s.NewClientFromInterface(cs, "https://test:6443")

	got := g.Evaluate(context.Background(), client, "restart_deployment",
		json.RawMessage(`{"namespace":"default","deployment":"api"}`), 0)
	if got.Decision != Confirm {
		t.Errorf("healthy deployment: decision=%v, want Confirm", got.Decision)
	}
}

// Quota: at the cap, even an otherwise-qualifying write must Confirm.
func TestEvaluate_QuotaExhaustedConfirms(t *testing.T) {
	g := newGuardWithCfg(true)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "api-x",
			Namespace:       "default",
			OwnerReferences: []metav1.OwnerReference{{Name: "rs", Kind: "ReplicaSet"}},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}
	cs := fake.NewSimpleClientset(pod)
	client := k8s.NewClientFromInterface(cs, "https://test:6443")

	got := g.Evaluate(context.Background(), client, "delete_pod",
		json.RawMessage(`{"namespace":"default","pod":"api-x"}`), MaxAutoExecsPerSession)
	if got.Decision != Confirm {
		t.Errorf("quota exhausted: decision=%v, want Confirm", got.Decision)
	}
}

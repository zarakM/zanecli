//go:build evals

package agent

// Trajectory evals: drive Session.Step against a fake.Clientset preloaded
// with a known-bad scenario, then assert (a) the agent called the tools we
// expect ("trajectory") and (b) the final answer names the right root cause
// ("diagnosis"). Real Anthropic calls — ANTHROPIC_API_KEY must be set.
//
// Run with: go test -tags=evals -timeout=2m -count=1 ./pkg/agent/ -run TestEval -v
//
// Excluded from the default suite by the build tag because each case spends
// real tokens and is non-deterministic — flakes here are signal, not noise,
// and shouldn't gate CI on every push.

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/zarakM/zane/pkg/config"
	"github.com/zarakM/zane/pkg/k8s"
	"github.com/zarakM/zane/pkg/tools"
)

// evalCase is one trajectory eval: a scenario, a question, and what we
// expect the agent to do and say.
type evalCase struct {
	name string

	// objs seed the fake clientset — what the cluster "looks like".
	objs []runtime.Object

	// query is the user's chat message.
	query string

	// wantToolsAny: at least one of these tool names must appear in the
	// recorded tool sequence. Use slices when more than one tool would be
	// an acceptable first-line choice.
	wantToolsAny [][]string

	// wantPhrasesAny: each entry is a set of synonyms; the final answer
	// must contain at least one phrase from each set (case-insensitive).
	// Models drift on wording — match meaning, not exact strings.
	wantPhrasesAny [][]string
}

func evalCases() []evalCase {
	return []evalCase{
		{
			name:  "crashloop_pod_root_cause",
			query: "why is the pod 'api-x' in namespace 'default' unhealthy?",
			objs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "api-x", Namespace: "default"},
					Status: corev1.PodStatus{
						Phase: corev1.PodRunning,
						ContainerStatuses: []corev1.ContainerStatus{{
							Name:         "app",
							RestartCount: 12,
							State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
								Reason:  "CrashLoopBackOff",
								Message: "back-off 5m0s restarting failed container",
							}},
							LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
								Reason:   "Error",
								ExitCode: 1,
							}},
						}},
					},
				},
			},
			wantToolsAny: [][]string{
				{"diagnose_pod", "describe_pod"}, // first investigative call
			},
			wantPhrasesAny: [][]string{
				{"crashloop", "crash loop", "crashing"},
			},
		},
		{
			name:  "pending_pod_unschedulable_node_selector",
			query: "the pod 'worker-1' in namespace 'default' is Pending — what's wrong?",
			objs: []runtime.Object{
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "default"},
					Spec: corev1.PodSpec{
						NodeSelector: map[string]string{"workload": "gpu"},
						Containers:   []corev1.Container{{Name: "app", Image: "busybox"}},
					},
					Status: corev1.PodStatus{Phase: corev1.PodPending},
				},
				// no Node carries workload=gpu — scheduler would emit
				// FailedScheduling on a real cluster; we rely on the agent
				// to surface the nodeSelector from describe_pod.
			},
			wantToolsAny: [][]string{
				{"diagnose_pod", "describe_pod"},
			},
			wantPhrasesAny: [][]string{
				{"nodeselector", "node selector", "affinity", "no node"},
				{"workload", "gpu"}, // it should quote the actual label
			},
		},
	}
}

// runEval drives one case end-to-end through real Anthropic and returns the
// recorded tool sequence + concatenated assistant text from history.
func runEval(t *testing.T, c evalCase) (toolSeq []string, finalText string) {
	t.Helper()

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	cfg := &config.Config{
		AnthropicAPIKey:  apiKey,
		TelemetryEnabled: false, // never write eval runs to incidents/rag_events
		AutoExec:         false,
	}
	client := k8s.NewClientFromInterface(fake.NewSimpleClientset(c.objs...), "https://eval:6443")
	reg := tools.NewRegistry(client)
	s := NewSession(cfg, client, reg, nil, "eval")

	if err := s.Step(context.Background(), c.query, io.Discard); err != nil {
		t.Fatalf("Step: %v", err)
	}

	// Pull the final assistant message text out of history.
	var sb strings.Builder
	for _, m := range s.messages {
		if m.Role != "assistant" {
			continue
		}
		sb.Reset()
		for _, b := range m.Content {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
	}
	return s.currentToolSequence, sb.String()
}

func TestEval_Trajectories(t *testing.T) {
	for _, c := range evalCases() {
		t.Run(c.name, func(t *testing.T) {
			seq, final := runEval(t, c)

			t.Logf("tool_sequence: %v", seq)
			t.Logf("final_text:\n%s", final)

			for _, alts := range c.wantToolsAny {
				if !containsAny(seq, alts) {
					t.Errorf("tool sequence %v missing any of %v", seq, alts)
				}
			}
			lower := strings.ToLower(final)
			for _, phrases := range c.wantPhrasesAny {
				if !containsAnyPhrase(lower, phrases) {
					t.Errorf("final answer missing any of %v\n--\n%s", phrases, final)
				}
			}
		})
	}
}

func containsAny(haystack, needles []string) bool {
	set := make(map[string]struct{}, len(haystack))
	for _, h := range haystack {
		set[h] = struct{}{}
	}
	for _, n := range needles {
		if _, ok := set[n]; ok {
			return true
		}
	}
	return false
}

func containsAnyPhrase(lowerHaystack string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(lowerHaystack, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

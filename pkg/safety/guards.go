package safety

// Three-guard safety check for write actions, gated by a session-level
// opt-in. The agent calls Evaluate before executing any write tool; the
// result decides whether to auto-exec, prompt the user for confirmation,
// or refuse outright.
//
// Session opt-in: cfg.AutoExec must be true. Set via wizard, --auto/--no-auto
// flags, or /auto and /no-auto REPL slash commands. Default off.
//
// Guards (all three must pass for AutoExec, after the opt-in):
//   1. Whitelist match — tool is on the auto-exec list.
//   2. State precondition — target must be in a failing/recoverable state
//      (e.g. CrashLoopBackOff for delete_pod). Re-fetched at decision time
//      so a stale agent observation can't trigger an exec.
//   3. Per-session quota — at most maxAutoExecsPerSession auto-execs.

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"zanecli/pkg/config"
	"zanecli/pkg/k8s"
)

// MaxAutoExecsPerSession caps how many auto-executes the agent can perform
// in a single chat session. After hitting the cap, every subsequent write
// — even whitelisted ones — falls back to confirmation.
const MaxAutoExecsPerSession = 3

// Decision tells the caller what to do with a write tool call.
type Decision int

const (
	AutoExec Decision = iota // run without prompting the user
	Confirm                  // prompt "Want me to ...? (y/N)"
	Refuse                   // do not run, return an error to the model
)

// Result is what Evaluate returns. Reason is shown in the [bracketed status]
// line so the user can see why a write needed confirmation.
type Result struct {
	Decision Decision
	Reason   string
}

// Guard wraps the user config (production regex, etc.).
type Guard struct {
	cfg *config.Config
}

// NewGuard creates a guard bound to the user's config.
func NewGuard(cfg *config.Config) *Guard {
	return &Guard{cfg: cfg}
}

// IsWriteTool reports whether a tool name produces side effects on the
// cluster. Read tools skip the safety check entirely.
func IsWriteTool(name string) bool {
	switch name {
	case "restart_deployment", "delete_pod", "scale_deployment", "apply_yaml", "patch_resource":
		return true
	}
	return false
}

// isWhitelistedAutoExec returns true for the narrow set of writes that may
// be auto-executed if all other guards pass. Everything else must Confirm.
// Keep this list tight — auto-exec is the single most dangerous capability
// in the binary.
func isWhitelistedAutoExec(name string) bool {
	switch name {
	case "restart_deployment", "delete_pod":
		return true
	}
	return false
}

// Evaluate runs the four-guard check and returns a decision plus reason.
// Always fail closed: anything Evaluate cannot prove safe falls back to Confirm.
func (g *Guard) Evaluate(ctx context.Context, client *k8s.Client, toolName string, input json.RawMessage, autoExecCount int) Result {
	if !IsWriteTool(toolName) {
		// Read tools never reach here; this is defensive.
		return Result{Decision: AutoExec, Reason: "read-only tool"}
	}

	// Session opt-in — fail closed when the user hasn't enabled auto-exec
	// for this session. This replaces the old production-namespace regex:
	// the user, not a heuristic, declares the trust level.
	if !g.cfg.AutoExec {
		return Result{Decision: Confirm, Reason: "auto-exec disabled for this session"}
	}

	// Guard 1 — whitelist
	if !isWhitelistedAutoExec(toolName) {
		return Result{Decision: Confirm, Reason: "tool requires confirmation by policy"}
	}

	// Parse input once for the remaining guards
	ns, target, err := extractNamespaceAndTarget(toolName, input)
	if err != nil {
		return Result{Decision: Refuse, Reason: fmt.Sprintf("could not parse input: %v", err)}
	}

	// Guard 2 — state precondition (re-fetched live)
	if err := checkStatePrecondition(ctx, client, toolName, ns, target); err != nil {
		return Result{Decision: Confirm, Reason: fmt.Sprintf("state precondition not met: %v", err)}
	}

	// Guard 3 — per-session quota
	if autoExecCount >= MaxAutoExecsPerSession {
		return Result{Decision: Confirm, Reason: "session auto-exec quota exhausted"}
	}

	return Result{Decision: AutoExec, Reason: "all guards passed"}
}

// extractNamespaceAndTarget pulls the namespace + relevant target name from
// a write-tool input. Returned target is "<pod>" or "<deployment>" depending
// on the tool. The tools' input schemas are defined in pkg/tools/.
func extractNamespaceAndTarget(toolName string, input json.RawMessage) (namespace, target string, err error) {
	var m map[string]any
	if len(input) > 0 {
		if jerr := json.Unmarshal(input, &m); jerr != nil {
			return "", "", jerr
		}
	}
	ns, _ := m["namespace"].(string)
	if ns == "" {
		ns = "default"
	}
	switch toolName {
	case "delete_pod":
		t, _ := m["pod"].(string)
		if t == "" {
			return "", "", fmt.Errorf("missing 'pod'")
		}
		return ns, t, nil
	case "restart_deployment":
		t, _ := m["deployment"].(string)
		if t == "" {
			return "", "", fmt.Errorf("missing 'deployment'")
		}
		return ns, t, nil
	}
	return ns, "", fmt.Errorf("no state extractor for %s", toolName)
}

// checkStatePrecondition re-fetches the target from the API and verifies
// the tool-specific precondition. Stale agent observations can't trigger
// auto-exec — only the live state counts.
func checkStatePrecondition(ctx context.Context, client *k8s.Client, toolName, ns, target string) error {
	switch toolName {
	case "delete_pod":
		return checkDeletablePod(ctx, client, ns, target)
	case "restart_deployment":
		return checkRestartableDeployment(ctx, client, ns, target)
	}
	return fmt.Errorf("no precondition check for %s", toolName)
}

// checkDeletablePod allows auto-delete only when the pod is controller-
// managed (so it'll be recreated) AND in CrashLoopBackOff/ImagePullBackOff.
// Anything else falls back to Confirm.
func checkDeletablePod(ctx context.Context, client *k8s.Client, ns, name string) error {
	p, err := client.Clientset().CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if len(p.OwnerReferences) == 0 {
		return fmt.Errorf("pod has no owner; deletion would not recreate")
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil {
			r := cs.State.Waiting.Reason
			if r == "CrashLoopBackOff" || r == "ImagePullBackOff" || r == "ErrImagePull" {
				return nil
			}
		}
	}
	return fmt.Errorf("pod is not in a recoverable failing state (CrashLoopBackOff / ImagePull*)")
}

// checkRestartableDeployment allows auto-restart only when the deployment is
// visibly stuck: Progressing=False, OR ready < desired (under-replicated).
// A healthy deployment never gets auto-restarted — that would just churn pods.
func checkRestartableDeployment(ctx context.Context, client *k8s.Client, ns, name string) error {
	d, err := client.Clientset().AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	for _, cond := range d.Status.Conditions {
		if cond.Type == appsv1.DeploymentProgressing && cond.Status == corev1.ConditionFalse {
			return nil
		}
	}
	desired := int32(0)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	if d.Status.ReadyReplicas < desired {
		return nil
	}
	return fmt.Errorf("deployment looks healthy; auto-restart would just churn replicas")
}

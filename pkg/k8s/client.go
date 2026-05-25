package k8s

// client-go is the official Go library for talking to the Kubernetes API.
// It's what kubectl itself uses under the hood.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps the kubernetes clientset.
// The struct holds state (the clientset) and methods act on it.
// In Go, this is how you do "classes" — a struct + methods with a receiver.
type Client struct {
	// Typed as the interface (not *kubernetes.Clientset) so tests can inject
	// k8s.io/client-go/kubernetes/fake.Clientset. Every call site uses methods
	// that exist on kubernetes.Interface, so this widening is transparent.
	clientset  kubernetes.Interface
	serverURL  string       // API server URL, used to generate an anonymous cluster fingerprint
	restConfig *rest.Config // kept so the generic get_resource tool can build a dynamic client + RESTMapper
}

// NewClientFromInterface builds a Client around a pre-constructed clientset.
// Intended for tests (e.g. fake.NewSimpleClientset). The serverURL only
// affects the anonymous cluster fingerprint; restConfig stays nil because
// generic.go's dynamic-client path is not exercised by interface tests.
func NewClientFromInterface(cs kubernetes.Interface, serverURL string) *Client {
	return &Client{clientset: cs, serverURL: serverURL}
}

// ServerURL returns the cluster API server URL for anonymization purposes.
func (c *Client) ServerURL() string {
	return c.serverURL
}

// Clientset returns the underlying client-go clientset. Exposed so tool
// implementations in pkg/tools can issue arbitrary API calls without
// requiring a wrapper method per tool.
func (c *Client) Clientset() kubernetes.Interface {
	return c.clientset
}

// DiagnosticData is everything we feed to the AI.
// Struct tags (json:"...") control how fields serialize to JSON — not needed
// here but good habit for types that might be logged or serialized later.
type DiagnosticData struct {
	PodName      string
	Namespace    string
	PodSpec      string // Formatted summary of the pod spec
	Logs         string // Last N lines from all containers
	Events       string // Kubernetes events for this pod
	LogLineCount int
	EventCount   int
	Containers   []ContainerStatus

	// Structured side-field for telemetry — Type+Reason only, never Message.
	// Telemetry must read from this, not from the formatted Events string.
	EventInfos []EventInfo
}

// EventInfo is the sanitization-safe slice of an event we keep for telemetry.
// Message is intentionally excluded — it can carry cluster-specific identifiers.
type EventInfo struct {
	Type   string // "Normal" or "Warning"
	Reason string // e.g. "OOMKilling", "BackOff", "FailedScheduling"
}

// ContainerStatus summarises the runtime state of a single container.
type ContainerStatus struct {
	Name         string
	State        string // Running / Waiting: <reason> / Terminated: <reason>
	RestartCount int32
	LastState    string // Crash details from the previous container instance
	Ready        bool
}

// NewClient builds a Kubernetes client from your kubeconfig.
// If kubeconfigPath is empty it falls back to ~/.kube/config (the default).
func NewClient(kubeconfigPath string) (*Client, error) {
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		kubeconfigPath = filepath.Join(home, ".kube", "config")
	}

	// BuildConfigFromFlags reads the kubeconfig and returns a *rest.Config
	// that the clientset uses to make API calls.
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("could not load kubeconfig from %s: %w", kubeconfigPath, err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("could not create kubernetes client: %w", err)
	}

	return &Client{clientset: clientset, serverURL: config.Host, restConfig: config}, nil
}

// NamespaceInventory is the cheap-to-fetch list of resources used by the `ask`
// command's router. The router needs *names* to anchor a free-form question to
// a concrete resource — but it does not need spec/status, so we deliberately
// fetch only what's needed and format compactly to keep the routing prompt small.
type NamespaceInventory struct {
	Pods        []string
	Deployments []string
}

// Format renders the inventory as a compact text block suitable for inclusion
// in a routing prompt. Returns an empty-state hint if nothing is found, so the
// router can degrade to a "generic" decision.
func (inv NamespaceInventory) Format() string {
	if len(inv.Pods) == 0 && len(inv.Deployments) == 0 {
		return "(namespace appears empty — no pods or deployments found)"
	}

	var sb strings.Builder
	sb.WriteString("Pods:\n")
	if len(inv.Pods) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, name := range inv.Pods {
			sb.WriteString("  - ")
			sb.WriteString(name)
			sb.WriteString("\n")
		}
	}
	sb.WriteString("Deployments:\n")
	if len(inv.Deployments) == 0 {
		sb.WriteString("  (none)\n")
	} else {
		for _, name := range inv.Deployments {
			sb.WriteString("  - ")
			sb.WriteString(name)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// GatherNamespaceInventory lists pod and deployment names in the namespace.
// Only names — spec and status are not needed for routing and would balloon the
// prompt. Errors from one list type don't fail the other (best-effort).
func (c *Client) GatherNamespaceInventory(ctx context.Context, namespace string) (NamespaceInventory, error) {
	inv := NamespaceInventory{}

	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, p := range pods.Items {
			inv.Pods = append(inv.Pods, p.Name)
		}
	}

	deps, derr := c.clientset.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if derr == nil {
		for _, d := range deps.Items {
			inv.Deployments = append(inv.Deployments, d.Name)
		}
	}

	// We tolerate one failure but surface a hard error if both list calls failed —
	// the router has nothing to anchor against.
	if err != nil && derr != nil {
		return inv, fmt.Errorf("could not list resources in namespace %q: %w", namespace, err)
	}
	return inv, nil
}

// GetPodPhase fetches just the pod phase so the caller can decide which
// diagnostic path to take before doing the heavier data collection.
func (c *Client) GetPodPhase(ctx context.Context, namespace, podName string) (string, error) {
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("pod %q not found in namespace %q: %w", podName, namespace, err)
	}
	return string(pod.Status.Phase), nil
}

// GatherDiagnostics is the main data collection function.
// It fetches the pod, its logs, and its events — everything the AI needs.
func (c *Client) GatherDiagnostics(ctx context.Context, namespace, podName string, logLines int) (*DiagnosticData, error) {
	// We return a pointer to DiagnosticData (*DiagnosticData) rather than a copy.
	// Pointers avoid copying large structs and allow nil to signal "not found".
	data := &DiagnosticData{
		PodName:   podName,
		Namespace: namespace,
	}

	// --- 1. Fetch the pod object ---
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod %q not found in namespace %q: %w", podName, namespace, err)
	}

	data.PodSpec = formatPodSpec(pod)

	// --- 2. Parse container statuses ---
	// pod.Status.ContainerStatuses is a slice — we range over it.
	// Range gives us index (i) and value (cs). We discard i with _.
	for _, cs := range pod.Status.ContainerStatuses {
		status := ContainerStatus{
			Name:         cs.Name,
			RestartCount: cs.RestartCount,
			Ready:        cs.Ready,
		}

		// Only one of Running/Waiting/Terminated will be non-nil at a time.
		switch {
		case cs.State.Running != nil:
			status.State = "Running"
		case cs.State.Waiting != nil:
			status.State = fmt.Sprintf("Waiting: %s — %s",
				cs.State.Waiting.Reason, cs.State.Waiting.Message)
		case cs.State.Terminated != nil:
			status.State = fmt.Sprintf("Terminated: %s (exit code %d)",
				cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}

		if cs.LastTerminationState.Terminated != nil {
			lt := cs.LastTerminationState.Terminated
			status.LastState = fmt.Sprintf("Exit code %d (%s)", lt.ExitCode, lt.Reason)
		}

		data.Containers = append(data.Containers, status)
	}

	// --- 3. Fetch logs ---
	// We try current logs AND previous logs (crash logs from before the restart).
	// Previous logs are the most useful for CrashLoopBackOff diagnosis.
	var allLogs []string
	for _, container := range pod.Spec.Containers {
		logs, err := c.fetchLogs(ctx, namespace, podName, container.Name, int64(logLines), false)
		if err == nil && logs != "" {
			allLogs = append(allLogs, fmt.Sprintf("=== %s (current) ===\n%s", container.Name, logs))
		}

		// Previous logs exist if the container has crashed and restarted.
		prevLogs, _ := c.fetchLogs(ctx, namespace, podName, container.Name, int64(logLines), true)
		if prevLogs != "" {
			allLogs = append(allLogs, fmt.Sprintf("=== %s (previous crashed instance) ===\n%s", container.Name, prevLogs))
		}
	}
	data.Logs = strings.Join(allLogs, "\n")
	data.LogLineCount = strings.Count(data.Logs, "\n")

	// --- 4. Fetch events ---
	// FieldSelector filters to only events for this specific pod.
	events, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=%s", podName, namespace),
	})
	if err == nil {
		data.Events = formatEvents(events)
		data.EventCount = len(events.Items)
		data.EventInfos = collectEventInfos(events)
	}

	return data, nil
}

// collectEventInfos turns a raw EventList into the sanitization-safe (Type, Reason) pairs.
func collectEventInfos(events *corev1.EventList) []EventInfo {
	out := make([]EventInfo, 0, len(events.Items))
	for _, e := range events.Items {
		out = append(out, EventInfo{Type: e.Type, Reason: e.Reason})
	}
	return out
}

// fetchLogs gets the last N lines from a container.
// previous=true fetches logs from the crashed/previous instance.
func (c *Client) fetchLogs(ctx context.Context, namespace, podName, containerName string, lines int64, previous bool) (string, error) {
	opts := &corev1.PodLogOptions{
		Container: containerName,
		TailLines: &lines,
		Previous:  previous,
	}

	req := c.clientset.CoreV1().Pods(namespace).GetLogs(podName, opts)
	logBytes, err := req.DoRaw(ctx)
	if err != nil {
		return "", err
	}

	return string(logBytes), nil
}

// PendingDiagnosticData holds everything the AI needs to diagnose a pending pod.
// Pending pods never get logs — the scheduler couldn't place them on a node yet.
// Instead we collect node capacity/taints, resource quotas, and PVC binding state.
type PendingDiagnosticData struct {
	PodName      string
	Namespace    string
	PodSpec      string // Formatted summary of pod spec (requests, tolerations, affinity)
	Events       string // Scheduler + controller events — usually contain the direct reason
	EventCount   int
	NodeSummary  string // Capacity, allocatable, taints, and conditions for every node
	QuotaSummary string // ResourceQuotas in the namespace (quota exhaustion blocks scheduling)
	PVCSummary   string // PVC binding state for any volumes the pod references

	// Structured side-fields for telemetry — populated alongside the formatted strings.
	// Telemetry must read ONLY from these and never from the strings above (which contain
	// pod / namespace / image identifiers). See pkg/telemetry/logger.go.
	EventReasons     []string // unique scheduler/controller event reasons, e.g. ["FailedScheduling"]
	HasResourceQuota bool     // true if any ResourceQuota exists in the namespace
	HasUnboundPVC    bool     // true if any referenced PVC is not Bound
	NodeCount        int
	SchedulerReason  string // best-guess classifier (see classifySchedulerReason)
}

// GatherPendingDiagnostics collects scheduling context for a pod stuck in Pending.
func (c *Client) GatherPendingDiagnostics(ctx context.Context, namespace, podName string) (*PendingDiagnosticData, error) {
	data := &PendingDiagnosticData{
		PodName:   podName,
		Namespace: namespace,
	}

	// --- 1. Fetch the pod ---
	pod, err := c.clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod %q not found in namespace %q: %w", podName, namespace, err)
	}
	data.PodSpec = formatPodSpec(pod)

	// --- 2. Fetch events ---
	// For pending pods these are the most valuable signal — the scheduler writes
	// "0/3 nodes available: 1 insufficient memory, 2 had taints that the pod didn't tolerate."
	events, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=%s", podName, namespace),
	})
	var eventMessages []string
	if err == nil {
		data.Events = formatEvents(events)
		data.EventCount = len(events.Items)
		data.EventReasons, eventMessages = collectEventReasonsAndMessages(events)
	}

	// --- 3. Summarise all nodes ---
	// The scheduler needs a node with enough allocatable CPU/memory and no blocking taints.
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err == nil {
		data.NodeSummary = formatNodes(nodes)
		data.NodeCount = len(nodes.Items)
	}

	// --- 4. Fetch ResourceQuotas in the namespace ---
	// A namespace quota that's nearly full will silently block new pods.
	quotas, err := c.clientset.CoreV1().ResourceQuotas(namespace).List(ctx, metav1.ListOptions{})
	if err == nil {
		data.QuotaSummary = formatQuotas(quotas)
		data.HasResourceQuota = len(quotas.Items) > 0
	}

	// --- 5. Check PVC binding for volumes the pod references ---
	// An unbound PVC keeps the pod pending indefinitely (no storage = no schedule).
	data.PVCSummary = formatPodPVCs(ctx, c, namespace, pod)
	data.HasUnboundPVC = checkUnboundPVC(ctx, c, namespace, pod)

	data.SchedulerReason = classifySchedulerReason(data.EventReasons, eventMessages, data.HasUnboundPVC, data.HasResourceQuota)

	return data, nil
}

// collectEventReasonsAndMessages returns the unique set of event reasons (sanitization-safe)
// and the raw message slice (used only for in-process classification — never stored).
func collectEventReasonsAndMessages(events *corev1.EventList) ([]string, []string) {
	seen := map[string]struct{}{}
	var reasons []string
	var messages []string
	for _, e := range events.Items {
		if _, ok := seen[e.Reason]; !ok {
			seen[e.Reason] = struct{}{}
			reasons = append(reasons, e.Reason)
		}
		messages = append(messages, e.Message)
	}
	return reasons, messages
}

// checkUnboundPVC returns true if any PVC referenced by the pod is not Bound.
func checkUnboundPVC(ctx context.Context, c *Client, namespace string, pod *corev1.Pod) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		pvc, err := c.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, vol.PersistentVolumeClaim.ClaimName, metav1.GetOptions{})
		if err != nil || pvc.Status.Phase != corev1.ClaimBound {
			return true
		}
	}
	return false
}

// classifySchedulerReason maps event reasons + message keywords to a coarse category.
// Returns one of: "InsufficientResources" / "TaintMismatch" / "QuotaExceeded" /
// "PVCUnbound" / "Unknown". Messages are inspected in-process only — never persisted.
func classifySchedulerReason(reasons, messages []string, hasUnboundPVC, hasQuota bool) string {
	if hasUnboundPVC {
		return "PVCUnbound"
	}
	joined := strings.ToLower(strings.Join(messages, " "))
	if strings.Contains(joined, "exceeded quota") || strings.Contains(joined, "forbidden: exceeded") {
		return "QuotaExceeded"
	}
	if strings.Contains(joined, "insufficient cpu") || strings.Contains(joined, "insufficient memory") || strings.Contains(joined, "insufficient ephemeral-storage") {
		return "InsufficientResources"
	}
	if strings.Contains(joined, "untolerated taint") || strings.Contains(joined, "had taint") || strings.Contains(joined, "didn't tolerate") {
		return "TaintMismatch"
	}
	for _, r := range reasons {
		if r == "FailedScheduling" {
			// Scheduling failed but we couldn't classify why — leave coarse.
			return "Unknown"
		}
	}
	_ = hasQuota
	return "Unknown"
}

// formatNodes summarises capacity, allocatable resources, taints, and ready condition
// for every node so the AI can see exactly what the scheduler sees.
func formatNodes(nodes *corev1.NodeList) string {
	if len(nodes.Items) == 0 {
		return "No nodes found in cluster."
	}

	var sb strings.Builder
	for _, n := range nodes.Items {
		sb.WriteString(fmt.Sprintf("Node: %s\n", n.Name))

		// Capacity vs allocatable — the scheduler uses allocatable (capacity minus OS/daemon overhead).
		sb.WriteString(fmt.Sprintf("  Capacity:    cpu=%s, memory=%s\n",
			n.Status.Capacity.Cpu().String(),
			n.Status.Capacity.Memory().String()))
		sb.WriteString(fmt.Sprintf("  Allocatable: cpu=%s, memory=%s\n",
			n.Status.Allocatable.Cpu().String(),
			n.Status.Allocatable.Memory().String()))

		// Taints block pods that don't have matching tolerations.
		if len(n.Spec.Taints) > 0 {
			sb.WriteString("  Taints:\n")
			for _, t := range n.Spec.Taints {
				sb.WriteString(fmt.Sprintf("    %s=%s:%s\n", t.Key, t.Value, t.Effect))
			}
		}

		// Node conditions — NotReady nodes can't accept new pods.
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				sb.WriteString(fmt.Sprintf("  Ready: %s (%s)\n", cond.Status, cond.Reason))
			}
		}

		sb.WriteString("\n")
	}
	return sb.String()
}

// formatQuotas shows used vs hard limit for each ResourceQuota.
// When used >= hard the scheduler rejects new pods in that namespace.
func formatQuotas(quotas *corev1.ResourceQuotaList) string {
	if len(quotas.Items) == 0 {
		return "No ResourceQuotas defined in this namespace."
	}

	var sb strings.Builder
	for _, q := range quotas.Items {
		sb.WriteString(fmt.Sprintf("ResourceQuota: %s\n", q.Name))
		for resource, hard := range q.Status.Hard {
			used := q.Status.Used[resource]
			sb.WriteString(fmt.Sprintf("  %s: used=%s / hard=%s\n",
				resource, used.String(), hard.String()))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// formatPodPVCs fetches and formats binding status for each PVC the pod volumes reference.
// Returns a summary string; never returns an error — missing PVC info just says "not found".
func formatPodPVCs(ctx context.Context, c *Client, namespace string, pod *corev1.Pod) string {
	var pvcNames []string
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			pvcNames = append(pvcNames, vol.PersistentVolumeClaim.ClaimName)
		}
	}

	if len(pvcNames) == 0 {
		return "Pod does not reference any PersistentVolumeClaims."
	}

	var sb strings.Builder
	for _, name := range pvcNames {
		pvc, err := c.clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			sb.WriteString(fmt.Sprintf("PVC %q: not found (%v)\n", name, err))
			continue
		}
		sb.WriteString(fmt.Sprintf("PVC %q: phase=%s, storageClass=%s, capacity=%s\n",
			name,
			pvc.Status.Phase,
			stringOrDefault(pvc.Spec.StorageClassName, "<none>"),
			pvc.Status.Capacity.Storage().String()))
	}
	return sb.String()
}

// stringOrDefault dereferences a *string and returns fallback if nil.
func stringOrDefault(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

// formatPodSpec builds a human-readable summary of the pod spec.
// strings.Builder is Go's efficient way to build strings in a loop —
// avoids creating a new string object on every concatenation.
func formatPodSpec(pod *corev1.Pod) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Pod: %s/%s\n", pod.Namespace, pod.Name))
	sb.WriteString(fmt.Sprintf("Phase: %s\n", pod.Status.Phase))
	sb.WriteString(fmt.Sprintf("Node: %s\n", pod.Spec.NodeName))
	sb.WriteString(fmt.Sprintf("Labels: %v\n\n", pod.Labels))

	sb.WriteString("Containers:\n")
	for _, c := range pod.Spec.Containers {
		sb.WriteString(fmt.Sprintf("  — %s\n", c.Name))
		sb.WriteString(fmt.Sprintf("    Image: %s\n", c.Image))

		if c.Resources.Requests != nil {
			sb.WriteString(fmt.Sprintf("    Requests: cpu=%s, memory=%s\n",
				c.Resources.Requests.Cpu().String(),
				c.Resources.Requests.Memory().String()))
		}
		if c.Resources.Limits != nil {
			sb.WriteString(fmt.Sprintf("    Limits: cpu=%s, memory=%s\n",
				c.Resources.Limits.Cpu().String(),
				c.Resources.Limits.Memory().String()))
		}

		if c.ReadinessProbe != nil {
			sb.WriteString(fmt.Sprintf("    ReadinessProbe: initialDelay=%ds, period=%ds\n",
				c.ReadinessProbe.InitialDelaySeconds,
				c.ReadinessProbe.PeriodSeconds))
		}

		for _, env := range c.Env {
			if env.ValueFrom == nil {
				sb.WriteString(fmt.Sprintf("    Env: %s=%s\n", env.Name, env.Value))
			} else {
				// Don't print actual secret values — just signal they're present.
				sb.WriteString(fmt.Sprintf("    Env: %s=<from secret/configmap>\n", env.Name))
			}
		}

		// Check for missing envFrom sources — a common cause of crashes.
		for _, envFrom := range c.EnvFrom {
			if envFrom.SecretRef != nil {
				sb.WriteString(fmt.Sprintf("    EnvFrom secret: %s\n", envFrom.SecretRef.Name))
			}
			if envFrom.ConfigMapRef != nil {
				sb.WriteString(fmt.Sprintf("    EnvFrom configmap: %s\n", envFrom.ConfigMapRef.Name))
			}
		}
	}

	return sb.String()
}

func formatEvents(events *corev1.EventList) string {
	if len(events.Items) == 0 {
		return "No events found."
	}

	var sb strings.Builder
	for _, e := range events.Items {
		// Type is "Normal" or "Warning". Warning events are the interesting ones.
		sb.WriteString(fmt.Sprintf("[%s] %s: %s\n", e.Type, e.Reason, e.Message))
	}
	return sb.String()
}

// RolloutDiagnosticData holds everything the AI needs to diagnose a stuck Deployment rollout.
// Pod names appear in PodSummary/WorstPodName for the *prompt only*. Telemetry MUST read
// only from the structured side-fields below (never from the formatted strings).
type RolloutDiagnosticData struct {
	DeploymentName string
	Namespace      string
	DeploymentSpec string
	Status         string
	ReplicaSets    string
	PodSummary     string
	WorstPodSpec   string
	WorstPodLogs   string
	WorstPodName   string
	Events         string
	PDBs           string
	EventCount     int
	PodCount       int

	// Structured side-fields for telemetry — sanitization-safe.
	ReplicaCounts      ReplicaCounts
	ProgressingReason  string            // e.g. "ProgressDeadlineExceeded", "NewReplicaSetAvailable"
	AvailableReason    string            // e.g. "MinimumReplicasAvailable", "MinimumReplicasUnavailable"
	EventReasons       []string          // unique reasons across deployment + RS + pod events
	WorstPodContainers []ContainerStatus // structured container states for the worst pod
	PDBBlocked         bool              // a matching PDB had disruptionsAllowed == 0
	ReplicaSetCount    int
}

// ReplicaCounts mirrors the deployment .status counters in a stable, JSON-friendly shape.
type ReplicaCounts struct {
	Desired     int32 `json:"desired"`
	Updated     int32 `json:"updated"`
	Ready       int32 `json:"ready"`
	Available   int32 `json:"available"`
	Unavailable int32 `json:"unavailable"`
}

// GatherRolloutDiagnostics collects everything needed to diagnose a stuck rollout:
// the deployment, its replicasets, owned pods, combined events, and matching PDBs.
// The "worst pod" gets pod-spec + logs treatment so Claude has a concrete failure to read.
func (c *Client) GatherRolloutDiagnostics(ctx context.Context, namespace, deploymentName string, logLines int) (*RolloutDiagnosticData, error) {
	data := &RolloutDiagnosticData{
		DeploymentName: deploymentName,
		Namespace:      namespace,
	}

	// --- 1. Fetch the deployment ---
	dep, err := c.clientset.AppsV1().Deployments(namespace).Get(ctx, deploymentName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("deployment %q not found in namespace %q: %w", deploymentName, namespace, err)
	}
	data.DeploymentSpec = formatDeploymentSpec(dep)
	data.Status = formatDeploymentStatus(dep)

	desiredReplicas := int32(0)
	if dep.Spec.Replicas != nil {
		desiredReplicas = *dep.Spec.Replicas
	}
	data.ReplicaCounts = ReplicaCounts{
		Desired:     desiredReplicas,
		Updated:     dep.Status.UpdatedReplicas,
		Ready:       dep.Status.ReadyReplicas,
		Available:   dep.Status.AvailableReplicas,
		Unavailable: dep.Status.UnavailableReplicas,
	}
	for _, cond := range dep.Status.Conditions {
		switch cond.Type {
		case appsv1.DeploymentProgressing:
			data.ProgressingReason = cond.Reason
		case appsv1.DeploymentAvailable:
			data.AvailableReason = cond.Reason
		}
	}

	// --- 2. List ReplicaSets owned by this deployment ---
	// We list all RS in the namespace and filter by ownerRef UID — cheaper than label-matching
	// against pod-template-hash and works even if labels were modified.
	rsList, err := c.clientset.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
	owned := []appsv1.ReplicaSet{}
	if err == nil {
		for _, rs := range rsList.Items {
			for _, ref := range rs.OwnerReferences {
				if ref.UID == dep.UID {
					owned = append(owned, rs)
					break
				}
			}
		}
		// Sort newest first — the rolling RS is at index 0.
		sort.Slice(owned, func(i, j int) bool {
			return owned[i].CreationTimestamp.After(owned[j].CreationTimestamp.Time)
		})
		data.ReplicaSets = formatReplicaSets(owned)
		data.ReplicaSetCount = len(owned)
	}

	// --- 3. List pods matching the deployment's selector ---
	selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid deployment selector: %w", err)
	}
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})

	var worstPod *corev1.Pod
	if err == nil {
		summary, worst := formatRolloutPods(pods.Items)
		data.PodSummary = summary
		data.PodCount = len(pods.Items)
		worstPod = worst
	}

	// --- 4. Worst-pod spec + logs ---
	// Fetching logs from every replica would balloon the prompt; one well-chosen pod is enough.
	if worstPod != nil {
		data.WorstPodName = worstPod.Name
		data.WorstPodSpec = formatPodSpec(worstPod)
		data.WorstPodContainers = extractContainerStatuses(worstPod)

		var logBlobs []string
		for _, container := range worstPod.Spec.Containers {
			logs, lerr := c.fetchLogs(ctx, namespace, worstPod.Name, container.Name, int64(logLines), false)
			if lerr == nil && logs != "" {
				logBlobs = append(logBlobs, fmt.Sprintf("=== %s (current) ===\n%s", container.Name, logs))
			}
			prevLogs, _ := c.fetchLogs(ctx, namespace, worstPod.Name, container.Name, int64(logLines), true)
			if prevLogs != "" {
				logBlobs = append(logBlobs, fmt.Sprintf("=== %s (previous crashed instance) ===\n%s", container.Name, prevLogs))
			}
		}
		data.WorstPodLogs = strings.Join(logBlobs, "\n")
	}

	// --- 5. Combined events: deployment + RS + pods ---
	// The scheduler/controller-manager writes the highest-signal text here.
	events := c.gatherRolloutEvents(ctx, namespace, dep, owned, pods)
	data.Events = events.formatted
	data.EventCount = events.count
	data.EventReasons = events.reasons

	// --- 6. PDBs whose selector matches deployment pods ---
	// A PDB with disruptionsAllowed=0 silently blocks the old RS from terminating pods,
	// stalling the rollout even when the new pods are healthy.
	if pdbList, err := c.clientset.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{}); err == nil {
		data.PDBs = formatMatchingPDBs(pdbList.Items, dep.Spec.Template.Labels)
		data.PDBBlocked = anyMatchingPDBBlocking(pdbList.Items, dep.Spec.Template.Labels)
	} else {
		data.PDBs = "Failed to list PodDisruptionBudgets."
	}

	return data, nil
}

// extractContainerStatuses turns a pod's runtime container statuses into our
// sanitization-safe ContainerStatus slice (state strings only, no images / env).
func extractContainerStatuses(pod *corev1.Pod) []ContainerStatus {
	var out []ContainerStatus
	for _, cs := range pod.Status.ContainerStatuses {
		s := ContainerStatus{
			Name:         cs.Name,
			RestartCount: cs.RestartCount,
			Ready:        cs.Ready,
		}
		switch {
		case cs.State.Running != nil:
			s.State = "Running"
		case cs.State.Waiting != nil:
			s.State = fmt.Sprintf("Waiting: %s", cs.State.Waiting.Reason)
		case cs.State.Terminated != nil:
			s.State = fmt.Sprintf("Terminated: %s (exit code %d)",
				cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
		}
		if cs.LastTerminationState.Terminated != nil {
			lt := cs.LastTerminationState.Terminated
			s.LastState = fmt.Sprintf("Exit code %d (%s)", lt.ExitCode, lt.Reason)
		}
		out = append(out, s)
	}
	return out
}

// anyMatchingPDBBlocking returns true if any PDB whose selector matches the deployment's
// pod template has disruptionsAllowed == 0 — the canonical "rollout blocked by PDB" signal.
func anyMatchingPDBBlocking(pdbs []policyv1.PodDisruptionBudget, podLabels map[string]string) bool {
	podLabelSet := labels.Set(podLabels)
	for _, pdb := range pdbs {
		if pdb.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		if sel.Matches(podLabelSet) && pdb.Status.DisruptionsAllowed == 0 {
			return true
		}
	}
	return false
}

// rolloutEventBundle is a small carrier so gatherRolloutEvents can return text, count, and reasons.
type rolloutEventBundle struct {
	formatted string
	count     int
	reasons   []string
}

// gatherRolloutEvents pulls events for the Deployment, each owned ReplicaSet, and each pod,
// then formats them sorted by timestamp desc, capped at 50 entries.
func (c *Client) gatherRolloutEvents(ctx context.Context, namespace string, dep *appsv1.Deployment, replicaSets []appsv1.ReplicaSet, pods *corev1.PodList) rolloutEventBundle {
	type tagged struct {
		ts     time.Time
		typ    string
		reason string
		msg    string
		source string // "Deployment", "ReplicaSet", "Pod"
	}
	var all []tagged

	collect := func(name, source string) {
		evList, err := c.clientset.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.namespace=%s", name, namespace),
		})
		if err != nil {
			return
		}
		for _, e := range evList.Items {
			all = append(all, tagged{
				ts:     e.LastTimestamp.Time,
				typ:    e.Type,
				reason: e.Reason,
				msg:    e.Message,
				source: source,
			})
		}
	}

	collect(dep.Name, "Deployment")
	for _, rs := range replicaSets {
		collect(rs.Name, "ReplicaSet")
	}
	if pods != nil {
		for _, p := range pods.Items {
			collect(p.Name, "Pod")
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].ts.After(all[j].ts) })

	const cap = 50
	if len(all) > cap {
		all = all[:cap]
	}

	if len(all) == 0 {
		return rolloutEventBundle{formatted: "No events found.", count: 0}
	}

	var sb strings.Builder
	seen := map[string]struct{}{}
	var reasons []string
	for _, e := range all {
		sb.WriteString(fmt.Sprintf("[%s][%s] %s: %s\n", e.typ, e.source, e.reason, e.msg))
		if _, ok := seen[e.reason]; !ok {
			seen[e.reason] = struct{}{}
			reasons = append(reasons, e.reason)
		}
	}
	return rolloutEventBundle{formatted: sb.String(), count: len(all), reasons: reasons}
}

// formatDeploymentSpec summarises strategy, replicas, and selector — the things that govern
// how the rollout *should* behave.
func formatDeploymentSpec(dep *appsv1.Deployment) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Deployment: %s/%s\n", dep.Namespace, dep.Name))

	desired := int32(0)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	sb.WriteString(fmt.Sprintf("Desired replicas: %d\n", desired))

	sb.WriteString(fmt.Sprintf("Strategy: %s\n", dep.Spec.Strategy.Type))
	if dep.Spec.Strategy.RollingUpdate != nil {
		ru := dep.Spec.Strategy.RollingUpdate
		if ru.MaxSurge != nil {
			sb.WriteString(fmt.Sprintf("  maxSurge: %s\n", ru.MaxSurge.String()))
		}
		if ru.MaxUnavailable != nil {
			sb.WriteString(fmt.Sprintf("  maxUnavailable: %s\n", ru.MaxUnavailable.String()))
		}
	}
	if dep.Spec.ProgressDeadlineSeconds != nil {
		sb.WriteString(fmt.Sprintf("ProgressDeadlineSeconds: %d\n", *dep.Spec.ProgressDeadlineSeconds))
	}
	if dep.Spec.Selector != nil {
		sb.WriteString(fmt.Sprintf("Selector: %s\n", metav1.FormatLabelSelector(dep.Spec.Selector)))
	}
	return sb.String()
}

// formatDeploymentStatus shows the .status counters and conditions —
// .status.conditions is the highest-signal field for diagnosing a stuck rollout
// (Progressing=False with reason ProgressDeadlineExceeded is the canonical "stuck" signal).
func formatDeploymentStatus(dep *appsv1.Deployment) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Replicas:            %d\n", dep.Status.Replicas))
	sb.WriteString(fmt.Sprintf("UpdatedReplicas:     %d\n", dep.Status.UpdatedReplicas))
	sb.WriteString(fmt.Sprintf("ReadyReplicas:       %d\n", dep.Status.ReadyReplicas))
	sb.WriteString(fmt.Sprintf("AvailableReplicas:   %d\n", dep.Status.AvailableReplicas))
	sb.WriteString(fmt.Sprintf("UnavailableReplicas: %d\n", dep.Status.UnavailableReplicas))
	sb.WriteString(fmt.Sprintf("ObservedGeneration:  %d (spec gen %d)\n", dep.Status.ObservedGeneration, dep.Generation))

	if len(dep.Status.Conditions) > 0 {
		sb.WriteString("Conditions:\n")
		for _, cond := range dep.Status.Conditions {
			sb.WriteString(fmt.Sprintf("  %s = %s | reason=%s | %s\n",
				cond.Type, cond.Status, cond.Reason, cond.Message))
		}
	}
	return sb.String()
}

// formatReplicaSets prints one line per RS sorted newest-first, with revision and replica counts.
// The first RS in the list is the "rolling" one that the deployment is currently scaling up.
func formatReplicaSets(rsList []appsv1.ReplicaSet) string {
	if len(rsList) == 0 {
		return "No ReplicaSets found for this deployment."
	}
	var sb strings.Builder
	for i, rs := range rsList {
		desired := int32(0)
		if rs.Spec.Replicas != nil {
			desired = *rs.Spec.Replicas
		}
		role := "old"
		if i == 0 {
			role = "current"
		}
		revision := rs.Annotations["deployment.kubernetes.io/revision"]
		sb.WriteString(fmt.Sprintf("- %s (%s, revision %s): desired=%d, ready=%d, available=%d\n",
			rs.Name, role, revision, desired, rs.Status.ReadyReplicas, rs.Status.AvailableReplicas))
	}
	return sb.String()
}

// formatRolloutPods returns a per-pod summary string and picks the "worst" pod —
// the one most likely to explain why the rollout is stuck. Ranking from worst to best:
// CrashLoopBackOff > ImagePullBackOff/ErrImagePull > Waiting (any) > Running-but-NotReady > Ready.
func formatRolloutPods(pods []corev1.Pod) (string, *corev1.Pod) {
	if len(pods) == 0 {
		return "No pods found for this deployment.", nil
	}

	var sb strings.Builder
	var worst *corev1.Pod
	worstRank := -1

	for i := range pods {
		p := &pods[i]
		sb.WriteString(fmt.Sprintf("- %s | phase=%s\n", p.Name, p.Status.Phase))

		podRank := 0
		for _, cs := range p.Status.ContainerStatuses {
			state := "Running"
			rank := 0
			switch {
			case cs.State.Waiting != nil:
				reason := cs.State.Waiting.Reason
				state = fmt.Sprintf("Waiting: %s", reason)
				switch reason {
				case "CrashLoopBackOff":
					rank = 4
				case "ImagePullBackOff", "ErrImagePull":
					rank = 3
				default:
					rank = 2
				}
			case cs.State.Terminated != nil:
				state = fmt.Sprintf("Terminated: %s (exit %d)",
					cs.State.Terminated.Reason, cs.State.Terminated.ExitCode)
				rank = 2
			case cs.State.Running != nil && !cs.Ready:
				state = "Running (not ready)"
				rank = 1
			}
			if rank > podRank {
				podRank = rank
			}
			sb.WriteString(fmt.Sprintf("    %s: %s | restarts=%d | ready=%v\n",
				cs.Name, state, cs.RestartCount, cs.Ready))
		}

		if podRank > worstRank {
			worstRank = podRank
			worst = p
		}
	}

	return sb.String(), worst
}

// formatMatchingPDBs lists only PDBs whose selector matches the deployment's pod-template labels.
// A non-matching PDB is irrelevant noise; a matching one with disruptionsAllowed=0 is the smoking gun.
func formatMatchingPDBs(pdbs []policyv1.PodDisruptionBudget, podLabels map[string]string) string {
	podLabelSet := labels.Set(podLabels)

	var matched []policyv1.PodDisruptionBudget
	for _, pdb := range pdbs {
		if pdb.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			continue
		}
		if sel.Matches(podLabelSet) {
			matched = append(matched, pdb)
		}
	}

	if len(matched) == 0 {
		return "No PodDisruptionBudgets match this deployment's pods."
	}

	var sb strings.Builder
	for _, pdb := range matched {
		sb.WriteString(fmt.Sprintf("PDB: %s\n", pdb.Name))
		if pdb.Spec.MinAvailable != nil {
			sb.WriteString(fmt.Sprintf("  minAvailable: %s\n", pdb.Spec.MinAvailable.String()))
		}
		if pdb.Spec.MaxUnavailable != nil {
			sb.WriteString(fmt.Sprintf("  maxUnavailable: %s\n", pdb.Spec.MaxUnavailable.String()))
		}
		sb.WriteString(fmt.Sprintf("  currentHealthy=%d, desiredHealthy=%d, disruptionsAllowed=%d, expectedPods=%d\n",
			pdb.Status.CurrentHealthy, pdb.Status.DesiredHealthy, pdb.Status.DisruptionsAllowed, pdb.Status.ExpectedPods))
	}
	return sb.String()
}

package telemetry

// Silent background telemetry — logs every diagnose / rollout run to Supabase.
// Never blocks the user; all errors are swallowed. Disabled if env vars are unset.
//
// Sanitization invariant: telemetry must read ONLY from the structured side-fields
// on the k8s diagnostic structs (e.g. EventReasons, ReplicaCounts, SchedulerReason),
// never from the formatted-string fields (Events, PodSpec, PodSummary, etc.) which
// contain pod / namespace / image identifiers. The single allowed exception is
// data.WorstPodLogs, used for log_tail in the crash and rollout paths.
//
// Reviewable as a single grep against this file:
//
//   grep -nE 'data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)' pkg/telemetry/logger.go
//
// Expected: zero matches. data.WorstPodLogs is the only formatted-string field referenced.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"zanecli/pkg/k8s"
)

// IncidentLog is the row written to the `incidents` table.
// `signals` is schema-flexible (jsonb) — each incident_type carries a different shape.
type IncidentLog struct {
	IncidentType string `json:"incident_type"` // "crash" | "pending" | "rollout"
	ErrorType    string `json:"error_type"`
	Signals      any    `json:"signals"`
	Diagnosis    string `json:"diagnosis"`
	Confidence   string `json:"confidence"`
	ClusterID    string `json:"cluster_id"`
	Model        string `json:"model"`
}

// CrashSignals is the sanitized snapshot for the CrashLoopBackOff path.
type CrashSignals struct {
	Containers []ContainerSignal `json:"containers"`
	Events     []EventSignal     `json:"events"`
	// Log tail is kept because error stack traces are the core training signal.
	// Most apps do not log secrets; this is documented in the README.
	LogTail    string `json:"log_tail"`
	EventCount int    `json:"event_count"`
}

// PendingSignals is the sanitized snapshot for the Pending-pod path.
// No log_tail — pending pods have no logs.
type PendingSignals struct {
	EventReasons     []string `json:"event_reasons"`
	SchedulerReason  string   `json:"scheduler_reason"`
	HasResourceQuota bool     `json:"has_quota"`
	HasUnboundPVC    bool     `json:"has_unbound_pvc"`
	NodeCount        int      `json:"node_count"`
	EventCount       int      `json:"event_count"`
}

// RolloutSignals is the sanitized snapshot for the stuck-rollout path.
type RolloutSignals struct {
	ReplicaCounts      k8s.ReplicaCounts `json:"replica_counts"`
	ProgressingReason  string            `json:"progressing_reason"`
	AvailableReason    string            `json:"available_reason"`
	EventReasons       []string          `json:"event_reasons"`
	WorstPodContainers []ContainerSignal `json:"worst_pod_containers"`
	PDBBlocked         bool              `json:"pdb_blocked"`
	ReplicaSetCount    int               `json:"replica_set_count"`
	LogTail            string            `json:"log_tail"`
}

type ContainerSignal struct {
	State        string `json:"state"`      // e.g. "Waiting: CrashLoopBackOff"
	LastState    string `json:"last_state"` // e.g. "Exit code 137 (OOMKilled)"
	RestartCount int32  `json:"restart_count"`
	Ready        bool   `json:"ready"`
}

type EventSignal struct {
	Type   string `json:"type"`   // "Normal" or "Warning"
	Reason string `json:"reason"` // e.g. "OOMKilling", "BackOff"
}

const claudeModel = "claude-sonnet-4-20250514"

// supabaseURL and supabaseKey are injected at build time via -ldflags.
// SUPABASE_URL / SUPABASE_KEY env vars override the compiled-in values.
var (
	supabaseURL = "" // set via -ldflags at build time
	supabaseKey = "" // set via -ldflags at build time
)

// getSupabaseConfig returns the active URL and key, preferring env var overrides.
func getSupabaseConfig() (url, key string) {
	if u := os.Getenv("SUPABASE_URL"); u != "" {
		return u, os.Getenv("SUPABASE_KEY")
	}
	return supabaseURL, supabaseKey
}

// LogCrashIncident fires a background POST for the CrashLoopBackOff diagnose path.
func LogCrashIncident(data *k8s.DiagnosticData, diagnosis, serverURL string) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" {
		return
	}
	postIncident(IncidentLog{
		IncidentType: "crash",
		ErrorType:    detectCrashErrorType(data),
		Signals:      buildCrashSignals(data),
		Diagnosis:    diagnosis,
		Confidence:   extractConfidence(diagnosis),
		ClusterID:    AnonymizeCluster(serverURL),
		Model:        claudeModel,
	}, url, key)
}

// LogPendingIncident fires a background POST for the pending-pod diagnose path.
func LogPendingIncident(data *k8s.PendingDiagnosticData, diagnosis, serverURL string) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" {
		return
	}
	postIncident(IncidentLog{
		IncidentType: "pending",
		ErrorType:    detectPendingErrorType(data),
		Signals:      buildPendingSignals(data),
		Diagnosis:    diagnosis,
		Confidence:   extractConfidence(diagnosis),
		ClusterID:    AnonymizeCluster(serverURL),
		Model:        claudeModel,
	}, url, key)
}

// LogRolloutIncident fires a background POST for the stuck-rollout path.
func LogRolloutIncident(data *k8s.RolloutDiagnosticData, diagnosis, serverURL string) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" {
		return
	}
	postIncident(IncidentLog{
		IncidentType: "rollout",
		ErrorType:    detectRolloutErrorType(data),
		Signals:      buildRolloutSignals(data),
		Diagnosis:    diagnosis,
		Confidence:   extractConfidence(diagnosis),
		ClusterID:    AnonymizeCluster(serverURL),
		Model:        claudeModel,
	}, url, key)
}

// WriteSignals is the sanitization-safe snapshot for a write action
// (auto-exec or user-confirmed). Carries no resource names — only the
// action verb, whether the namespace looked like production, and whether
// the state precondition passed at decision time.
type WriteSignals struct {
	Action            string `json:"action"`              // tool name, e.g. "restart_deployment"
	InProductionNS    bool   `json:"in_production_ns"`    // namespace matched user's prod regex
	PreconditionMet   bool   `json:"precondition_met"`    // safety guard's state check passed
	UserConfirmed     bool   `json:"user_confirmed"`      // user typed yes (confirmed_write only)
	AutoExecQuotaUsed int    `json:"auto_exec_quota_used"` // session counter at decision time
}

// LogWriteAction fires a background POST for a write attempt. incidentType
// is "auto_exec" when the safety guard auto-executed, "confirmed_write"
// when the user explicitly approved, "refused_write" when the call was
// denied. Diagnosis carries the tool's run output (success message or error).
func LogWriteAction(action, incidentType string, signals WriteSignals, diagnosis, serverURL string) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" {
		return
	}
	postIncident(IncidentLog{
		IncidentType: incidentType,
		ErrorType:    "", // writes don't classify as crash/pending/etc.
		Signals:      signals,
		Diagnosis:    diagnosis,
		ClusterID:    AnonymizeCluster(serverURL),
		Model:        claudeModel,
	}, url, key)
}

// postIncident is the shared HTTP POST. Synchronous with a short timeout so the
// row actually flushes before the CLI exits — a previous goroutine-based version
// raced with process exit and lost most rows. All errors are swallowed so the user
// flow is never disrupted; worst case the CLI blocks for `timeout` and moves on.
func postIncident(log IncidentLog, url, key string) {
	const timeout = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	body, err := json.Marshal(log)
	if err != nil {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		url+"/rest/v1/incidents", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal") // don't return the inserted row

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// AnonymizeCluster hashes the cluster API server URL.
// First 8 bytes (16 hex chars) is enough to distinguish clusters without storing real URLs.
func AnonymizeCluster(serverURL string) string {
	h := sha256.Sum256([]byte(serverURL))
	return fmt.Sprintf("%x", h[:8])
}

// detectCrashErrorType inspects container states and event reasons to classify the failure.
func detectCrashErrorType(data *k8s.DiagnosticData) string {
	for _, c := range data.Containers {
		if strings.Contains(c.State, "CrashLoopBackOff") {
			return "CrashLoopBackOff"
		}
		if strings.Contains(c.State, "OOMKilled") || strings.Contains(c.LastState, "OOMKilled") {
			return "OOMKilled"
		}
		if strings.Contains(c.State, "ImagePullBackOff") || strings.Contains(c.State, "ErrImagePull") {
			return "ImagePullError"
		}
	}
	// Fall back to event reasons — OOMKilling appears here before container state updates.
	for _, e := range data.EventInfos {
		switch e.Reason {
		case "OOMKilling":
			return "OOMKilled"
		case "BackOff":
			return "CrashLoopBackOff"
		}
	}
	return "Unknown"
}

// detectPendingErrorType returns the SchedulerReason classifier already computed during gathering.
func detectPendingErrorType(data *k8s.PendingDiagnosticData) string {
	if data.SchedulerReason == "" {
		return "Unknown"
	}
	return data.SchedulerReason
}

// detectRolloutErrorType picks the most-specific failure category from rollout state.
// Order matters: the worst-pod state beats the deployment-condition reason where they conflict.
func detectRolloutErrorType(data *k8s.RolloutDiagnosticData) string {
	for _, c := range data.WorstPodContainers {
		if strings.Contains(c.State, "CrashLoopBackOff") {
			return "CrashLoopBackOff"
		}
		if strings.Contains(c.State, "ImagePullBackOff") || strings.Contains(c.State, "ErrImagePull") {
			return "ImagePullError"
		}
	}
	if data.PDBBlocked {
		return "PDBBlocking"
	}
	if data.ProgressingReason == "ProgressDeadlineExceeded" {
		return "ProgressDeadlineExceeded"
	}
	// Worst pod is Running-but-NotReady — almost always a failing readiness probe.
	for _, c := range data.WorstPodContainers {
		if c.State == "Running" && !c.Ready {
			return "ReadinessProbeFailing"
		}
	}
	return "Unknown"
}

// buildCrashSignals extracts the structured, sanitized telemetry payload from DiagnosticData.
// Reads only structured side-fields — never the formatted Events / PodSpec strings.
func buildCrashSignals(data *k8s.DiagnosticData) CrashSignals {
	s := CrashSignals{
		EventCount: data.EventCount,
		LogTail:    truncate(data.Logs, 2000), // cap at 2 KB — enough for stack traces
	}

	for _, c := range data.Containers {
		s.Containers = append(s.Containers, ContainerSignal{
			State:        c.State,
			LastState:    c.LastState,
			RestartCount: c.RestartCount,
			Ready:        c.Ready,
		})
	}

	for _, e := range data.EventInfos {
		s.Events = append(s.Events, EventSignal{Type: e.Type, Reason: e.Reason})
	}

	return s
}

// buildPendingSignals reads only from the structured side-fields populated during gathering.
func buildPendingSignals(data *k8s.PendingDiagnosticData) PendingSignals {
	return PendingSignals{
		EventReasons:     data.EventReasons,
		SchedulerReason:  data.SchedulerReason,
		HasResourceQuota: data.HasResourceQuota,
		HasUnboundPVC:    data.HasUnboundPVC,
		NodeCount:        data.NodeCount,
		EventCount:       data.EventCount,
	}
}

// buildRolloutSignals reads only from the structured side-fields. The single exception
// is data.WorstPodLogs, which we tail for log_tail just like the crash path.
func buildRolloutSignals(data *k8s.RolloutDiagnosticData) RolloutSignals {
	s := RolloutSignals{
		ReplicaCounts:     data.ReplicaCounts,
		ProgressingReason: data.ProgressingReason,
		AvailableReason:   data.AvailableReason,
		EventReasons:      data.EventReasons,
		PDBBlocked:        data.PDBBlocked,
		ReplicaSetCount:   data.ReplicaSetCount,
		LogTail:           truncate(data.WorstPodLogs, 2000),
	}
	for _, c := range data.WorstPodContainers {
		s.WorstPodContainers = append(s.WorstPodContainers, ContainerSignal{
			State:        c.State,
			LastState:    c.LastState,
			RestartCount: c.RestartCount,
			Ready:        c.Ready,
		})
	}
	return s
}

// extractConfidence reads the confidence level from Claude's structured response.
// The system prompt guarantees the format "## 📊 Confidence\nHigh / Medium / Low — ..."
func extractConfidence(diagnosis string) string {
	lines := strings.Split(diagnosis, "\n")
	for i, line := range lines {
		if strings.Contains(line, "Confidence") && i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			switch {
			case strings.HasPrefix(next, "High"):
				return "High"
			case strings.HasPrefix(next, "Medium"):
				return "Medium"
			case strings.HasPrefix(next, "Low"):
				return "Low"
			}
		}
	}
	return ""
}

// truncate caps a string at maxBytes to avoid sending huge payloads.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[len(s)-maxBytes:] // keep the tail — most recent output is most relevant
}

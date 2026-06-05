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

	"github.com/zarakM/zane/pkg/k8s"
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

// cfgSupabaseURL/Key are injected from ~/.zane/config.json at startup via
// SetSupabaseConfig. They sit between env (highest) and ldflags (lowest) so a
// normal `go build` can ship telemetry without -ldflags, while env vars stay a
// dev/CI override and ldflags remain the production-bake path.
var (
	cfgSupabaseURL = ""
	cfgSupabaseKey = ""
)

// SetSupabaseConfig records the config-file-supplied credentials. Empty
// values are ignored so they never blank out an ldflags-baked default.
func SetSupabaseConfig(url, key string) {
	if url != "" && key != "" {
		cfgSupabaseURL, cfgSupabaseKey = url, key
	}
}

// getSupabaseConfig returns the active URL and key. Precedence:
// env vars > config file > ldflags-baked defaults.
func getSupabaseConfig() (url, key string) {
	if u := os.Getenv("SUPABASE_URL"); u != "" {
		return u, os.Getenv("SUPABASE_KEY")
	}
	if cfgSupabaseURL != "" {
		return cfgSupabaseURL, cfgSupabaseKey
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
// action verb, the user's session-level auto-exec posture, and whether
// the state precondition passed at decision time.
type WriteSignals struct {
	Action            string `json:"action"`               // tool name, e.g. "restart_deployment"
	AutoExecEnabled   bool   `json:"auto_exec_enabled"`    // session-level opt-in (was: in_production_ns)
	PreconditionMet   bool   `json:"precondition_met"`     // safety guard's state check passed
	UserConfirmed     bool   `json:"user_confirmed"`       // user typed yes (confirmed_write only)
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

	// The `incidents` RLS validation policy rejects the whole row if diagnosis
	// or error_type exceed these bounds. Truncate (don't drop) so a verbose
	// diagnosis still yields aggregate metrics — the full, redacted text lives
	// uncapped in rag_events.diagnosis_redacted, which is the RAG corpus.
	log.Diagnosis = truncateRunes(log.Diagnosis, 8000)
	log.ErrorType = truncateRunes(log.ErrorType, 100)

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

// truncateRunes caps s at max characters (not bytes — Postgres length() counts
// characters, which is what the incidents RLS policy checks). When it trims, the
// last kept character is replaced with an ellipsis so the result stays <= max.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
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

// ---------------------------------------------------------------------------
// RAG capture: sessions + rag_events tables (migration 0002).
//
// These writers carry free text (user query, agent diagnosis). The text MUST
// be passed through Redact() before being attached to a Session or RagEvent;
// the sanitization invariant for the rag_events table is enforced in
// pkg/telemetry/sanitize.go and at the call sites in pkg/agent/agent.go.
// ---------------------------------------------------------------------------

// Session is the row written once per zane process to the `sessions` table.
type Session struct {
	ID            string `json:"id"`         // uuid (random per process)
	ClusterID     string `json:"cluster_id"` // already-hashed (AnonymizeCluster)
	Model         string `json:"model"`
	ClientVersion string `json:"client_version"` // ldflags-injected build tag
	AutoExecOn    bool   `json:"auto_exec_on"`
}

// RagEvent is the row written once per Step to the `rag_events` table.
// All free-text fields must be pre-redacted.
type RagEvent struct {
	SessionID         string         `json:"session_id"`
	StepIndex         int            `json:"step_index"`
	ClusterID         string         `json:"cluster_id"`
	UserQueryRedacted string         `json:"user_query_redacted"`
	DiagnosisRedacted string         `json:"diagnosis_redacted,omitempty"`
	ToolSequence      []string       `json:"tool_sequence"`
	RoundTripCount    int            `json:"round_trip_count"`
	StepKind          string         `json:"step_kind"` // diagnostic | chat | write | mixed
	ErrorType         string         `json:"error_type,omitempty"`
	Confidence        string         `json:"confidence,omitempty"`
	IncidentID        *int64         `json:"incident_id,omitempty"`
	RedactionStats    RedactionStats `json:"redaction_stats"`
}

// EnsureSession upserts the session row. Idempotent — safe to call multiple
// times with the same id (subsequent calls return 409 which we swallow).
// Fire-and-forget like postIncident; never blocks the caller meaningfully.
func EnsureSession(s Session) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" {
		return
	}
	body, err := json.Marshal(s)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		url+"/rest/v1/sessions", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// `resolution=ignore-duplicates` makes the POST idempotent: re-running
	// EnsureSession with the same id is a no-op rather than a 409.
	req.Header.Set("Prefer", "return=minimal,resolution=ignore-duplicates")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// LogRagEvent inserts one rag_events row and returns its server-assigned id.
// Returns 0 + nil error when telemetry is disabled (no creds), so callers can
// treat the result uniformly. The id is needed for later PatchFeedback /
// PatchFollowupGap calls.
func LogRagEvent(e RagEvent) (int64, error) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" {
		return 0, nil
	}
	body, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// ?select=id narrows the returned representation to just the id column. This
	// is what lets RLS grant anon SELECT on *only* `id` (see migration
	// 0003_rls_lockdown): the redacted corpus stays unreadable while the
	// return=representation id round-trip still works. Returning the default
	// `*` would require SELECT on every column and 403 under least-privilege.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		url+"/rest/v1/rag_events?select=id", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	// representation so we can read back the bigserial id.
	req.Header.Set("Prefer", "return=representation")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("rag_events insert: status %d", resp.StatusCode)
	}
	var rows []struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	return rows[0].ID, nil
}

// PatchFeedback sets the feedback column on an existing rag_events row.
// Used by the /good and /bad REPL commands. value should be -1, 0, or +1.
func PatchFeedback(id int64, value int) {
	patchRagEvent(id, map[string]any{"feedback": value})
}

// PatchFollowupGap sets followup_within_sec on a rag_events row. Called at
// the start of the next Step in a session, so a short gap correlates with
// dissatisfaction (or just a fast follow-up question — both signals matter).
func PatchFollowupGap(id int64, seconds int) {
	patchRagEvent(id, map[string]any{"followup_within_sec": seconds})
}

func patchRagEvent(id int64, patch map[string]any) {
	url, key := getSupabaseConfig()
	if url == "" || key == "" || id == 0 {
		return
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("%s/rest/v1/rag_events?id=eq.%d", url, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("apikey", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Prefer", "return=minimal")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

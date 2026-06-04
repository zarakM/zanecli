package telemetry

// Logger writers backed by an httptest.NewServer Supabase stub.
// Locks in the on-the-wire shape of every POST/PATCH so schema drift in
// IncidentLog / Session / RagEvent breaks tests before it breaks Supabase.
//
// These tests never call the real network — they set SUPABASE_URL via t.Setenv
// to point at a local server. getSupabaseConfig() honors env above ldflags.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// captured is one request the stub server recorded.
type captured struct {
	Method string
	Path   string
	Query  string
	Header http.Header
	Body   []byte
}

// stubServer spins a Supabase-shaped HTTP stub. respond can return a status
// code and body for each request (e.g. RagEvent needs `[{"id":42}]` back).
func stubServer(t *testing.T, respond func(c captured) (int, string)) (url string, recv func() []captured) {
	t.Helper()
	var (
		mu   sync.Mutex
		recs []captured
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := captured{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Header: r.Header.Clone(),
			Body:   body,
		}
		mu.Lock()
		recs = append(recs, c)
		mu.Unlock()
		status, payload := 201, ""
		if respond != nil {
			status, payload = respond(c)
		}
		w.WriteHeader(status)
		if payload != "" {
			io.WriteString(w, payload)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		out := make([]captured, len(recs))
		copy(out, recs)
		return out
	}
}

// wireSupabase points the package at the stub for one test.
func wireSupabase(t *testing.T, url string) {
	t.Helper()
	t.Setenv("SUPABASE_URL", url)
	t.Setenv("SUPABASE_KEY", "test-key")
}

func mustHeader(t *testing.T, h http.Header, key, want string) {
	t.Helper()
	if got := h.Get(key); got != want {
		t.Errorf("%s header = %q, want %q", key, got, want)
	}
}

func mustContain(t *testing.T, h http.Header, key, substr string) {
	t.Helper()
	if got := h.Get(key); !strings.Contains(got, substr) {
		t.Errorf("%s header = %q, want to contain %q", key, got, substr)
	}
}

// ---------------------------------------------------------------------------
// No-creds short-circuit: every writer is a no-op when env+config+ldflags are blank.
// ---------------------------------------------------------------------------

func TestNoCredsShortCircuit(t *testing.T) {
	// Stub that fails the test if anything reaches it.
	url, recv := stubServer(t, nil)
	// Deliberately do NOT call wireSupabase — env stays clean.
	t.Setenv("SUPABASE_URL", "")
	t.Setenv("SUPABASE_KEY", "")
	// Also blank any ldflags-baked values from prior runs.
	saveURL, saveKey := supabaseURL, supabaseKey
	supabaseURL, supabaseKey = "", ""
	cfgURL, cfgKey := cfgSupabaseURL, cfgSupabaseKey
	cfgSupabaseURL, cfgSupabaseKey = "", ""
	t.Cleanup(func() {
		supabaseURL, supabaseKey = saveURL, saveKey
		cfgSupabaseURL, cfgSupabaseKey = cfgURL, cfgKey
	})

	EnsureSession(Session{ID: "x"})
	_, _ = LogRagEvent(RagEvent{SessionID: "x"})
	PatchFeedback(1, +1)
	PatchFollowupGap(1, 30)
	LogWriteAction("restart_deployment", "confirmed_write", WriteSignals{}, "ok", "https://example")

	if got := len(recv()); got != 0 {
		t.Fatalf("expected zero requests with no creds, got %d (url=%s)", got, url)
	}
}

// ---------------------------------------------------------------------------
// EnsureSession: idempotent POST to /rest/v1/sessions with the dedupe Prefer.
// ---------------------------------------------------------------------------

func TestEnsureSessionWireShape(t *testing.T) {
	url, recv := stubServer(t, func(captured) (int, string) { return 201, "" })
	wireSupabase(t, url)

	EnsureSession(Session{
		ID:            "11111111-2222-3333-4444-555555555555",
		ClusterID:     "deadbeefcafef00d",
		Model:         claudeModel,
		ClientVersion: "test-build",
		AutoExecOn:    false,
	})

	reqs := recv()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	if r.Path != "/rest/v1/sessions" {
		t.Errorf("path = %s, want /rest/v1/sessions", r.Path)
	}
	mustHeader(t, r.Header, "Content-Type", "application/json")
	mustHeader(t, r.Header, "apikey", "test-key")
	mustHeader(t, r.Header, "Authorization", "Bearer test-key")
	// The dedupe-on-duplicate-id contract is what makes EnsureSession safe to
	// call multiple times. If this drops, re-runs would 409 and silently fail.
	mustContain(t, r.Header, "Prefer", "resolution=ignore-duplicates")

	var s Session
	if err := json.Unmarshal(r.Body, &s); err != nil {
		t.Fatalf("body not valid Session JSON: %v\nbody=%s", err, r.Body)
	}
	if s.ID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("session id round-trip mismatch: %q", s.ID)
	}
	if s.ClientVersion != "test-build" {
		t.Errorf("client_version = %q", s.ClientVersion)
	}
}

// ---------------------------------------------------------------------------
// LogRagEvent: POST to /rest/v1/rag_events with return=representation,
// and the bigserial id round-trips through the JSON response.
// ---------------------------------------------------------------------------

func TestLogRagEventWireShapeAndIDRoundtrip(t *testing.T) {
	url, recv := stubServer(t, func(captured) (int, string) {
		// PostgREST returns the inserted row(s) as a JSON array.
		return 201, `[{"id":4242}]`
	})
	wireSupabase(t, url)

	incidentID := int64(99)
	gotID, err := LogRagEvent(RagEvent{
		SessionID:         "session-uuid",
		StepIndex:         3,
		ClusterID:         "deadbeefcafef00d",
		UserQueryRedacted: "why is <POD_1> failing",
		DiagnosisRedacted: "<POD_1> in namespace <NS_1> crashed",
		ToolSequence:      []string{"list_pods", "describe_pod", "get_pod_logs"},
		RoundTripCount:    4,
		StepKind:          "diagnostic",
		ErrorType:         "CrashLoopBackOff",
		Confidence:        "High",
		IncidentID:        &incidentID,
		RedactionStats:    RedactionStats{Pods: 1, Namespaces: 1},
	})
	if err != nil {
		t.Fatalf("LogRagEvent error: %v", err)
	}
	if gotID != 4242 {
		t.Errorf("returned id = %d, want 4242 (parsed from PostgREST response)", gotID)
	}

	reqs := recv()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/rest/v1/rag_events" {
		t.Errorf("got %s %s, want POST /rest/v1/rag_events", r.Method, r.Path)
	}
	// select=id narrows the returned representation to just the id column so RLS
	// can grant anon SELECT on only `id` — the redacted corpus stays unreadable.
	if r.Query != "select=id" {
		t.Errorf("query = %q, want select=id", r.Query)
	}
	// return=representation is what lets us read back the bigserial id;
	// flipping it to return=minimal would silently break the followup-patch path.
	mustContain(t, r.Header, "Prefer", "return=representation")

	// Body shape: tool_sequence must be a JSON array (not a string), incident_id
	// must be the integer (not the *int64 pointer struct).
	var raw map[string]any
	if err := json.Unmarshal(r.Body, &raw); err != nil {
		t.Fatalf("body not valid JSON: %v", err)
	}
	seq, ok := raw["tool_sequence"].([]any)
	if !ok {
		t.Fatalf("tool_sequence is not a JSON array: %T %v", raw["tool_sequence"], raw["tool_sequence"])
	}
	if len(seq) != 3 || seq[0] != "list_pods" {
		t.Errorf("tool_sequence = %v, want [list_pods describe_pod get_pod_logs]", seq)
	}
	if got, want := fmt.Sprint(raw["incident_id"]), "99"; got != want {
		t.Errorf("incident_id = %v, want %s", raw["incident_id"], want)
	}
	if raw["confidence"] != "High" {
		t.Errorf("confidence = %v, want High", raw["confidence"])
	}
}

// LogRagEvent must omit incident_id when nil, otherwise non-diagnostic Steps
// would write incident_id=null which is FK-valid but noisy. omitempty does
// this, but only because the field is *int64 — if anyone retypes it back to
// int64 this test catches it.
func TestLogRagEvent_OmitsNilIncidentID(t *testing.T) {
	url, recv := stubServer(t, func(captured) (int, string) { return 201, `[{"id":1}]` })
	wireSupabase(t, url)

	_, err := LogRagEvent(RagEvent{
		SessionID:         "s",
		StepIndex:         0,
		ClusterID:         "x",
		UserQueryRedacted: "hello",
		ToolSequence:      []string{},
		StepKind:          "chat",
		// IncidentID intentionally left nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs := recv()
	if len(reqs) != 1 {
		t.Fatal("expected one request")
	}
	if strings.Contains(string(reqs[0].Body), "incident_id") {
		t.Errorf("incident_id should be omitted when nil, but body contains it: %s", reqs[0].Body)
	}
}

// A non-2xx response should surface as an error so the caller doesn't store
// gotID=0 alongside lastRagEventID and silently break the followup patches.
func TestLogRagEvent_ServerErrorReturnsError(t *testing.T) {
	url, _ := stubServer(t, func(captured) (int, string) {
		return 500, `{"message":"boom"}`
	})
	wireSupabase(t, url)

	id, err := LogRagEvent(RagEvent{SessionID: "s", StepKind: "chat", ToolSequence: []string{}})
	if err == nil {
		t.Error("expected error on 500 response, got nil")
	}
	if id != 0 {
		t.Errorf("id = %d on server error, want 0", id)
	}
}

// ---------------------------------------------------------------------------
// PatchFeedback / PatchFollowupGap: PATCH to ?id=eq.<id> with the right body.
// ---------------------------------------------------------------------------

func TestPatchFeedbackWireShape(t *testing.T) {
	url, recv := stubServer(t, func(captured) (int, string) { return 204, "" })
	wireSupabase(t, url)

	PatchFeedback(4242, -1)

	reqs := recv()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", r.Method)
	}
	if r.Path != "/rest/v1/rag_events" {
		t.Errorf("path = %s, want /rest/v1/rag_events", r.Path)
	}
	// PostgREST filter — id=eq.4242 is what scopes the PATCH to one row.
	if r.Query != "id=eq.4242" {
		t.Errorf("query = %q, want id=eq.4242", r.Query)
	}
	mustContain(t, r.Header, "Prefer", "return=minimal")

	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	// JSON numbers decode as float64.
	if body["feedback"] != float64(-1) {
		t.Errorf("feedback = %v, want -1", body["feedback"])
	}
}

func TestPatchFollowupGapWireShape(t *testing.T) {
	url, recv := stubServer(t, func(captured) (int, string) { return 204, "" })
	wireSupabase(t, url)

	PatchFollowupGap(7, 42)

	reqs := recv()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPatch || r.Path != "/rest/v1/rag_events" || r.Query != "id=eq.7" {
		t.Errorf("got %s %s?%s, want PATCH /rest/v1/rag_events?id=eq.7", r.Method, r.Path, r.Query)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["followup_within_sec"] != float64(42) {
		t.Errorf("followup_within_sec = %v, want 42", body["followup_within_sec"])
	}
}

// id=0 means "no prior rag_events row was written" (telemetry disabled at the
// time, or the insert failed). Patches must short-circuit so we don't issue
// PATCH ?id=eq.0 which would silently match nothing or, worse, fan out.
func TestPatchRagEvent_ZeroIDIsNoop(t *testing.T) {
	url, recv := stubServer(t, nil)
	wireSupabase(t, url)

	PatchFeedback(0, +1)
	PatchFollowupGap(0, 5)

	if got := len(recv()); got != 0 {
		t.Errorf("expected zero PATCHes for id=0, got %d", got)
	}
	_ = url
}

// ---------------------------------------------------------------------------
// postIncident: spot-check that the incidents writer still hits the right
// path with return=minimal. Regression guard for the existing audit invariant.
// ---------------------------------------------------------------------------

func TestLogWriteActionWireShape(t *testing.T) {
	url, recv := stubServer(t, func(captured) (int, string) { return 201, "" })
	wireSupabase(t, url)

	LogWriteAction("restart_deployment", "confirmed_write",
		WriteSignals{Action: "restart_deployment", UserConfirmed: true, PreconditionMet: true},
		"rollout restart issued", "https://example-cluster:6443")

	reqs := recv()
	if len(reqs) != 1 {
		t.Fatalf("want 1 request, got %d", len(reqs))
	}
	r := reqs[0]
	if r.Method != http.MethodPost || r.Path != "/rest/v1/incidents" {
		t.Errorf("got %s %s, want POST /rest/v1/incidents", r.Method, r.Path)
	}
	mustContain(t, r.Header, "Prefer", "return=minimal")

	var log IncidentLog
	if err := json.Unmarshal(r.Body, &log); err != nil {
		t.Fatalf("body not IncidentLog JSON: %v", err)
	}
	if log.IncidentType != "confirmed_write" {
		t.Errorf("incident_type = %q, want confirmed_write", log.IncidentType)
	}
	// cluster_id must be the SHA-256 hash, never the raw URL.
	if strings.Contains(log.ClusterID, "example-cluster") || strings.Contains(log.ClusterID, "6443") {
		t.Errorf("cluster_id leaked raw URL: %q", log.ClusterID)
	}
	if len(log.ClusterID) != 16 {
		t.Errorf("cluster_id len = %d, want 16 (8-byte hex)", len(log.ClusterID))
	}
}

// ---------------------------------------------------------------------------
// extractConfidence: ensure the parser still recognizes the format the
// system prompt asks the model to emit. Cheap unit test, no HTTP.
// ---------------------------------------------------------------------------

func TestExtractConfidence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"high", "## Confidence\nHigh — readiness probe matches logs", "High"},
		{"medium", "## Confidence\nMedium — partial evidence", "Medium"},
		{"low", "## Confidence\nLow — insufficient data", "Low"},
		{"emoji header", "## 📊 Confidence\nHigh — clear signal", "High"},
		{"missing", "no confidence line here", ""},
		{"header but no following line", "## Confidence", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractConfidence(c.in); got != c.want {
				t.Errorf("extractConfidence(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// truncateRunes must keep results within the incidents RLS length bounds so a
// verbose diagnosis is trimmed, never dropped. Counts characters, not bytes.
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello", 8000); got != "hello" {
		t.Errorf("short string changed: %q", got)
	}
	if got := truncateRunes(strings.Repeat("x", 8000), 8000); got != strings.Repeat("x", 8000) {
		t.Errorf("exact-length string should be untouched")
	}
	// Over the cap: result is exactly max runes, ending in the ellipsis.
	got := truncateRunes(strings.Repeat("x", 9000), 8000)
	if n := len([]rune(got)); n != 8000 {
		t.Errorf("rune length = %d, want 8000", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncated string should end with ellipsis, got %q", got[len(got)-4:])
	}
	// Multi-byte runes count as one character each (matches Postgres length()).
	if got := truncateRunes(strings.Repeat("é", 200), 100); len([]rune(got)) != 100 {
		t.Errorf("multibyte rune length = %d, want 100", len([]rune(got)))
	}
}

package telemetry

import (
	"strings"
	"testing"
)

func TestRedact_PodsAreTemplated(t *testing.T) {
	in := "pod payments-7d9fb-x4k2p is in CrashLoopBackOff"
	out, stats := Redact(in)
	if strings.Contains(out, "payments-7d9fb-x4k2p") {
		t.Errorf("raw pod name leaked: %q", out)
	}
	if !strings.Contains(out, "<POD_1>") {
		t.Errorf("expected <POD_1> placeholder, got %q", out)
	}
	if stats.Pods != 1 {
		t.Errorf("Pods count = %d, want 1", stats.Pods)
	}
}

func TestRedact_CoreferenceWithinCall(t *testing.T) {
	in := "pod payments-7d9fb-x4k2p crashed; restart payments-7d9fb-x4k2p next"
	out, _ := Redact(in)
	if strings.Count(out, "<POD_1>") != 2 {
		t.Errorf("expected <POD_1> twice (coreference), got %q", out)
	}
	if strings.Contains(out, "<POD_2>") {
		t.Errorf("same name should map to same placeholder, got %q", out)
	}
}

func TestRedact_DistinctPodsGetDistinctTags(t *testing.T) {
	in := "pods aaa-bbb-ccccc and ddd-eee-fffff both failed"
	out, stats := Redact(in)
	if !strings.Contains(out, "<POD_1>") || !strings.Contains(out, "<POD_2>") {
		t.Errorf("expected two distinct POD tags, got %q", out)
	}
	if stats.Pods != 2 {
		t.Errorf("Pods count = %d, want 2", stats.Pods)
	}
}

func TestRedact_ImagesIncludingShaDigest(t *testing.T) {
	cases := []string{
		"image gcr.io/myproj/api:v1.2.3 failed to pull",
		"image docker.io/library/nginx:1.21.6 failed to pull",
		"image quay.io/coreos/etcd@sha256:" + strings.Repeat("a", 64) + " failed",
	}
	for _, in := range cases {
		out, stats := Redact(in)
		if strings.Contains(out, "gcr.io") || strings.Contains(out, "docker.io") || strings.Contains(out, "quay.io") {
			t.Errorf("image registry leaked in %q -> %q", in, out)
		}
		if !strings.Contains(out, "<IMAGE_1>") {
			t.Errorf("expected <IMAGE_1>, got %q", out)
		}
		if stats.Images < 1 {
			t.Errorf("Images count = %d, want >=1 for %q", stats.Images, in)
		}
	}
}

func TestRedact_URLsHTTPandHTTPS(t *testing.T) {
	in := "see https://prod-east.cluster.local:6443/api/v1 and http://10.0.0.1:8080/healthz"
	out, stats := Redact(in)
	if strings.Contains(out, "prod-east.cluster.local") {
		t.Errorf("hostname leaked: %q", out)
	}
	if !strings.Contains(out, "<URL_1>") || !strings.Contains(out, "<URL_2>") {
		t.Errorf("expected two URL placeholders, got %q", out)
	}
	if stats.URLs != 2 {
		t.Errorf("URLs count = %d, want 2", stats.URLs)
	}
}

func TestRedact_IPv4andIPv6(t *testing.T) {
	in := "client 192.168.1.42 connected; server ::1 and 2001:db8::1 reachable"
	out, stats := Redact(in)
	if strings.Contains(out, "192.168.1.42") {
		t.Errorf("IPv4 leaked: %q", out)
	}
	if strings.Contains(out, "2001:db8") {
		t.Errorf("IPv6 leaked: %q", out)
	}
	if stats.IPs < 2 {
		t.Errorf("IPs count = %d, want >=2", stats.IPs)
	}
}

func TestRedact_UUIDs(t *testing.T) {
	in := "uid 550e8400-e29b-41d4-a716-446655440000 owned this pod"
	out, stats := Redact(in)
	if strings.Contains(out, "550e8400") {
		t.Errorf("UUID leaked: %q", out)
	}
	if !strings.Contains(out, "<UUID_1>") {
		t.Errorf("expected <UUID_1>, got %q", out)
	}
	if stats.UUIDs != 1 {
		t.Errorf("UUIDs count = %d, want 1", stats.UUIDs)
	}
}

func TestRedact_NamespaceKeywordAndFlag(t *testing.T) {
	in := "kubectl get pods -n production; check namespace kube-system too"
	out, stats := Redact(in)
	if strings.Contains(out, "production") || strings.Contains(out, "kube-system") {
		t.Errorf("namespace leaked: %q", out)
	}
	if !strings.Contains(out, "<NS_") {
		t.Errorf("expected <NS_*> placeholder, got %q", out)
	}
	if stats.Namespaces < 2 {
		t.Errorf("Namespaces count = %d, want >=2", stats.Namespaces)
	}
}

func TestRedact_EmptyAndPlainText(t *testing.T) {
	out, stats := Redact("")
	if out != "" {
		t.Errorf("empty in, got %q", out)
	}
	if (stats != RedactionStats{}) {
		t.Errorf("empty in, got nonzero stats %+v", stats)
	}

	out, _ = Redact("what does CrashLoopBackOff mean?")
	if out != "what does CrashLoopBackOff mean?" {
		t.Errorf("plain text mutated: %q", out)
	}
}

func TestRedact_DoesNotDoubleTemplatePlaceholders(t *testing.T) {
	// If Redact runs twice on the same string the placeholders must survive.
	once, _ := Redact("pod payments-7d9fb-x4k2p crashed")
	twice, _ := Redact(once)
	if once != twice {
		t.Errorf("double-redact diverged: once=%q twice=%q", once, twice)
	}
}

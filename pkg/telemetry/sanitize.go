package telemetry

// Redaction pipeline for free-text fields that flow to Supabase
// (user_query_redacted and diagnosis_redacted in the rag_events table).
//
// Sanitization invariant for rag_events: no raw pod / namespace /
// deployment / image / IP / URL / UUID strings may be sent. The
// `incidents` table is read-only structured fields and is enforced by
// the grep on logger.go; this file is the enforcement for free text.
//
// Strategy: ordered regex pipeline, most-specific first so a broader
// pattern never half-eats a narrower one. A per-call coreference map
// keeps the same identifier mapped to the same placeholder inside one
// string (so "<POD_1>" appears repeatedly when the same pod is
// referenced), preserving meaning for downstream RAG without leaking
// the real name.
//
// We deliberately over-redact: a false positive (e.g. a benign hyphenated
// noun gets templated to <POD_1>) is harmless for the corpus; a false
// negative (a real pod name leaks) breaks the invariant.

import (
	"fmt"
	"regexp"
)

// RedactionStats records how many of each kind of identifier were
// templated. Stored as `redaction_stats` jsonb for QC dashboards.
type RedactionStats struct {
	Pods       int `json:"pods"`
	Namespaces int `json:"namespaces"`
	Images     int `json:"images"`
	IPs        int `json:"ips"`
	URLs       int `json:"urls"`
	UUIDs      int `json:"uuids"`
}

// Order matters. Image/URL/IP/UUID first so their slashes / colons /
// hyphens are gone before the pod pattern looks for hyphen-separated
// tokens. Namespace tokens last because they ride on context words.
var (
	// Image refs require at least one slash before the colon so we don't
	// eat URLs (which would already be replaced by URL_N) or bare host:port.
	// Examples: gcr.io/proj/svc:v1.2, docker.io/library/nginx@sha256:abc...
	reImage = regexp.MustCompile(`\b[a-z0-9][a-z0-9._-]*(?:/[a-z0-9._-]+)+(?::[a-zA-Z0-9._-]+)?(?:@sha256:[a-f0-9]{64})?\b`)

	reURL = regexp.MustCompile(`\bhttps?://[^\s)>"']+`)

	reIPv4 = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// IPv6 (loose): two-or-more colon-separated hex groups. Empty groups
	// are allowed so the compressed forms `::1` and `2001:db8::1` match.
	// No `\b` — `:` is not a word character so the boundary anchor would
	// disallow leading-colon forms like `::1`.
	reIPv6 = regexp.MustCompile(`(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}`)

	reUUID = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)

	// Pod-shaped names: lowercase token with at least two hyphens
	// (deployment-replicaset-pod shape). Catches the controller-generated
	// suffix grammar plus most user-named pods.
	rePod = regexp.MustCompile(`\b[a-z][a-z0-9]*(?:-[a-z0-9]+){2,}\b`)

	// Namespace tokens: only after the explicit "namespace" keyword or the
	// "-n" kubectl flag. "in production" is too ambiguous to match safely.
	reNamespaceWord = regexp.MustCompile(`(?i)\bnamespace\s+([a-z0-9][a-z0-9-]*)`)
	reNamespaceFlag = regexp.MustCompile(`(?i)(^|\s)-n\s+([a-z0-9][a-z0-9-]*)`)
)

// Redact templates cluster identifiers in s. Returns the redacted text
// and per-category counts. Safe to call with an empty string.
func Redact(s string) (string, RedactionStats) {
	if s == "" {
		return "", RedactionStats{}
	}
	var stats RedactionStats
	r := newReplacer()

	s = reURL.ReplaceAllStringFunc(s, func(m string) string {
		stats.URLs++
		return r.tag("URL", m)
	})
	s = reImage.ReplaceAllStringFunc(s, func(m string) string {
		// Image rule requires a slash; otherwise leave it (probably not an image).
		// The regex already encodes that; the extra guard is defense in depth.
		stats.Images++
		return r.tag("IMAGE", m)
	})
	s = reUUID.ReplaceAllStringFunc(s, func(m string) string {
		stats.UUIDs++
		return r.tag("UUID", m)
	})
	s = reIPv6.ReplaceAllStringFunc(s, func(m string) string {
		stats.IPs++
		return r.tag("IP", m)
	})
	s = reIPv4.ReplaceAllStringFunc(s, func(m string) string {
		stats.IPs++
		return r.tag("IP", m)
	})

	s = reNamespaceWord.ReplaceAllStringFunc(s, func(m string) string {
		sub := reNamespaceWord.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		stats.Namespaces++
		return "namespace " + r.tag("NS", sub[1])
	})
	s = reNamespaceFlag.ReplaceAllStringFunc(s, func(m string) string {
		sub := reNamespaceFlag.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		stats.Namespaces++
		return sub[1] + "-n " + r.tag("NS", sub[2])
	})

	s = rePod.ReplaceAllStringFunc(s, func(m string) string {
		// Don't re-template our own placeholders (already-redacted UUID, etc.).
		if isPlaceholder(m) {
			return m
		}
		stats.Pods++
		return r.tag("POD", m)
	})

	return s, stats
}

// replacer assigns stable POD_1 / IMAGE_1 / ... numbers within one call.
type replacer struct {
	seen   map[string]string // raw -> placeholder
	counts map[string]int    // tag -> next number
}

func newReplacer() *replacer {
	return &replacer{seen: map[string]string{}, counts: map[string]int{}}
}

func (r *replacer) tag(kind, raw string) string {
	key := kind + "\x00" + raw
	if p, ok := r.seen[key]; ok {
		return p
	}
	r.counts[kind]++
	p := fmt.Sprintf("<%s_%d>", kind, r.counts[kind])
	r.seen[key] = p
	return p
}

// isPlaceholder reports whether a token already looks like one of our
// templates (<TAG_N>). Used to avoid double-templating.
func isPlaceholder(s string) bool {
	if len(s) < 5 || s[0] != '<' || s[len(s)-1] != '>' {
		return false
	}
	return true
}

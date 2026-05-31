#!/usr/bin/env bash
# review-checks.sh — mechanical invariant checks for the zanecli code-review skill.
#
# Run from the zanecli repo root:
#     bash .claude/skills/zanecli-review/scripts/review-checks.sh
#
# Each check prints PASS or FAIL (with offending lines). The script runs ALL
# checks even when one fails, then exits non-zero if any failed. The grep guards
# below are copied verbatim from .github/workflows/ci.yml so this never drifts
# from CI.

set -uo pipefail

failures=0

# pass <name>            -> green PASS line
# fail <name> [details]  -> red FAIL line + optional indented detail block
pass() { printf '  [PASS] %s\n' "$1"; }
fail() {
  printf '  [FAIL] %s\n' "$1"
  if [[ -n "${2:-}" ]]; then
    printf '%s\n' "$2" | sed 's/^/         /'
  fi
  failures=$((failures + 1))
}

# zero_match <name> <output>  -> PASS when output is empty, else FAIL w/ matches
zero_match() {
  if [[ -z "$2" ]]; then pass "$1"; else fail "$1" "$2"; fi
}

if [[ ! -f go.mod ]] || [[ ! -d pkg/telemetry ]]; then
  echo "ERROR: run this from the zanecli repo root (go.mod + pkg/telemetry not found)." >&2
  exit 2
fi

echo "== Security invariants =="

# Guard 1: telemetry logger must never read formatted-string identifier fields.
m=$(grep -nE 'data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)' pkg/telemetry/logger.go || true)
zero_match "Telemetry sanitization (logger.go reads only structured side-fields)" "$m"

# Guard 2: every redacted-field assignment must come from a redacted* local.
m=$(grep -nE '(UserQueryRedacted|DiagnosisRedacted):' pkg/agent/agent.go | grep -v 'redacted\(Query\|Diagnosis\)' || true)
zero_match "RAG redaction (agent.go uses redactedQuery/redactedDiagnosis locals)" "$m"

echo
echo "== Conventions =="

# ForTesting helpers may be DEFINED in production code (exported so cross-package
# tests can call them) but must never be CALLED from production paths. So flag
# ForTesting in non-test .go files only after dropping comment lines and the func
# declaration itself — what remains is a production call site.
m=$(grep -rnE 'ForTesting' --include='*.go' . \
      | grep -v '_test\.go:' \
      | grep -vE ':[0-9]+:[[:space:]]*//' \
      | grep -vE ':[0-9]+:func ' || true)
zero_match "Test-only helpers (ForTesting called only from *_test.go)" "$m"

# Banned dependencies: LLM frameworks/SDKs and web frameworks.
m=$(grep -nEi 'langchain|llamaindex|anthropic-sdk|github\.com/anthropics|gin-gonic|go-chi/chi|labstack/echo|gofiber/fiber' go.mod go.sum 2>/dev/null || true)
zero_match "No banned deps (LLM frameworks/SDKs, web frameworks)" "$m"

echo
echo "== Build / vet / format / test =="

if go build ./... 2>build.err; then pass "go build ./..."; else fail "go build ./..." "$(cat build.err)"; fi
rm -f build.err

if out=$(go vet ./... 2>&1); then pass "go vet ./..."; else fail "go vet ./..." "$out"; fi

m=$(gofmt -l . || true)
zero_match "gofmt (no formatting drift)" "$m"

if out=$(go test ./... -race -count=1 -timeout=120s 2>&1); then
  pass "go test ./... -race"
else
  fail "go test ./... -race" "$out"
fi

echo
if [[ $failures -eq 0 ]]; then
  echo "ALL CHECKS PASSED"
  exit 0
else
  echo "$failures CHECK(S) FAILED"
  exit 1
fi

---
name: zanecli-review
description: Review zanecli (the conversational Kubernetes co-pilot CLI) code against its telemetry-sanitization, RAG-redaction, fail-closed safety, and tool conventions. Use when reviewing a diff, PR, or the whole codebase for this repo.
---

# zanecli code review

A repeatable reviewer for the `zanecli` repo. It encodes the project's strict,
security-critical invariants (telemetry sanitization, RAG redaction, fail-closed
safety) and its code conventions, then checks code against them. Use it to review
a diff/PR or to sweep the whole codebase.

Each rule is tagged **[auto]** (verified by the bundled script) or **[judgment]**
(you must read the code and reason). The script catches regressions mechanically;
your job is the rest.

## How to run

1. **Run the mechanical checks first**, from the repo root:
   ```bash
   bash .claude/skills/zanecli-review/scripts/review-checks.sh
   ```
   It must be run with the repo as the working directory (it greps repo-relative
   paths). Triage any `[FAIL]` before doing anything else — the two security
   guards are blockers by definition.

2. **Pick the review scope.** For a PR/diff: `git diff main...HEAD --stat` and
   review the changed `.go` files. For a full sweep: review all of `pkg/` plus
   `main.go`.

3. **Walk each changed file through the checklist below.** The script proves the
   `[auto]` invariants hold *globally*; you still apply the `[judgment]` rules to
   the specific code under review.

4. **Report findings** in the output format at the bottom.

## Checklist

### 1. Telemetry sanitization — `pkg/telemetry/logger.go` [auto + judgment]

`logger.go` may read **only** the structured side-fields on the `k8s` diagnostic
structs (`EventReasons`, `ReplicaCounts`, `SchedulerReason`, `WorstPodContainers`,
container-state strings). It must **never** read the formatted-string identifier
fields (`Events`, `PodSpec`, `PodSummary`, `DeploymentName`, `PodName`,
`Namespace`, `DeploymentSpec`, …) — those carry cluster identifiers.

- The one allowed exception is `data.WorstPodLogs → log_tail` (crash + rollout paths).
- The script runs the exact CI grep. **Additionally, by judgment:** if a change
  adds a *new* side-field and reads it in `logger.go`, confirm that field cannot
  contain a pod/namespace/image/secret name. The grep can't catch a newly-named
  leaky field.
- The `incidents` row must never contain pod/namespace/deployment names, env var
  values, image strings, secret names, or real cluster URLs (only the SHA-256
  `cluster_id`).

### 2. RAG redaction — `pkg/agent/agent.go` + `pkg/telemetry/sanitize.go` [auto + judgment]

Every assignment to `UserQueryRedacted` or `DiagnosisRedacted` must use a local
named exactly `redactedQuery` / `redactedDiagnosis`, populated immediately above
the struct literal from `telemetry.Redact(...)`.

- The script runs the CI grep (any non-`redacted*` assignment = bypass = blocker).
- Tool **inputs** are never logged; only tool **names** go into
  `rag_events.tool_sequence`. Flag any code that puts tool input/args into
  telemetry.
- Over-redaction is acceptable; under-redaction breaks the invariant. If a change
  touches `Redact()`, check it still templates pods/namespaces/images/IPs/URLs/UUIDs.

### 3. Fail-closed safety — `pkg/safety` + `main.go` [judgment]

- All cluster **writes** (`restart_deployment`, `delete_pod`) must route through
  `pkg/safety.Guard.Evaluate`. Any direct write that bypasses the guard is a blocker.
- Fail-closed: any uncertainty must resolve to `Confirm`, never silent `AutoExec`.
- **Do NOT flag these as bugs — they are intentional** (see CLAUDE.md "Auto-exec
  reality"): `main.go` hard-setting `cfg.AutoExec = false` on every launch; the
  dormant `--auto`/`--no-auto`/`/auto` machinery; the dropped namespace-regex
  guard. Don't "fix" the forced-off line into an enablement path. Trust control
  flow over stale comments.

### 4. Tool conventions — `pkg/tools` + `pkg/agent/agent.go` [judgment]

- Tool failures are returned as a **string result** (`return "error: ...", nil`),
  not a Go error. Go errors are reserved for protocol faults (unknown tool name).
  Flag a tool whose `Run` returns a non-nil Go error for an ordinary failure.
- `get_resource` and any generic accessor must issue **Get/List only** — never
  Patch/Delete/Create — and must keep Secret-value redaction (`pkg/k8s/generic.go`).
- Adding/removing a tool must update **both** `NewRegistry` (`pkg/tools/tools.go`)
  **and** `systemPrompt()` (`pkg/agent/agent.go`) — the prompt's tool inventory and
  per-kind playbooks must match the registry. Check both when a tool changes.

### 5. Code style [auto + judgment]

- Errors wrapped with `%w` and returned up to `main.go` for printing (no panics
  for ordinary errors).
- `context.Context` threaded through every function that does I/O.
- No package-level mutable global state. The `sync.Once`-cached dynamic client in
  `pkg/k8s/generic.go` is the **only** sanctioned exception.
- Comments explain the **WHY**, not the WHAT.
- `ForTesting`-suffixed helpers appear **only** in `*_test.go` (script greps for
  leaks into production code).

### 6. "Do not" list [auto] (CLAUDE.md)

No web server, database, or persistence beyond the JSONL history file; no auth /
multi-tenancy; **no LLM frameworks or vendor SDKs** (LangChain, LlamaIndex,
Anthropic SDK — direct HTTP to `/v1/messages` only); no subcommands and no
bare-arg one-shot mode (`--prompt` is the only sanctioned future non-interactive
path). The script greps `go.mod`/`go.sum` for banned deps; also eyeball new
`import` blocks for web/LLM frameworks.

### 7. Tests [judgment]

- Tests live in `*_test.go` next to the source they cover.
- New tests needing a cluster use `k8s.NewClientFromInterface(...)` with a
  `fake.Clientset`; tests stubbing Anthropic use `ai.SetAPIURLForTesting(...)`
  against an `httptest.Server`.
- A behavior change should come with a test; flag write/safety/telemetry changes
  that ship without one.

## Output format

Report findings grouped by severity:

- **Blockers** — security-invariant violations (telemetry/RAG/safety), broken
  build/tests, anything from the "Do not" list.
- **Should-fix** — convention violations (error handling, missing context,
  registry/prompt drift, missing tests).
- **Nits** — style, naming, comment quality.

Each finding cites `file:line` and the rule it violates. Explicitly state which
invariant sections **passed** (e.g. "Telemetry sanitization: PASS — guard clean,
no new side-fields"). End with the script's summary line (`ALL CHECKS PASSED` or
the failure count).

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`zane` — a conversational Kubernetes co-pilot. The user runs `zane` and gets a chat REPL: ask a question in plain English, the agent investigates the cluster via Anthropic tool use, cites evidence, and proposes fixes. A tightly-scoped set of writes (`restart_deployment`, `delete_pod`) can be executed, but every write is confirmation-gated in the current build (see "Auto-exec reality" below).

All six original implementation phases (rename, wizard, agent loop, write actions, history, polish) are shipped and in `pkg/`. This is no longer a one-shot CLI — the old `kubectl-ai` `cmd/` + Cobra layer is gone; there are no subcommands.

## Build, run, check

```bash
go build -o zane .                 # build
./zane                             # run (first launch triggers the config wizard)
go vet ./...                       # vet
go test ./... -race -count=1       # unit tests (full suite ~3s)
go mod tidy                        # after touching imports
```

Tests live in `*_test.go` next to the source they cover (`pkg/safety/guards_test.go`, `pkg/k8s/generic_test.go`, `pkg/telemetry/sanitize_test.go`, `pkg/telemetry/logger_test.go`, `pkg/tools/tools_test.go`, `pkg/ai/claude_test.go`, `pkg/history/history_test.go`, `pkg/config/config_test.go`, `pkg/agent/agent_test.go`). Two pieces of test-only infrastructure are worth knowing about before extending them:

- `pkg/k8s/client.go` exposes `NewClientFromInterface(kubernetes.Interface, serverURL)` so tests can inject `k8s.io/client-go/kubernetes/fake.Clientset`. The production `Client.clientset` field is typed as the interface (not `*kubernetes.Clientset`) to make this work transparently.
- `pkg/ai/claude.go` exposes `SetAPIURLForTesting(url)` to redirect `claudeAPIURL` at an `httptest.Server` from cross-package tests (notably `pkg/agent`). Suffix `ForTesting` is intentional — it should never appear outside `*_test.go`.

CI runs the suite plus two grep-based invariant guards (see [Telemetry sanitization invariant](#telemetry-sanitization-invariant-do-not-break) below) via `.github/workflows/ci.yml`.

`testdata/` still holds manual smoke targets (`crashloop-pod.yaml`, `stuck-rollout.yaml`) you apply to a real cluster and then drive the agent against; they are not automated tests.

Production build with telemetry baked in (manual; the release flow below does this automatically):
```bash
GOOS=linux GOARCH=amd64 go build -ldflags "\
  -X main.ClientVersion=v0.1.0 \
  -X github.com/zarakM/zane/pkg/telemetry.supabaseURL=https://yourproject.supabase.co \
  -X github.com/zarakM/zane/pkg/telemetry.supabaseKey=your-anon-key" \
  -o zane-linux .
```
Credential precedence (highest → lowest): `SUPABASE_URL`/`SUPABASE_KEY` env vars > `~/.zane/config.json` (passed in via `telemetry.SetSupabaseConfig`) > ldflags-baked defaults. Same env-wins precedence for `ANTHROPIC_API_KEY` / `KUBECONFIG` over the config file.

### Releases

Cutting a release is one tag + push:
```bash
git tag v0.1.0 && git push origin v0.1.0
```
`.github/workflows/release.yml` triggers on any `v*` tag and invokes GoReleaser per `.goreleaser.yaml`. GoReleaser cross-compiles for Linux/macOS/Windows (amd64 + arm64; Windows-arm64 skipped), bundles each binary with `LICENSE` and `README.md`, generates `checksums.txt`, and attaches everything to a GitHub Release. Pre-release tags (`v0.1.0-rc.1`) are marked as pre-release automatically.

The `brews:` block also generates `Formula/zane.rb` and pushes it to the `github.com/zarakM/homebrew-tap` repo (install: `brew install zarakM/tap/zane`). The binary/command is `zane`; the Go module/repo stays `github.com/zarakM/zane`. This depends on two pieces of GitHub-side setup that are NOT in the repo: the `homebrew-tap` repo must exist, and a `HOMEBREW_TAP_GITHUB_TOKEN` secret (PAT with write access to the tap) must be set — the default `GITHUB_TOKEN` can't push to another repo. `skip_upload: auto` means pre-release tags do not update the tap; only final tags publish a formula.

ldflag injection at release time is GoReleaser's responsibility — `.goreleaser.yaml` reads `{{.Version}}` for `main.ClientVersion`, plus `SUPABASE_URL` / `SUPABASE_KEY` from repo secrets (silently empty if unset). **`go install github.com/zarakM/zane@v0.1.0` does NOT apply these ldflags** — it builds from source, so `ClientVersion` stays `dev` and any Supabase creds you'd baked in via secrets are absent. Production users should download the release archive; `go install` is the developer / contributor path.

## Repo skills (`.claude/skills/`)

Two project-scoped Claude Code skills encode repeatable workflows so they run the
same way every time instead of being re-derived per session:

- **`zane-review`** — reviews a diff/PR/the whole codebase against the project
  invariants (telemetry sanitization, RAG redaction, fail-closed safety, tool
  conventions). Each rule is tagged `[auto]` (verified by the bundled
  `scripts/review-checks.sh`) or `[judgment]`. Run it before opening or merging
  anything that touches `pkg/telemetry`, `pkg/agent`, `pkg/safety`, or `pkg/tools`.
- **`open-pr`** — the PR-creation ritual: run the pre-PR gates (tests, `gofmt`,
  `go vet`, `go mod tidy`, the two invariant greps), branch off `main`, commit
  with the `Co-Authored-By` trailer, push (the `.githooks` pre-push guard runs),
  and open the PR with the house `## What` body format. It composes with
  `zane-review` rather than duplicating its checks.

## Architecture

Request flow: `main.go` REPL loop → `agent.Session.Step` (multi-turn Anthropic tool-use loop) → `tools.Registry` dispatch → `pkg/k8s` → results fed back to the model until it stops calling tools and emits final text.

```
main.go              REPL: config wizard, ⌃C handling, history persistence,
                     stdin-backed write confirmer. Builtins: exit/quit, /clear.
pkg/config           First-run wizard + ~/.zane/config.json (mode 0600).
pkg/agent            Session: owns message history, runs the tool-use loop,
                     streams text + [bracketed tool-status] lines, drains the
                     telemetry buffer at end_turn.
pkg/tools            One Tool per file; Registry.NewRegistry wires them all.
                     Reads always run; writes are routed through pkg/safety.
pkg/safety           Guard.Evaluate → AutoExec / Confirm / Refuse. Fail-closed.
pkg/k8s              Cluster access. Typed gather/format helpers (REUSED from
                     the kubectl-ai era, unchanged) + generic.go (dynamic
                     client + discovery RESTMapper, backs get_resource).
pkg/ai               Direct HTTP to Anthropic /v1/messages. No SDK, no LLM
                     framework. streamTo (SSE), complete (one-shot), and the
                     AgentRequest tool-use content-block types.
pkg/telemetry        Silent background Supabase logging. Sanitization invariant.
pkg/history          Opt-in JSONL session log + resume-at-launch.
pkg/ui               ANSI color constants.
```

### The agent loop (pkg/agent)

`Session.Step` is the unit of work: one user input → final answer, internally taking as many Anthropic round-trips as the model needs. `Session` holds `[]ai.AgentMessage`; `Clear`/`LoadMessages`/`Messages` exist for `/clear` and `pkg/history` resume. `Session` implements `tools.DiagnosticSink` — `diagnose_pod`/`diagnose_rollout` push structured payloads into a per-Step buffer via `RecordCrash/Pending/Rollout`; telemetry fires once at `end_turn`, using the agent's final text as the `diagnosis` field (not per tool call). Preserve this end-of-Step attribution if you touch telemetry wiring.

### Tools (pkg/tools)

Every tool implements `Tool` (`Name/Description/InputSchema/Run`) and is registered in `NewRegistry`. Tool errors are returned as the *string result* (so the model can adjust and retry), not as Go errors — Go errors are reserved for protocol problems like an unknown tool name. Read tools: `list_namespaces/pods/deployments`, `describe_pod/deployment`, `get_pod_logs`, `get_events`, `diagnose_pod`, `diagnose_rollout`, `list_pvcs`, `list_storageclasses`, and `get_resource` (the catch-all dynamic reader for any kind without a dedicated tool — StatefulSet, DaemonSet, Job, PV, Service, HPA, Node, CRDs; returns sanitized YAML, Secret values redacted in `pkg/k8s/generic.go`). Write tools: `restart_deployment`, `delete_pod`.

The agent's persona and operating rules live in `systemPrompt()` at the bottom of `pkg/agent/agent.go`. It must only reference tools actually registered in `NewRegistry`. When adding/removing a tool, update both the registry and the prompt's tool inventory and per-kind playbooks.

### Auto-exec reality (read before touching writes)

The codebase contains a full three-guard auto-exec design in `pkg/safety` (opt-in `cfg.AutoExec` → whitelist → live state precondition → per-session quota of `MaxAutoExecsPerSession`). **But `main.go` hard-sets `cfg.AutoExec = false` on every launch**, so `Guard.Evaluate` always returns `Confirm` for writes — every cluster mutation prompts `y/N` via the stdin `Confirmer`. The README and several code comments describe `--auto`/`--no-auto` CLI flags, `/auto`/`/no-auto` slash commands, and a production-namespace regex guard: **none of these are wired** (no flag parsing in `main.go`, only `exit`/`quit`/`/clear` builtins, the namespace-regex guard was dropped). Treat the safety machinery as present-but-dormant; do not "fix" the forced-off line into an enablement path without an explicit request, and don't trust comments over the actual control flow.

### Telemetry sanitization invariant (do not break)

`pkg/telemetry/logger.go` may read **only** the structured side-fields on the `k8s` diagnostic structs (`EventReasons`, `ReplicaCounts`, `SchedulerReason`, `WorstPodContainers`, etc.), never the formatted-string fields (`Events`, `PodSpec`, `PodSummary`, `DeploymentName`, `PodName`, `Namespace`, …) which carry cluster identifiers. The single allowed exception is `data.WorstPodLogs` → `log_tail` on the crash and rollout paths. This is enforced by a reviewable grep that must return **zero matches**:

```
grep -nE 'data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)' pkg/telemetry/logger.go
```

Telemetry row (`incidents` table): `incident_type`, `error_type`, `signals` (jsonb, schema-flexible per type), `diagnosis`, `confidence`, `cluster_id` (SHA-256 of server URL, first 8 bytes), `model`, `created_at`. Never store pod/namespace/deployment names, env var values, image strings, secret names, or real cluster URLs.

### RAG capture: `sessions` + `rag_events` (migration 0002)

A parallel capture path stores per-Step rich data for the future RAG corpus. The free-text fields (`user_query_redacted`, `diagnosis_redacted`) MUST be passed through `telemetry.Redact()` (`pkg/telemetry/sanitize.go`) before leaving the process — that's the enforcement for the rag_events table, equivalent to the structured-side-fields rule for `incidents`. The redactor templates pod names → `<POD_N>`, namespaces → `<NS_N>`, images → `<IMAGE_N>`, IPs → `<IP_N>`, URLs → `<URL_N>`, UUIDs → `<UUID_N>` with stable coreference inside one string. Over-redaction is acceptable; under-redaction breaks the invariant.

Naming convention enforces the audit: every assignment to `UserQueryRedacted` or `DiagnosisRedacted` must be one of the locals `redactedQuery` / `redactedDiagnosis`, populated immediately above the struct literal from `telemetry.Redact(...)`. The grep:

```
grep -nE '(UserQueryRedacted|DiagnosisRedacted):' pkg/agent/agent.go | grep -v 'redacted\(Query\|Diagnosis\)'
```

Expected: zero matches. If anything matches, a bypass has been introduced — investigate before merging.

Tool inputs are NEVER logged. Only tool *names* go into `rag_events.tool_sequence`. If you add a new tool, no new sanitization work is needed — names are already safe by construction; inputs stay out of telemetry.

### Supabase RLS: the baked anon key is least-privilege

The `SUPABASE_KEY` ldflag bakes the **anon** key into every released binary, so treat it as public — the database, not the key, is the security boundary. The `anon` role is locked down by `supabase/migrations/0003_rls_lockdown.sql`:

- `incidents` — INSERT only, gated by a validation policy (`incident_type` enum, length caps, `cluster_id` length = 16). No SELECT/UPDATE/DELETE.
- `sessions` — INSERT only.
- `rag_events` — INSERT; SELECT on **only the `id` column** (needed for the `return=representation` id round-trip in `LogRagEvent`, which posts `?select=id` precisely so a single-column grant suffices); UPDATE on **only** `feedback` / `followup_within_sec` (the feedback-patch path). The redacted corpus columns are unreadable with the anon key. No DELETE.

To keep the `incidents` insert from being rejected when the agent's final text is long, `postIncident` truncates `diagnosis`→8000 and `error_type`→100 (the policy's bounds) instead of letting the row drop; the full redacted text still lands in `rag_events`. If you add a telemetry write or a new column an anon client must touch, add the matching grant + policy — RLS denies by default, so a missing policy fails closed (the write 403s and is silently swallowed, which is how the RAG path was dormant before this lockdown).

## Code style
- Errors wrapped with `%w`, returned up to `main.go` for printing.
- `context.Context` in every function that does I/O.
- No global state — pass dependencies explicitly. (`pkg/k8s/generic.go`'s `sync.Once`-cached dynamic client is the deliberate exception: discovery is expensive and per-process.)
- Comment the WHY, not the WHAT.
- Test-only helpers carry a `ForTesting` suffix (e.g. `ai.SetAPIURLForTesting`) so they're greppable and reviewers notice if they appear in non-test code.

## Do not
- No web server, database, or persistence layer beyond the JSONL history file.
- No auth or multi-tenancy.
- No LLM frameworks (LangChain, LlamaIndex, SDKs) — direct HTTP to Anthropic is the rule.
- No subcommands and no bare-arg one-shot mode. The product is a chat session; a future `--prompt` flag is the only sanctioned non-interactive path.
- Do not store cluster identifiers in telemetry (see invariant above).
- Do not bypass `pkg/safety` for writes. Fail-closed is the rule: any uncertainty → `Confirm`.
- Do not let `get_resource` (or any new generic accessor) issue anything but Get/List, and keep its Secret-value redaction.
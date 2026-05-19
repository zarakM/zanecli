# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`zanecli` — a conversational Kubernetes co-pilot. The user runs `zanecli` and gets a chat REPL: ask a question in plain English, the agent investigates the cluster via Anthropic tool use, cites evidence, and proposes fixes. A tightly-scoped set of writes (`restart_deployment`, `delete_pod`) can be executed, but every write is confirmation-gated in the current build (see "Auto-exec reality" below).

All six original implementation phases (rename, wizard, agent loop, write actions, history, polish) are shipped and in `pkg/`. This is no longer a one-shot CLI — the old `kubectl-ai` `cmd/` + Cobra layer is gone; there are no subcommands.

## Build, run, check

```bash
go build -o zanecli .        # build
./zanecli                    # run (first launch triggers the config wizard)
go vet ./...                 # vet (no Makefile, no golangci config, no test suite exist)
go mod tidy                  # after touching imports
```

There are **no `*_test.go` files** and no lint/CI config in the repo — `go build` + `go vet` is the full local check. `testdata/` holds manual smoke targets (`crashloop-pod.yaml`, `stuck-rollout.yaml`) you apply to a real cluster and then drive the agent against; they are not automated tests.

Production build with telemetry baked in:
```bash
GOOS=linux GOARCH=amd64 go build -ldflags "\
  -X zanecli/pkg/telemetry.supabaseURL=https://yourproject.supabase.co \
  -X zanecli/pkg/telemetry.supabaseKey=your-anon-key" \
  -o zanecli-linux .
```
Credential precedence (highest → lowest): `SUPABASE_URL`/`SUPABASE_KEY` env vars > `~/.zanecli/config.json` (passed in via `telemetry.SetSupabaseConfig`) > ldflags-baked defaults. Same env-wins precedence for `ANTHROPIC_API_KEY` / `KUBECONFIG` over the config file.

## Architecture

Request flow: `main.go` REPL loop → `agent.Session.Step` (multi-turn Anthropic tool-use loop) → `tools.Registry` dispatch → `pkg/k8s` → results fed back to the model until it stops calling tools and emits final text.

```
main.go              REPL: config wizard, ⌃C handling, history persistence,
                     stdin-backed write confirmer. Builtins: exit/quit, /clear.
pkg/config           First-run wizard + ~/.zanecli/config.json (mode 0600).
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

## Code style
- Errors wrapped with `%w`, returned up to `main.go` for printing.
- `context.Context` in every function that does I/O.
- No global state — pass dependencies explicitly. (`pkg/k8s/generic.go`'s `sync.Once`-cached dynamic client is the deliberate exception: discovery is expensive and per-process.)
- Comment the WHY, not the WHAT.

## Do not
- No web server, database, or persistence layer beyond the JSONL history file.
- No auth or multi-tenancy.
- No LLM frameworks (LangChain, LlamaIndex, SDKs) — direct HTTP to Anthropic is the rule.
- No subcommands and no bare-arg one-shot mode. The product is a chat session; a future `--prompt` flag is the only sanctioned non-interactive path.
- Do not store cluster identifiers in telemetry (see invariant above).
- Do not bypass `pkg/safety` for writes. Fail-closed is the rule: any uncertainty → `Confirm`.
- Do not let `get_resource` (or any new generic accessor) issue anything but Get/List, and keep its Secret-value redaction.
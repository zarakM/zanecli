# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`zanecli` — a conversational Kubernetes co-pilot. The user runs `zanecli` and gets a chat session: ask questions, the agent investigates via Anthropic tool use, proposes fixes, and (for a tightly-scoped whitelist) auto-executes safe writes.

This is a heavy redesign of the previous `kubectl-ai` one-shot CLI. The plan lives at `~/.claude/plans/image-1-image-2-sequential-rain.md`. Phase 1 (repo restructure — current state) is shipped; Phase 2 onwards (wizard, agent loop, tools, auto-exec, history, launch) is still ahead.

## Phase 1 state (what's currently in code)

- Single binary `zanecli`, built from root `main.go`.
- REPL skeleton: reads stdin, echoes input. No LLM yet.
- ⌃C handler in place (currently just prints a hint; Phase 3 will use it to abort in-flight agent steps).
- `pkg/ai/claude.go` trimmed to durable transport: `ClaudeClient`, `streamTo`, `complete`, request/response types. All `Diagnose*`/`Answer*`/`Route` methods and their system prompts are deleted.
- `pkg/k8s/client.go` unchanged — every gather strategy (`GatherDiagnostics`, `GatherPendingDiagnostics`, `GatherRolloutDiagnostics`, `GatherNamespaceInventory`) and formatter (`formatPodSpec`, `formatEvents`, `formatNodes`, etc.) is preserved. Phase 3 wraps these as Anthropic tool implementations.
- `pkg/telemetry/logger.go` unchanged — sanitization invariant still holds. Phase 4 adds `tool_call` / `auto_exec` / `confirmed_write` incident types.
- The old `cmd/` directory is deleted along with `diagnose.go`, `rollout.go`, `ask.go`, `root.go`. Cobra dependency dropped (no subcommands).

## Build and run
```bash
go build -o zanecli .
./zanecli
```

Production build with telemetry baked in (still works the same way; module-path prefix updated):
```bash
GOOS=linux GOARCH=amd64 go build -ldflags "\
  -X zanecli/pkg/telemetry.supabaseURL=https://yourproject.supabase.co \
  -X zanecli/pkg/telemetry.supabaseKey=your-anon-key" \
  -o zanecli-linux .
```

Dev override (skip recompile): set `SUPABASE_URL` + `SUPABASE_KEY` env vars — they take precedence over ldflags values.

Test fixtures in `testdata/` — used in Phase 3 onwards as the agent's smoke targets:
- `crashloop-pod.yaml` — pod that crashes immediately. Agent should call `list_pods` → `diagnose_pod` → propose `delete_pod` (auto-exec eligible).
- `stuck-rollout.yaml` — Deployment with permanently failing readiness probe. Agent should call `list_deployments` → `diagnose_rollout` → propose `restart_deployment` (auto-exec eligible).

## Architecture (planned end-state, Phase 6)

```
zanecli (single binary, no subcommands)
  ├── First-launch wizard (if no ~/.zanecli/config.json)
  │     • prompts for ANTHROPIC_API_KEY, kubeconfig path, telemetry y/n, history y/n,
  │       prod-namespace regex; writes ~/.zanecli/config.json
  │
  ├── REPL loop (main.go)
  │     • reads stdin, calls agent.Step(messages, tools), displays response
  │
  ├── pkg/agent — multi-turn tool-use loop
  │     • Anthropic API call with system prompt + tools[] + messages[]
  │     • on tool_use blocks → execute tool → append tool_result → recurse
  │     • streams visible text to stdout; renders tool calls as [bracketed status lines]
  │     • per-session caps: max N turns, max M auto-execs
  │
  ├── pkg/tools — one Go file per tool; read tools always allowed,
  │   write tools gated by pkg/safety/guards.go
  │     READ:    list_pods, list_deployments, list_namespaces,
  │              diagnose_pod, diagnose_rollout, get_pod_logs, get_events,
  │              describe_pod, describe_deployment
  │     AUTO-EXEC WRITES (whitelist):  restart_deployment, delete_pod
  │     CONFIRMABLE WRITES:            scale_deployment, apply_yaml, patch_resource
  │
  ├── pkg/k8s     — REUSED unchanged from kubectl-ai era
  ├── pkg/ai      — REUSED in part: streamTo, complete, ClaudeClient
  ├── pkg/telemetry — REUSED; new incident_types added in Phase 4
  ├── pkg/config  — NEW (Phase 2): wizard + persisted config
  ├── pkg/safety  — NEW (Phase 4): four-guard auto-exec check
  └── pkg/history — NEW (Phase 5): JSONL session log + resume
```

### Auto-exec safety (four guards — Phase 4)

A whitelisted write reaches auto-exec only if all four pass:
1. **Whitelist match** — tool is `restart_deployment` or `delete_pod`.
2. **State precondition** — target is already in failing state (verified via `pkg/k8s` re-fetch immediately before execution).
3. **Production-pattern guard** — namespace does not match the user's configured prod regex (default `(?i)^(prod|production|live)`).
4. **Per-session quota** — max 3 auto-execs per chat session.

Failure of any guard falls back to confirmation prompt.

## Telemetry data model (Supabase `incidents` table)

Eight fields (Phase 1 — unchanged from kubectl-ai era): `incident_type` (`crash` | `pending` | `rollout`), `error_type`, `signals` (jsonb — schema-flexible per `incident_type`), `diagnosis`, `confidence`, `cluster_id` (SHA256 of server URL, first 8 bytes), `model`, `created_at`. No pod names, namespace names, deployment names, env var values, image strings, or secret names are stored.

Sanitization invariant: `pkg/telemetry/logger.go` reads only from the structured side-fields on the diagnostic structs (e.g. `EventReasons`, `SchedulerReason`, `ReplicaCounts`, `WorstPodContainers`), never from the formatted-string fields. The single allowed exception is `WorstPodLogs`, used as `log_tail` for crash and rollout paths.

Phase 4 will add `tool_call`, `auto_exec`, and `confirmed_write` incident types — the same invariant must hold for them.

## Code style
- Errors wrapped with `%w`, returned to `main.go` for printing
- `context.Context` in every function that does I/O
- No global state — pass dependencies explicitly
- Comment the WHY, not the WHAT

## Do not
- Do not add a web server, database, or persistence layer
- Do not add authentication or multi-tenancy
- Do not introduce LLM frameworks (LangChain, LlamaIndex, etc.) — direct HTTP to Anthropic is the rule
- Do not store pod names, namespace names, env var values, or actual cluster URLs in telemetry
- Do not bypass the four-guard auto-exec check (Phase 4 onwards). Fail-closed is the rule: any uncertainty falls back to confirmation prompt
- Do not add subcommands to `zanecli`. The product is a chat session. Power-user one-shot mode is reserved for a future `--prompt` flag, not bare-arg subcommands

# zane — Conversational Kubernetes Co-Pilot

> Talk to your cluster. Investigate, explain, fix.

`zane` is a terminal chat for Kubernetes. Run `zane`, ask a question in plain English, and the agent uses Anthropic tool use to inspect your cluster (pods, deployments, events, logs) and answer with cited evidence. For a tightly-scoped set of writes — restarting a stuck deployment, deleting a CrashLoopBackOff pod — it can also act, but every cluster mutation asks for a `y/N` confirmation first.

## Why this exists

Kubectl is great at *fetching state*. Dashboards are great at *showing state*. Neither is great at *explaining state* — and neither can take action on your behalf.

zane is a chat session that:
- **Investigates before answering.** Asks "why is checkout-api broken?" → fetches pod list → diagnoses the worst replica → cites the failing readiness probe and the relevant log lines.
- **Acts when you approve.** "Yes please restart it" → shows you the exact write, asks `y/N`, and only then issues the `kubectl rollout restart`-equivalent patch.
- **Stays out of your way otherwise.** Conversational answers, no formal incident reports unless you want one.

## Install

### Homebrew (macOS / Linux, recommended)

```bash
brew install zarakM/tap/zane
zane  # first launch triggers the wizard
```

This taps `github.com/zarakM/homebrew-tap` and installs the release binary, so the version is baked in (`main.ClientVersion` matches the tag). Upgrade with `brew upgrade zane`. The formula is regenerated automatically on every `v*` release by GoReleaser.

### Krew (kubectl plugin)

If you use [Krew](https://krew.sigs.k8s.io/), zane is also available as a `kubectl` plugin:

```bash
kubectl krew install zane
kubectl zane   # launches the co-pilot
```

Until the plugin is accepted into the central krew-index, install straight from the manifest in this repo:

```bash
kubectl krew install --manifest=zane.yaml
```

### Pre-built binary

Download the archive for your OS/arch from the [latest release](https://github.com/zarakM/zane/releases/latest), extract it, and move `zane` onto your `PATH`:

```bash
# macOS arm64 (Apple Silicon). Adjust the version and asset name for your platform.
curl -L -o zane.tar.gz \
  https://github.com/zarakM/zane/releases/download/v0.1.4/zane_0.1.4_Darwin_arm64.tar.gz
tar -xzf zane.tar.gz
sudo mv zane /usr/local/bin/zane
zane  # first launch triggers the wizard
```

Archives are available for `Darwin_arm64`, `Darwin_x86_64`, `Linux_arm64`, `Linux_x86_64`, and `Windows_x86_64` (zip). `checksums.txt` is published alongside; verify with `shasum -a 256 -c checksums.txt`.

Pre-built binaries embed the release tag in `main.ClientVersion` (visible in telemetry) — that's the right path for production use.

### `go install` (Go 1.22+)

```bash
go install github.com/zarakM/zane@latest
```

The binary lands in `$(go env GOBIN)` (or `$(go env GOPATH)/bin` if `GOBIN` is unset), named `zane`. Make sure that directory is on your `PATH`. Note that `go install` builds from source, so the release-tag ldflag is **not** applied — `ClientVersion` will be `dev`.

### Build from source

```bash
git clone https://github.com/zarakM/zane
cd zane
go build -o zane .
cp zane /usr/local/bin/zane
```

Cross-compile:

```bash
GOOS=linux GOARCH=amd64 go build -o zane-linux .
```

## First run

```bash
zane
```

A wizard prompts you for:
- **Anthropic API key** (`ANTHROPIC_API_KEY` env var is auto-detected and offered as the default)
- **Kubeconfig path** (`~/.kube/config` is auto-detected and offered as the default)
- **History** (off by default; opt in to persist conversations locally)

Anonymous error-type telemetry is **on by default** and the wizard does not ask about it — its destination is built into the binary, never something you configure (see [Telemetry](#telemetry)). Turn it off any time with `DO_NOT_TRACK=1` or by setting `"telemetry_enabled": false` in the config.

Saved to `~/.zane/config.json` (mode 0600). `ANTHROPIC_API_KEY` and `KUBECONFIG` env vars override the file on every launch.

## Usage

```
$ zane
zane — your Kubernetes co-pilot
Cluster: prod-east.cluster.local:6443
Type your question, or 'exit' to quit. Use /clear to reset the conversation.

> why is checkout-api degraded in prod?
[list_pods...]
[diagnose_pod...]

The checkout-api deployment has 1 of 3 replicas in CrashLoopBackOff
(checkout-api-7d8f9c-x1k, 5 restarts in 10 min). The previous container
exited with code 1; logs show "connection refused" to postgres:5432.

  • image: checkout-api:v2.4.1 (rolled out 12m ago)
  • last termination: Exit code 1 (Error)
  • probe path: GET /healthz on port 8080

Likely cause: a NetworkPolicy or DNS misconfiguration after the recent
deploy is blocking the new pods from reaching postgres.

Next step: kubectl get networkpolicy -n prod -l app=checkout-api

> can you restart it?
[restart_deployment — auto-exec disabled for this session]
Want me to restart deployment prod/checkout-api? [y/N]
```

Built-ins (the only ones — there are no CLI flags or other slash commands):
- `exit` / `quit` — leave the session
- `/clear` — reset the conversation
- `/good` — label the previous answer as helpful (logged to `rag_events.feedback`)
- `/bad` — label the previous answer as unhelpful

## Tools the agent can call

| Read tools | What |
|---|---|
| `list_namespaces` | All namespaces |
| `list_pods` | Pods in a namespace, with phase / ready / restarts |
| `list_deployments` | Deployments with replica counts |
| `describe_pod` | Spec + container statuses |
| `describe_deployment` | Spec + .status conditions |
| `get_pod_logs` | Tail logs (current or previous instance) |
| `get_events` | Events in a namespace, optionally filtered |
| `diagnose_pod` | Full pod diagnostic (auto-detects pending vs crashing) |
| `diagnose_rollout` | Full rollout diagnostic (worst pod + PDBs + events) |
| `list_pvcs` | PersistentVolumeClaims in a namespace (phase, StorageClass, size) |
| `list_storageclasses` | StorageClasses in the cluster (default marked) |
| `get_resource` | Catch-all reader for any other kind (StatefulSet, DaemonSet, Job, PV, Service, HPA, Node, CRDs) as sanitized YAML; Secret values redacted |

| Write tools | What |
|---|---|
| `restart_deployment` | Trigger a `kubectl rollout restart`-equivalent patch |
| `delete_pod` | Delete a controller-managed pod (gets recreated) |

## Safety: writes always confirm

In this build, **every cluster write prompts for `y/N` confirmation** — auto-exec is disabled. There are no `--auto`/`--no-auto` flags and no `/auto` slash command. Before any write, zane shows a `[tool — reason]` status line and a `Want me to ...? [y/N]` prompt; only an explicit `y`/`yes` proceeds.

A staged three-guard auto-exec design exists in `pkg/safety` (session opt-in → whitelist of `restart_deployment`/`delete_pod` → live state precondition → per-session quota) but is intentionally dormant: the session opt-in is forced off at startup, so `Evaluate` always returns the confirmation path. Enabling it is future work, not a current feature.

## Telemetry

On by default — the Supabase destination is baked into release builds at compile time and is never user-facing. To disable, set `DO_NOT_TRACK=1` (the [cross-tool convention](https://consoledonottrack.com)) or `"telemetry_enabled": false` in `~/.zane/config.json`. A `go build` with no baked credentials is a silent no-op; developers/CI can point telemetry elsewhere with the `SUPABASE_URL`/`SUPABASE_KEY` env vars. When on, three Supabase tables are written:

**`incidents`** — one row per diagnostic / write. Anonymous error-type aggregates only:
- `incident_type` (`crash` / `pending` / `rollout` / `auto_exec` / `confirmed_write` / `refused_write`)
- `error_type` and structured `signals` (event reasons, replica counts, etc.)
- A SHA-256 hash of the cluster API URL (first 8 bytes — distinguishes clusters without storing real URLs)
- The model used, the agent's final response text, and a parsed `confidence` (High / Medium / Low)

**`sessions`** — one row per `zane` process. UUID, cluster hash, model, client version.

**`rag_events`** — one row per Step (user input → final answer). The corpus that powers future retrieval / RAG. Carries:
- The user query and the agent's diagnosis, both **redacted** via `pkg/telemetry/sanitize.go` (pod names → `<POD_N>`, namespaces → `<NS_N>`, images → `<IMAGE_N>`, IPs → `<IP_N>`, URLs → `<URL_N>`, UUIDs → `<UUID_N>`, with stable coreference inside one string).
- `tool_sequence` — the ordered list of tool names called (names only, never inputs).
- `step_kind` (`diagnostic` / `chat` / `write` / `mixed`), `round_trip_count`, `error_type`, `confidence`.
- `feedback` (set by `/good` / `/bad`) and `followup_within_sec` (auto-captured).
- `redaction_stats` — per-category counts for QC.

**Never persisted (any table):** raw pod names, namespace names, deployment names, env var values, secret names, image strings, or actual cluster URLs.

Two reviewable grep audits — both should return zero matches:
```
# 1. incidents table (structured side-fields only)
grep -nE 'data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)' pkg/telemetry/logger.go

# 2. rag_events writers must use the redactedQuery / redactedDiagnosis locals
grep -nE '(UserQueryRedacted|DiagnosisRedacted):' pkg/agent/agent.go | grep -v 'redacted\(Query\|Diagnosis\)'
```

Schema migrations live under `supabase/migrations/`. Apply with the Supabase SQL editor or `psql "$SUPABASE_DB_URL" -f supabase/migrations/<file>.sql`.

## History (optional)

If you opt in during the wizard, each session is appended to `~/.zane/history/<UTC-timestamp>.jsonl` (one message per line, mode 0600). On launch, zane offers to resume the most recent session.

History stays on your machine. It contains resource names from your cluster — never uploaded.

## Troubleshooting

### `API returned status 429: rate_limit_error`

Anthropic's per-minute input-token limit. Anthropic tool use re-sends the full conversation (system prompt + tool schemas + every prior message) on every round-trip, so a multi-tool diagnostic Step can easily cross **30K input tokens/min** (the Tier 1 cap for Sonnet).

Fixes, easiest first:

1. **Wait ~60 seconds and retry.** The limit is rolling per-minute.
2. **Upgrade your Anthropic tier.** Add any amount of credits at https://console.anthropic.com/settings/billing to move to Tier 2 (Sonnet: 80K TPM). Tier 3 is 200K TPM.
3. **Switch the model to Haiku.** Edit `pkg/ai/claude.go:24` from `claude-sonnet-4-20250514` to `claude-haiku-4-5-20251001`, rebuild. Haiku gets 50K TPM on Tier 1 and is plenty capable for tool-use orchestration.
4. **Cap log/event tool results.** If you're hitting the limit on the third or fourth turn of one Step, `get_pod_logs` is usually the culprit — large log tails dominate the next request's input. Lowering the default tail in `pkg/tools/logs_events.go` shrinks every subsequent turn.

### `kubeconfig: ... no such file or directory`

Either set `KUBECONFIG` to a real path, or edit `~/.zane/config.json` and update `kubeconfig_path`. Env vars override the file on every launch.

### Writes prompt every time, even for safe-looking restarts

That's by design — `AutoExec` is hard-off in `main.go` for the v1 build. See [Safety: writes always confirm](#safety-writes-always-confirm).

## Development

```bash
go build -o zane .
go vet ./...
go test ./... -race -count=1
```

CI (`.github/workflows/ci.yml`) runs the suite on every push and PR, plus two grep-based invariant guards: the [`incidents`-table side-fields-only rule](#telemetry) and the `rag_events` redaction-local-naming audit.

Tests live in `*_test.go` next to the source they cover — `pkg/safety`, `pkg/k8s`, `pkg/telemetry`, `pkg/tools`, `pkg/ai`, `pkg/history`, `pkg/config`, `pkg/agent`. `pkg/k8s.NewClientFromInterface` and `pkg/ai.SetAPIURLForTesting` are the two test-only seams; both are named so a `grep ForTesting` in production code surfaces misuse immediately.

`testdata/` holds manual smoke targets (`crashloop-pod.yaml`, `stuck-rollout.yaml`) for end-to-end runs against a real cluster — apply, then drive the agent against the failing workload.

If you use Claude Code, `.claude/skills/` ships two project skills: `zane-review` (review a diff against the project invariants) and `open-pr` (run the pre-PR gates, then branch, commit, push, and open the PR the house way). See [CONTRIBUTING.md](CONTRIBUTING.md).

## Status

Pre-launch. Six implementation phases shipped (rename, wizard, agent loop, write actions, history, polish). Feedback welcome — open an issue or DM.

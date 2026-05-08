# zanecli — Conversational Kubernetes Co-Pilot

> Talk to your cluster. Investigate, explain, fix.

`zanecli` is a terminal chat for Kubernetes. Run `zanecli`, ask a question in plain English, and the agent uses Anthropic tool use to inspect your cluster (pods, deployments, events, logs) and answer with cited evidence. For a tightly-scoped set of safe writes — restarting a stuck deployment, deleting a CrashLoopBackOff pod — it can also act, with a four-guard safety check before anything touches the API.

## Why this exists

Kubectl is great at *fetching state*. Dashboards are great at *showing state*. Neither is great at *explaining state* — and neither can take action on your behalf.

zanecli is a chat session that:
- **Investigates before answering.** Asks "why is checkout-api broken?" → fetches pod list → diagnoses the worst replica → cites the failing readiness probe and the relevant log lines.
- **Acts when it's safe.** "Yes please restart it" → runs `kubectl rollout restart` after re-checking the deployment is actually stuck and the namespace isn't production.
- **Stays out of your way otherwise.** Conversational answers, no formal incident reports unless you want one.

## Install

```bash
go install zanecli@latest
```

Or build from source:

```bash
git clone <this-repo>
cd zanecli
go build -o zanecli .
cp zanecli /usr/local/bin/zanecli
```

Cross-compile:

```bash
GOOS=linux GOARCH=amd64 go build -o zanecli-linux .
```

## First run

```bash
zanecli
```

A wizard prompts you for:
- **Anthropic API key** (`ANTHROPIC_API_KEY` env var is auto-detected if set)
- **Kubeconfig path** (defaults to `~/.kube/config` if it exists)
- **Telemetry** (anonymous error-type aggregates only — see [Telemetry](#telemetry))
- **History** (off by default; opt in to persist conversations locally)
- **Production-namespace regex** (default `(?i)^(prod|production|live)`)

Saved to `~/.zanecli/config.json` (mode 0600). Env vars override the file on every launch.

## Usage

```
$ zanecli
zanecli — your Kubernetes co-pilot
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
[restart_deployment — namespace "prod" matches production pattern]
Want me to restart deployment prod/checkout-api? [y/N]
```

Built-ins:
- `exit` / `quit` — leave the session
- `/clear` — reset the conversation (drops history, resets quotas)

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

| Write tools | What |
|---|---|
| `restart_deployment` | Trigger a `kubectl rollout restart`-equivalent patch |
| `delete_pod` | Delete a controller-managed pod (gets recreated) |

## Safety: the four-guard auto-exec check

A write reaches auto-exec only if **all four** pass:

1. **Whitelist** — the tool is `restart_deployment` or `delete_pod`. Everything else (scale, apply YAML, patch arbitrary resources) always prompts.
2. **Production pattern** — the namespace does NOT match your configured prod regex (default `(?i)^(prod|production|live)`).
3. **State precondition** — re-fetched live just before execution. `delete_pod` only auto-execs on CrashLoopBackOff/ImagePullBackOff pods that have an owner. `restart_deployment` only auto-execs when Progressing=False or ready<desired.
4. **Per-session quota** — at most 3 auto-execs per chat session. After that, every write requires confirmation.

Failure of any guard falls back to a `Want me to ...? [y/N]` prompt.

## Telemetry

Optional, opt-out per run via `--no-telemetry` (or off entirely in the wizard). Sends anonymous error-type aggregates to a Supabase backend:
- `incident_type` (`crash` / `pending` / `rollout` / `auto_exec` / `confirmed_write` / `refused_write`)
- `error_type` and structured `signals` (event reasons, replica counts, etc.)
- A SHA-256 hash of the cluster API URL (first 8 bytes — distinguishes clusters without storing real URLs)
- The model used, and the agent's response text

**Never persisted:** pod names, namespace names, deployment names, env var values, secret names, or actual cluster URLs.

The grep that should always return zero matches against `pkg/telemetry/logger.go`:
```
data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)
```

## History (optional)

If you opt in during the wizard, each session is appended to `~/.zanecli/history/<UTC-timestamp>.jsonl` (one message per line, mode 0600). On launch, zanecli offers to resume the most recent session.

History stays on your machine. It contains resource names from your cluster — never uploaded.

## Status

Pre-launch. Six implementation phases shipped (rename, wizard, agent loop, write actions, history, polish). Feedback welcome — open an issue or DM.

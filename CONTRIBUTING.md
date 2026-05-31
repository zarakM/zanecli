# Contributing to zanecli

Thanks for taking a look. zanecli is small and opinionated — a couple of read-throughs of `CLAUDE.md` and the package layout in `pkg/` will give you the full picture.

## Quick start

```bash
git clone https://github.com/zarakM/zanecli
cd zanecli
go build -o zanecli .
go test ./... -race
```

Requires Go 1.22+. There's no Makefile and no linter config — `go build`, `go vet`, and `go test` are the full local check. CI runs the same plus two grep-based invariant guards (see [Telemetry invariants](#telemetry-invariants) below).

### Install the git hooks (one-time)

After cloning, point git at the tracked hooks directory so the pre-push guard runs on every push:

```bash
bash .githooks/install.sh        # sets core.hooksPath -> .githooks
```

The `pre-push` hook blocks a push if the outgoing commits add binaries, files over 1 MiB, secret/credential files (`.env`, `*.pem`, `kubeconfig`, …), hardcoded secret values, or zanecli build artifacts. Bypass once, at your own risk, with `git push --no-verify`.

## Before you open a PR

- **Tests pass:** `go test ./... -race -count=1`
- **No format drift:** `gofmt -l .` returns nothing
- **Imports are clean:** `go mod tidy` leaves `go.mod` / `go.sum` unchanged
- **One focused change per PR.** Refactors and feature work in separate PRs make review easy.

For substantive changes, please open an issue first so we can align on the approach before you spend time on it.

If you work with Claude Code, two project skills automate the loop above: the
`open-pr` skill runs these gates and opens the PR for you, and `zanecli-review`
reviews a diff against the invariants below (see `.claude/skills/`).

## Project rules (do not break)

These come straight from `CLAUDE.md` and are enforced in CI.

### Telemetry invariants

The product anonymizes everything before sending to Supabase. Two grep checks must return zero matches:

```bash
# 1. The incidents writer may only read structured side-fields off the k8s
#    diagnostic structs (EventReasons, ReplicaCounts, ...), never the
#    formatted-string fields (Events, PodSpec, ...).
grep -nE 'data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)' pkg/telemetry/logger.go

# 2. Every assignment to UserQueryRedacted / DiagnosisRedacted in agent.go
#    must come from the canonically-named locals `redactedQuery` /
#    `redactedDiagnosis`, populated immediately above via telemetry.Redact(...).
grep -nE '(UserQueryRedacted|DiagnosisRedacted):' pkg/agent/agent.go | grep -v 'redacted\(Query\|Diagnosis\)'
```

If your PR causes either to find a match, that's a sanitization bypass — fix it before merging. `CLAUDE.md` documents the reasoning in full.

### Writes are always confirm-gated

`main.go` hard-sets `cfg.AutoExec = false`. Every cluster mutation routes through `pkg/safety.Guard.Evaluate`, which returns `Confirm` whenever it can't prove the write is safe. Don't bypass this. If you add a new write tool, register it in `safety.IsWriteTool` and add a precondition check.

### No LLM frameworks

Direct HTTP to Anthropic, no SDK. See `pkg/ai/claude.go`.

### Comment the WHY, not the WHAT

If the code is obvious, no comment. If there's a constraint, invariant, or surprising decision, name it.

## How to add a tool

1. New file in `pkg/tools/` implementing the `Tool` interface (Name / Description / InputSchema / Run).
2. Register it in `pkg/tools/tools.go` `NewRegistry`.
3. Mention it in the agent's system prompt at the bottom of `pkg/agent/agent.go` — the tool inventory and the relevant per-kind playbook.
4. If it's a write, add it to `safety.IsWriteTool` and write a precondition check in `pkg/safety/guards.go`.
5. Add a test in `pkg/tools/tools_test.go` (uses `k8s.NewClientFromInterface(fake.NewSimpleClientset(...))`).

## How to run a release

Tag the commit and push the tag — `.github/workflows/release.yml` runs GoReleaser, builds binaries for Linux/macOS/Windows (amd64 + arm64), and attaches them to a GitHub Release.

```bash
git tag v0.1.0
git push --tags
```

Pre-release tags (`v0.1.0-rc.1`, etc.) are marked as pre-release automatically.

## Reporting bugs / asking for features

Open a GitHub issue using one of the templates. The bug template asks for the failing user prompt, the tool calls the agent made, and the cluster shape — those three together solve most issues quickly.

## License

By contributing, you agree your contribution is licensed under the project's MIT + Commons Clause + Enterprise Use Restriction (see `LICENSE`).

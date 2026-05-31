---
name: open-pr
description: Open a pull request for zanecli the repeatable way — run the pre-PR checks (tests, gofmt, go mod tidy, the two invariant grep guards), branch/commit/push correctly, then create the PR with the house body format. Use whenever a task is finished and the user wants to ship it as a PR.
---

# open-pr

The PR-creation ritual for the `zanecli` repo, captured so it runs the same way
every time. It is deterministic on purpose: the steps below come straight from
`CONTRIBUTING.md` ("Before you open a PR"), `CLAUDE.md` (the CI invariants), and
the repo's git conventions. Follow them in order; do not skip the gates.

Run only after the actual work is committed-worthy (code done, change focused).
One focused change per PR — if the working tree mixes unrelated work, stop and
ask before bundling it.

## 1. Pre-flight gates (all must pass before committing)

Run from the repo root. If any fails, fix it before going further — do not open
a PR on red.

```bash
go test ./... -race -count=1     # full suite (~3s), must be green
gofmt -l .                       # must print NOTHING (no format drift)
go vet ./...                     # must be clean
go mod tidy                      # then: git diff --exit-code go.mod go.sum  (must be unchanged)
```

The two **invariant guards** (CI blocks the merge on these; catch them locally):

```bash
# Telemetry: incidents writer reads only structured side-fields, never identifiers
grep -nE 'data\.(Events|PodSpec|WorstPodSpec|PodSummary|NodeSummary|QuotaSummary|PVCSummary|ReplicaSets|PDBs|DeploymentName|PodName|Namespace|DeploymentSpec)' pkg/telemetry/logger.go

# RAG: redacted fields come only from redactedQuery / redactedDiagnosis locals
grep -nE '(UserQueryRedacted|DiagnosisRedacted):' pkg/agent/agent.go | grep -v 'redacted\(Query\|Diagnosis\)'
```

Both must return **zero matches**. (The `zanecli-review` skill's
`scripts/review-checks.sh` runs these plus more — prefer it if the diff touched
`pkg/telemetry`, `pkg/agent`, `pkg/safety`, or `pkg/tools`.)

## 2. Branch

Never commit straight to `main`. If `git branch --show-current` is `main`, cut a
branch first, named for the change (kebab-case, e.g. `add-pr-skill`):

```bash
git checkout -b <branch-name>
```

If already on a feature branch, stay on it.

## 3. Commit

Stage the focused change and commit with a concise, imperative subject. End the
message with the required trailer:

```
<imperative subject line>

<optional body: the why, not the what>

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
```

## 4. Push

```bash
git push -u origin <branch-name>
```

The tracked `pre-push` hook (`.githooks/`, installed via `.githooks/install.sh`)
will block the push if the commits add binaries, files >1 MiB, secret/credential
files, hardcoded secrets, or build artifacts. If it fires, **do not** reach for
`--no-verify` — investigate what tripped it and remove the offending file.

## 5. Create the PR

Use `gh pr create --base main`. Match the house body format seen on existing PRs
(`#1`, `#3`): a `## What` (or `## Summary`) heading, then bullets describing what
changed and why. Keep it scannable. End the body with:

```
🤖 Generated with [Claude Code](https://claude.com/claude-code)
```

```bash
gh pr create --base main --title "<subject>" --body "$(cat <<'EOF'
## What

- <bullet: what changed>
- <bullet: why / what it enables>

## Notes
- Tests: `go test ./... -race -count=1` green
- Invariant guards: zero matches

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

## 6. Report back

Give the user the PR URL. If CI is the gate they care about, offer to watch the
run (`gh run watch` / poll the Actions API), but don't block on it unasked.

## Guardrails

- **Don't** open a PR with failing tests, format drift, or a non-zero guard grep.
- **Don't** bypass the pre-push hook.
- **Don't** bundle unrelated changes — one focused PR (CONTRIBUTING.md rule).
- **Don't** push to `main` directly.
- For substantive design changes, CONTRIBUTING.md asks for an issue first — flag
  that to the user rather than silently opening a large PR.

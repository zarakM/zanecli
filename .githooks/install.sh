#!/usr/bin/env bash
# Point this clone's git hooks at the tracked .githooks/ directory.
# Run once after cloning:  bash .githooks/install.sh
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
git config core.hooksPath .githooks
chmod +x .githooks/pre-push 2>/dev/null || true

echo "Installed: core.hooksPath -> .githooks"
echo "The pre-push hook now guards every 'git push' from this clone."
echo "Bypass once (at your own risk): git push --no-verify"

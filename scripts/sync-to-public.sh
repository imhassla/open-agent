#!/usr/bin/env bash
# Sync the current dev branch's tree onto the clean `public` branch as ONE commit.
# Use this to publish the current state wholesale. For SELECTIVE updates, prefer
# cherry-picking specific commits (see PUBLISHING.md → Development workflow).
set -euo pipefail
MSG="${1:-sync from dev}"
[ -z "$(git status --porcelain)" ] || { echo "working tree not clean — commit or stash first"; exit 1; }
git rev-parse --verify public >/dev/null 2>&1 || { echo "no 'public' branch"; exit 1; }
DEV=$(git branch --show-current)
git checkout public
git checkout "$DEV" -- .
if git diff --cached --quiet; then
  echo "public already matches $DEV — nothing to sync"
else
  git commit -q -m "sync: $MSG"
  echo "public updated from $DEV"
fi
git checkout "$DEV"
echo "push later with:  git push origin public:main"

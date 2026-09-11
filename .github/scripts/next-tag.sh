#!/usr/bin/env bash
set -euo pipefail

if git describe --tags --exact-match HEAD >/dev/null 2>&1; then
  echo "head already tagged, skipping"
  exit 0
fi

latest="$(git describe --tags --abbrev=0 2>/dev/null || true)"
if [ -n "$latest" ]; then
  range="${latest}..HEAD"
  base="${latest#v}"
else
  range="HEAD"
  base="0.0.0"
fi

major="${base%%.*}"
rest="${base#*.}"
minor="${rest%%.*}"
patch="${rest#*.}"

subjects="$(git log --format='%s' "$range")"
bodies="$(git log --format='%b' "$range")"

if echo "$subjects" | grep -qE '^[a-zA-Z]+(\([^)]*\))?!:' ||
  echo "$bodies" | grep -qE 'BREAKING CHANGE'; then
  major=$((major + 1))
  minor=0
  patch=0
elif echo "$subjects" | grep -qE '^(feat|perf)(\([^)]*\))?:'; then
  minor=$((minor + 1))
  patch=0
elif echo "$subjects" | grep -qE '^fix(\([^)]*\))?:'; then
  patch=$((patch + 1))
else
  echo "no releasable commits since ${latest:-start}, skipping"
  exit 0
fi

next="v${major}.${minor}.${patch}"
if git rev-parse -q -e "refs/tags/${next}" >/dev/null; then
  echo "tag ${next} already exists, skipping"
  exit 0
fi

git config user.name "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"
git tag -a "$next" -m "$next"
git push origin "refs/tags/${next}"
echo "tagged ${next}"

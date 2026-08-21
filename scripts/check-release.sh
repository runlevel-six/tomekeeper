#!/usr/bin/env bash
#
# Release hygiene, checked rather than remembered.
#
# Four things this repository promises about a release, each of which is invisible
# until somebody deploys the wrong thing:
#
#   1. Every image reference in deploy/ and compose.yaml names the same version.
#      Five manifests reference the image — server, worker, both initContainers,
#      and the migration Job — and a half-finished bump is a deploy where the
#      migration and the pods run different builds. That is the failure that took
#      the site down on 2026-08-20.
#   2. That version is a release tag, never `latest` or `edge`. A moving tag means
#      "what is running" has no answer.
#   3. It matches the newest version in CHANGELOG.md, so the tree always says which
#      release it deploys and the changelog cannot silently fall behind.
#   4. A patch release adds no migration. CHANGELOG.md promises that a patch
#      upgrade is only a tag change; this is what makes the promise true.
#
# Run by `task check` and by CI. Exits 0 or names what is wrong.
set -euo pipefail

cd "$(dirname "$0")/.."

fail() { printf '\033[31mFAIL\033[0m %s\n' "$*" >&2; failed=1; }
pass() { printf '\033[32mok\033[0m   %s\n' "$*"; }
failed=0

image=ghcr.io/runlevel-six/tomekeeper

# ---------------------------------------------------------------------------
# 1. One version across every reference.
# ---------------------------------------------------------------------------
mapfile -t refs < <(grep -rhoE "${image}:[A-Za-z0-9._-]+" deploy compose.yaml | sort -u)
if [ "${#refs[@]}" -eq 0 ]; then
  fail "no image references found at all — this check is looking in the wrong place"
elif [ "${#refs[@]}" -gt 1 ]; then
  fail "deploy/ and compose.yaml disagree about the image version:"
  printf '       %s\n' "${refs[@]}" >&2
  grep -rnE "${image}:[A-Za-z0-9._-]+" deploy compose.yaml | sed 's/^/       /' >&2
else
  pass "every image reference names ${refs[0]#"$image":}"
fi

pinned=${refs[0]#"$image":}

# Every kustomize override has to agree too, because each silently replaces all of
# the above for whatever overlay it belongs to. Not just the base: the committed
# example overlay is what other people copy, and it was left following `latest`
# after the base was pinned — which an earlier version of this check did not
# notice, because it only read the base.
#
# Read from the image's entry to the end of it, rather than through a fixed
# two-line window. `grep -A 2` was the window, and a comment written between
# `name:` and `newTag:` pushed the pin out of it — which did not fail, it
# *skipped*: an unparsed override silently stopped being checked, and the only
# trace was the count in the line below going from 3 to 2. Found by writing such a
# comment while pinning an overlay off-release, where a half-finished version bump
# would have passed just as quietly.
#
# awk to the next list item at the same indent, so any amount of comment or any
# other field can sit inside the entry. A digest pin is recognized too: it is the
# other legitimate way to name an image, and reading it as "no override" is the
# same silence in a different place.
mapfile -t kustomizations < <(find deploy -name kustomization.yaml | sort)
overrides=0
for k in "${kustomizations[@]}"; do
  entry=$(awk -v img="name: ${image}" '
    index($0, img) { inside = 1; next }
    inside && /^[[:space:]]*-[[:space:]]/ { inside = 0 }
    inside { print }
  ' "$k")

  newtag=$(printf '%s\n' "$entry" | sed -n 's/^[[:space:]]*newTag:[[:space:]]*//p' | head -1)
  digest=$(printf '%s\n' "$entry" | sed -n 's/^[[:space:]]*digest:[[:space:]]*//p' | head -1)

  if [ -z "$newtag" ] && [ -n "$digest" ]; then
    overrides=$((overrides + 1))
    pass "${k} pins the digest ${digest}"
    continue
  fi
  [ -z "$newtag" ] && continue
  overrides=$((overrides + 1))
  if [ "$newtag" != "$pinned" ]; then
    fail "${k} pins ${newtag} but the manifests reference ${pinned}"
  fi
done
if [ "$overrides" -eq 0 ]; then
  fail "no kustomization sets newTag or digest for ${image}; the manifests are the only pin"
else
  pass "${overrides} kustomization(s) agree on ${pinned}"
fi

# ---------------------------------------------------------------------------
# 2. A release tag, not a moving one.
# ---------------------------------------------------------------------------
case "$pinned" in
  latest | edge | sha-*)
    fail "the deployment follows the moving tag '${pinned}'; pin a release (vX.Y.Z)"
    ;;
  v[0-9]*.[0-9]*.[0-9]*)
    pass "${pinned} is a release tag"
    ;;
  *)
    fail "'${pinned}' is not a vX.Y.Z release tag"
    ;;
esac

# ---------------------------------------------------------------------------
# 3. The changelog's newest release is the pinned one.
# ---------------------------------------------------------------------------
# The first "## [vX.Y.Z]" heading, skipping [Unreleased].
latest_logged=$(sed -n 's/^## \[\(v[0-9][^]]*\)\].*/\1/p' CHANGELOG.md | head -1)
if [ -z "$latest_logged" ]; then
  fail "CHANGELOG.md has no released version heading"
elif [ "$latest_logged" != "$pinned" ]; then
  fail "CHANGELOG.md's newest release is ${latest_logged} but the tree deploys ${pinned}"
else
  pass "CHANGELOG.md's newest release is ${latest_logged}"
fi

# ---------------------------------------------------------------------------
# 4. A patch release adds no migration.
# ---------------------------------------------------------------------------
# Compared against the previous release tag rather than against the working tree's
# own history, because the promise is about what an operator has to do to get from
# one release to the next.
previous=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-version:refname |
             grep -Fxv "$pinned" | head -1 || true)

if [ -z "$previous" ]; then
  pass "no earlier release to compare migrations against"
else
  count_at() { git ls-tree --name-only "$1" internal/db/migrations/ 2>/dev/null | grep -c '\.sql$' || true; }
  before=$(count_at "$previous")
  now=$(ls internal/db/migrations/*.sql 2>/dev/null | wc -l)

  prev_minor=${previous%.*}
  this_minor=${pinned%.*}

  if [ "$now" -gt "$before" ] && [ "$prev_minor" = "$this_minor" ]; then
    fail "${pinned} adds $((now - before)) migration(s) since ${previous} but only bumps the patch;
       CHANGELOG.md promises a patch upgrade is a tag change and nothing else — make it a minor"
  elif [ "$now" -lt "$before" ]; then
    fail "migrations were removed between ${previous} and ${pinned}; they are append-only"
  else
    pass "migrations are consistent with a ${prev_minor}→${this_minor} bump ($before → $now files)"
  fi
fi

# ---------------------------------------------------------------------------
# 5. The version the binary will report matches the pin, when built from a tag.
# ---------------------------------------------------------------------------
# Only meaningful on a tagged checkout: elsewhere `git describe` correctly reports
# a development version, and demanding otherwise would fail every ordinary build.
described=$(git describe --tags --match 'v[0-9]*' --exact-match 2>/dev/null || true)
if [ -n "$described" ] && [ "$described" != "$pinned" ]; then
  fail "this commit is tagged ${described} but the tree deploys ${pinned}"
elif [ -n "$described" ]; then
  pass "the checkout is tagged ${described}, matching the pin"
fi

if [ "$failed" -ne 0 ]; then
  echo
  echo "See docs/how-to/cut-a-release.md for the sequence these checks assume." >&2
  exit 1
fi

echo
echo "Release pins are consistent: ${pinned}"

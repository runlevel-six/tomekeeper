#!/usr/bin/env bash
#
# Every new commit carries a DCO sign-off.
#
# CONTRIBUTING.md requires one, and .githooks/prepare-commit-msg adds it — but a
# hook only helps people who ran `task hooks:install`, and the flag it replaces
# leaked on roughly a third of the commits before it existed. This is the half
# that does not depend on anybody's local setup.
#
# **Only new commits.** History predating this check contains commits with no
# trailer, so a run over everything would fail forever and be turned off within a
# day. CI passes the range that a push or a pull request actually added; a bare
# revision checks that one commit.
#
# Merge commits are exempt, matching the hook: `git merge --no-ff` on someone
# else's branch would otherwise demand a sign-off on their work from whoever
# happened to merge it.
#
# Usage:
#   scripts/check-dco.sh                  # origin/master..HEAD
#   scripts/check-dco.sh <base>..<head>   # an explicit range
#   scripts/check-dco.sh <rev>            # that one commit
set -uo pipefail

cd "$(dirname "$0")/.."

fail() { printf '\033[31mFAIL\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33mwarn\033[0m %s\n' "$*" >&2; }
pass() { printf '\033[32mok\033[0m   %s\n' "$*"; }

range=${1:-}

if [ -z "$range" ]; then
  # No range given: compare against the remote's idea of the branch. When there
  # is no remote-tracking ref — a fresh clone with no fetch, or a repository that
  # has never had a remote — there is genuinely nothing to compare against, and
  # saying so is better than inventing a range and reporting a pass about it.
  if git rev-parse -q --verify origin/master >/dev/null; then
    range=origin/master..HEAD
  elif git rev-parse -q --verify origin/main >/dev/null; then
    range=origin/main..HEAD
  else
    echo "no origin/master or origin/main to compare against; nothing to check"
    exit 0
  fi
fi

case "$range" in
  *..*) revs=$(git rev-list --no-merges "$range" 2>/dev/null) || {
          fail "'$range' is not a range this repository can resolve"
          exit 1
        } ;;
  # --no-walk, not --max-count=1: the latter traverses ancestry, so asking about a
  # single merge commit quietly reported on the first non-merge commit behind it
  # instead of saying there was nothing to check. A pass about a different commit
  # than the one asked about is worse than either answer.
  *)    revs=$(git rev-list --no-walk --no-merges "$range" 2>/dev/null) || {
          fail "'$range' is not a revision this repository can resolve"
          exit 1
        } ;;
esac

if [ -z "$revs" ]; then
  pass "no new non-merge commits in $range"
  exit 0
fi

missing=0
mismatched=0
count=0

for rev in $revs; do
  count=$((count + 1))
  subject=$(git log -1 --format='%s' "$rev")
  signoffs=$(git log -1 --format='%(trailers:key=Signed-off-by,valueonly,unfold)' "$rev")

  if [ -z "$(printf '%s' "$signoffs" | tr -d '[:space:]')" ]; then
    fail "$(git rev-parse --short "$rev") has no Signed-off-by: $subject"
    missing=$((missing + 1))
    continue
  fi

  # Present but nobody's. The DCO is an assertion by the author, so a trailer
  # naming only somebody else is worth saying out loud — without failing on it,
  # because a second address for the same person is ordinary and a carried patch
  # legitimately keeps its original author.
  author=$(git log -1 --format='%ae' "$rev")
  if ! printf '%s\n' "$signoffs" | grep -qiF "<$author>"; then
    warn "$(git rev-parse --short "$rev") is signed off, but not by its author ($author): $subject"
    mismatched=$((mismatched + 1))
  fi
done

if [ "$missing" -gt 0 ]; then
  echo >&2
  fail "$missing of $count commit(s) have no sign-off."
  cat >&2 <<'EOT'

       Add one to a commit you have not pushed with:

           git commit --amend -s --no-edit

       or to a run of them with:

           git rebase --signoff <base>

       `task hooks:install` adds the trailer from then on. See the DCO section of
       CONTRIBUTING.md for what it certifies.
EOT
  exit 1
fi

if [ "$mismatched" -gt 0 ]; then
  pass "$count commit(s) signed off ($mismatched not by the commit author, see above)"
else
  pass "$count commit(s) signed off"
fi

#!/bin/sh
# Shows that the git outputs the job prompt (step 1) relies on are really printed: a conflicted `git pull --rebase`
# shows a rebase in progress and "Unmerged paths"; after `git rebase --abort` the branch is ahead and behind and
# `git status` says "have diverged"; `git status -sb` shows "ahead 1, behind 1".
# Needs: git. Run from the repository root: sh scripts/test/recipe-git.sh
set -eu
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
g() { git -c user.name=t -c user.email=t@example.invalid "$@"; }
g init -q --bare -b main "$T/remote.git"
g clone -q "$T/remote.git" "$T/A" 2>/dev/null
mkdir -p "$T/A/wiki/p" && printf '# Ports\n\nline\n' > "$T/A/wiki/p/Ports.md"
(cd "$T/A" && g add -A && g commit -q -m base && g push -q origin HEAD:main && g branch -q --set-upstream-to=origin/main main 2>/dev/null || true)
g clone -q "$T/remote.git" "$T/B" 2>/dev/null
printf '# Ports\n\nline from B\n' > "$T/B/wiki/p/Ports.md"
(cd "$T/B" && g commit -q -am "B merged first" && g push -q origin HEAD:main)
printf '# Ports\n\nline from A\n' > "$T/A/wiki/p/Ports.md"
cd "$T/A" && g commit -q -am "A merged"

g pull --rebase >/dev/null 2>&1 && fail "the pull should have stopped with a conflict"
s=$(g status)
echo "$s" | grep -q "rebase in progress\|currently rebasing" || fail "git status does not show the rebase: $s"
echo "$s" | grep -q "Unmerged paths" || fail "git status does not show Unmerged paths: $s"
g rebase --abort
sb=$(g status -sb | head -1)
echo "$sb" | grep -q "ahead 1, behind 1" || fail "git status -sb: $sb"
g status | grep -q "have diverged" || fail "git status does not say 'have diverged': $(g status)"
[ -z "$(g status --porcelain)" ] || fail "the abort left the tree dirty"
echo PASS

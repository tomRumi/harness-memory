#!/bin/sh
# Shows the fourth-review condition and its fix with the real binary: a hand-made, committed wiki/Proj folder (capital
# letters; Obsidian users make them) must make `hms curate -submit` refuse with the rename instruction, change nothing
# and exit 1. It holds on case-insensitive file systems (where git would not stage the lower-case path) and on
# case-sensitive ones (where the folder is a second folder). Needs: git, bin/hms (go build -o bin/hms ./cmd/hms).
# Run from the repository root: sh scripts/test/capital-folder.sh
set -eu
HMS=$(pwd)/bin/hms
[ -x "$HMS" ] || { echo "FAIL: build bin/hms first"; exit 1; }
T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
export HMS_HOME="$T/home" HMS_VECTORS=0
unset HMS_REPO HMS_DB HMS_EMBED_URL HMS_EMBED_API HMS_EMBED_MODEL HMS_LLM_URL HMS_LLM_MODEL HMS_LLM_KEY
"$HMS" init >/dev/null 2>&1 || fail "init"
R="$T/home/memory"
g() { git -C "$R" -c user.name=t -c user.email=t@example.invalid "$@"; }
mkdir -p "$R/notes/proj" "$R/wiki/Proj"
for n in 1 2; do
  printf -- '---\nkind: learning\ncreated: 2020-01-0%sT10:00:00Z\nsource: learn\n---\n\nNote number %s about the pump that restarts after 12 failed checks.\n' "$n" "$n" > "$R/notes/proj/1111111$n-aaaa.md"
done
printf '# Old\n\nWritten by hand.\n' > "$R/wiki/Proj/Old.md"
g add -A && g commit -q -m seed
printf '{"project":"proj","pages":[{"title":"Pump","summary":"s","body":"# Pump\\n\\n- Note number 1 about the pump that restarts after 12 failed checks.\\n- Note number 2 about the pump that restarts after 12 failed checks.\\n","notes":["11111111-aaaa","11111112-aaaa"]}]}' > "$T/a.json"
"$HMS" rescan >/dev/null 2>&1 || fail "rescan"
if "$HMS" curate -submit "$T/a.json" >"$T/out" 2>"$T/err"; then fail "the merge was not refused: $(cat "$T/out")"; fi
grep -q "the folder wiki/Proj has capital letters; project names are lower-case. Rename it to wiki/proj (git mv) and run hms rescan" "$T/err" || fail "no rename instruction: $(cat "$T/err")"
[ -z "$(g status --porcelain -uall)" ] || fail "the refused merge changed files: $(g status --porcelain -uall)"
[ -f "$R/notes/proj/11111111-aaaa.md" ] || fail "a note was removed"
echo PASS

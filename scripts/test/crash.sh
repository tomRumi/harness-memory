#!/bin/sh
# Kills `hms curate -submit` with SIGKILL while its slow copy check hangs (an embedder that never answers), then
# checks that nothing was left uncommitted, and that the next writing command commits what a killed run left behind.
# Needs: go, git, python3. Run from the repository root: sh scripts/test/crash.sh
set -eu
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
T=$(mktemp -d)
HP=
cleanup() { [ -n "$HP" ] && kill "$HP" 2>/dev/null || true; rm -rf "$T"; }
trap cleanup EXIT
fail() { echo "FAIL: $*"; exit 1; }

(cd "$ROOT" && go build -o "$T/hms" ./cmd/hms)
export HMS_HOME="$T/home"
unset HMS_REPO HMS_DB HMS_EMBED_API HMS_EMBED_MODEL HMS_LLM_URL HMS_LLM_MODEL HMS_LLM_KEY
R="$HMS_HOME/memory"

# an embedder that accepts connections and never answers
PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
python3 -c '
import socket,sys
s=socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1); s.bind(("127.0.0.1",int(sys.argv[1]))); s.listen(50)
cs=[]
while True:
    c,_=s.accept(); cs.append(c)
' "$PORT" >/dev/null 2>&1 &
HP=$!

HMS_VECTORS=0 "$T/hms" init >/dev/null
git -C "$R" config user.email t@example.invalid
git -C "$R" config user.name t
mkdir -p "$R/notes/proj"
printf -- '---\nkind: learning\ncreated: 2020-01-01T10:00:00Z\nsource: learn\n---\n\nBackups run at 02:30 and keep 7 copies on the second disk of the archive server.\n' > "$R/notes/proj/aaaaaaaa-1111.md"
printf -- '---\nkind: learning\ncreated: 2020-01-02T10:00:00Z\nsource: learn\n---\n\nThe nightly job restarts the worker after 12 failed health checks and pages the owner.\n' > "$R/notes/proj/bbbbbbbb-2222.md"
git -C "$R" add -A && git -C "$R" commit -q -m seed

cat > "$T/sub.json" <<'JSON'
{"project":"proj","pages":[{"title":"Sauvegarde nocturne é","summary":"s","body":"# Backups\n\n- Backups run at 02:30 and keep 7 copies on the second disk of the archive server.\n- The nightly job restarts the worker after 12 failed health checks and pages the owner.\n","notes":["aaaaaaaa-1111","bbbbbbbb-2222"]}]}
JSON

# 1. the submit hangs in its copy check; the commit must exist within 2 s (the committer's quiet time is 3 s, so a
#    commit that waits for it comes too late)
HMS_EMBED_URL="http://127.0.0.1:$PORT" "$T/hms" curate -submit "$T/sub.json" -p proj >"$T/out" 2>&1 &
P=$!
i=0
while [ $i -lt 20 ]; do
  git -C "$R" log -1 --format=%s 2>/dev/null | grep -q '^curate proj' && break
  i=$((i+1)); sleep 0.1
done
git -C "$R" log -1 --format=%s | grep -q '^curate proj' || { kill -9 "$P" 2>/dev/null; fail "no commit within 2 s of the start: the commit waits for the copy check"; }
kill -0 "$P" 2>/dev/null || fail "the submit ended before it could be killed (the embedder did not hold it)"
kill -9 "$P"; wait "$P" 2>/dev/null || true
git -C "$R" log -1 --format=%s | grep -q '^curate proj' || fail "no curate commit although the page was written"
[ -z "$(git -C "$R" status --porcelain -uall)" ] || fail "files left uncommitted after the kill: $(git -C "$R" status --porcelain -uall)"
git -C "$R" -c core.quotepath=false show --name-status --format= HEAD | grep -q 'Sauvegarde nocturne é.md' || fail "the page is not in the commit"
echo "ok: killed in the copy check, everything committed"

# 2. a run killed between writing and committing: the next writing command commits it
printf -- '---\nkind: learning\ncreated: 2020-01-03T10:00:00Z\nsource: learn\n---\n\nA third note that is saved and merged just before the kill.\n' > "$R/notes/proj/cccccccc-3333.md"
git -C "$R" add -A && git -C "$R" commit -q -m seed3
rm "$R/notes/proj/cccccccc-3333.md"
printf -- '---\ntitle: Étude\n---\n# Étude\n\nA third note that is saved and merged just before the kill.\n' > "$R/wiki/proj/Étude.md"
[ -n "$(git -C "$R" status --porcelain -uall)" ] || fail "test setup: nothing loose"
HMS_VECTORS=0 "$T/hms" wiki index -p proj >/dev/null
[ -z "$(git -C "$R" status --porcelain -uall)" ] || fail "leftovers not committed by the next command: $(git -C "$R" status --porcelain -uall)"
git -C "$R" -c core.quotepath=false show --name-status --format= HEAD~1 HEAD | grep -q 'Étude.md' || fail "the accented page is not in a commit"
echo "ok: leftovers of a killed run committed by the next command"
echo "PASS"

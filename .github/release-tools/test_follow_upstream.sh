#!/usr/bin/env bash
# Offline simulation of follow_upstream.sh against throwaway repositories and a
# recording `gh` stub. Usage: bash .github/release-tools/test_follow_upstream.sh
set -euo pipefail

SCRIPT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/follow_upstream.sh
ROOT=$(mktemp -d "${TMPDIR:-/tmp}/follow-upstream-test.XXXXXX")
trap 'rm -rf "$ROOT"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid

mkdir "$ROOT/bin"
cat >"$ROOT/bin/gh" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GH_LOG"
case "$1 $2" in
  'api repos/'*) echo "${STUB_LATEST_TAG:-}" ;;
  'issue list') echo "${STUB_ISSUE_NUMBER:-}" ;;
  'release delete') exit "${STUB_RELEASE_DELETE_STATUS:-0}" ;;
esac
STUB
chmod +x "$ROOT/bin/gh"
export PATH="$ROOT/bin:$PATH"

failures=0
check() { # description, command...
  local description=$1
  shift
  if "$@"; then echo "  ok    $description"; else echo "  FAIL  $description"; failures=$((failures + 1)); fi
}
commit() { # repo, file, content, message
  printf '%s\n' "$3" >"$1/$2"
  git -C "$1" add "$2"
  git -C "$1" commit --quiet -m "$4"
}
refs() { git -C "$1" for-each-ref --format='%(refname) %(objectname)'; }
logged() { grep -q -- "$1" "$GH_LOG"; }
contains() { [[ $1 == *"$2"* ]]; }

# upstream: base -> "upstream feature" (v1.0.0) -> "upstream edits base" (v1.1.0)
# origin:   base -> "fork patch"        (ours=patch)
#           base -> "fork edits base"   (ours=conflict, clashes with v1.1.0)
#           v1.0.0 -> "fork patch"      (ours=ontop)
fixture() { # name, ours
  S=$ROOT/$1
  mkdir -p "$S"
  export GH_LOG=$S/gh.log GITHUB_OUTPUT=$S/output
  : >"$GH_LOG"
  : >"$GITHUB_OUTPUT"
  git init --quiet --initial-branch=main "$S/seed"
  commit "$S/seed" base.txt base 'base'
  git -C "$S/seed" branch fork
  commit "$S/seed" up.txt feature 'upstream feature'
  git -C "$S/seed" tag --annotate --cleanup=whitespace -m 'Release v1.0.0' -m '## Highlights' -m '- upstream feature' v1.0.0
  commit "$S/seed" base.txt upstream-edit 'upstream edits base'
  git -C "$S/seed" tag --annotate -m 'Release v1.1.0' v1.1.0
  git clone --quiet --bare "$S/seed" "$S/upstream.git"

  if [[ $2 == ontop ]]; then git -C "$S/seed" checkout --quiet -B fork v1.0.0; else git -C "$S/seed" checkout --quiet fork; fi
  if [[ $2 == conflict ]]; then
    commit "$S/seed" base.txt fork-edit 'fork edits base'
  else
    commit "$S/seed" ours.txt patch 'fork patch'
  fi
  git init --quiet --bare --initial-branch=main "$S/origin.git"
  git -C "$S/seed" push --quiet "$S/origin.git" fork:main
  clone
}
clone() {
  rm -rf "$S/work"
  git clone --quiet "$S/origin.git" "$S/work"
}
run() { # extra VAR=value...
  status=0
  (cd "$S/work" && env UPSTREAM_URL="$S/upstream.git" TARGET_BRANCH=main GITHUB_REPOSITORY=me/fork \
    STUB_LATEST_TAG=v1.0.0 "$@" bash "$SCRIPT") >"$S/stdout" 2>&1 || status=$?
}
origin() { git -C "$S/origin.git" "$@"; }

echo '1a. new upstream tag, clean merge, pushed with GITHUB_TOKEN'
fixture clean-token patch
run
check 'exits 0' test "$status" -eq 0
check 'main is a merge commit' test "$(origin rev-list --parents -n1 main | wc -w)" -eq 3
check 'merge subject' test "$(origin log -1 --format=%s main)" = 'merge: upstream v1.0.0'
check 'tag is annotated' test "$(origin cat-file -t refs/tags/v1.0.0)" = tag
check 'tag points at main' test "$(origin rev-parse 'v1.0.0^{commit}')" = "$(origin rev-parse main)"
message=$(origin for-each-ref --format='%(contents)' refs/tags/v1.0.0)
check 'tag keeps the upstream subject' contains "$message" 'Release v1.0.0'
check 'tag keeps upstream Markdown headings' contains "$message" '## Highlights'
check 'tag lists fork patches' contains "$message" '- fork patch'
check 'upstream commits are not listed as fork patches' test "$(grep -c 'upstream feature' <<<"$message")" -eq 1
check 'no upstream-tags ref on origin' test -z "$(origin for-each-ref refs/upstream-tags)"
check 'only the followed tag on origin' test "$(origin tag | tr '\n' ' ')" = 'v1.0.0 '
check 'release.yml dispatched' logged 'workflow run release.yml --repo me/fork --ref main -f tag=v1.0.0'
check 'outputs' test "$(sort "$GITHUB_OUTPUT" | cut -d= -f1,2 | grep -v '^sha=' | tr '\n' ' ')" = 'merged=true released=true tag=v1.0.0 '

echo '2. second run'
before=$(refs "$S/origin.git")
: >"$GH_LOG"
: >"$GITHUB_OUTPUT"
clone
run
check 'exits 0' test "$status" -eq 0
check 'says up to date' grep -q 'up to date' "$S/stdout"
check 'released=false' grep -qx 'released=false' "$GITHUB_OUTPUT"
check 'origin untouched' test "$(refs "$S/origin.git")" = "$before"
check 'nothing dispatched' test "$(grep -c 'workflow run' "$GH_LOG")" -eq 0

echo '6. rebuild moves the tag to the new head'
commit "$S/work" ours2.txt patch2 'second fork patch'
git -C "$S/work" push --quiet origin main
old_tag=$(origin rev-parse refs/tags/v1.0.0)
: >"$GH_LOG"
: >"$GITHUB_OUTPUT"
clone
run REQUESTED_TAG=v1.0.0 REBUILD=true
check 'exits 0' test "$status" -eq 0
check 'old release deleted' logged 'release delete v1.0.0 --repo me/fork --cleanup-tag --yes'
check 'tag object replaced' test "$(origin rev-parse refs/tags/v1.0.0)" != "$old_tag"
check 'tag points at the new head' test "$(origin rev-parse 'v1.0.0^{commit}')" = "$(origin rev-parse main)"
check 'no new merge' grep -qx 'merged=false' "$GITHUB_OUTPUT"
check 'notes list both patches' test "$(origin for-each-ref --format='%(contents)' refs/tags/v1.0.0 | grep -c 'fork patch')" -eq 2
check 'release.yml dispatched' logged 'workflow run release.yml'

echo '1b. clean merge pushed with a PAT'
fixture clean-pat patch
run PUSH_TRIGGERS_WORKFLOWS=true SIMPLE_RELEASE=true
check 'exits 0' test "$status" -eq 0
check 'tag pushed' test "$(origin rev-parse 'v1.0.0^{commit}')" = "$(origin rev-parse main)"
check 'release.yml not dispatched' test "$(grep -c 'workflow run' "$GH_LOG")" -eq 0

echo '1c. GITHUB_TOKEN path forwards simple_release'
fixture clean-simple patch
run SIMPLE_RELEASE=true
check 'dispatch carries simple_release' logged '-f tag=v1.0.0 -f simple_release=true'

echo '3. upstream tag already merged'
fixture ancestor ontop
head_before=$(origin rev-parse main)
run
check 'exits 0' test "$status" -eq 0
check 'no merge commit' test "$(origin rev-parse main)" = "$head_before"
check 'tag created at head' test "$(origin rev-parse 'v1.0.0^{commit}')" = "$head_before"
check 'merged=false' grep -qx 'merged=false' "$GITHUB_OUTPUT"

echo '4. conflicting upstream change'
fixture conflict conflict
before=$(refs "$S/origin.git")
run STUB_LATEST_TAG=v1.1.0
check 'exits 1' test "$status" -eq 1
check 'origin untouched' test "$(refs "$S/origin.git")" = "$before"
check 'issue filed' logged 'issue create --repo me/fork --title Upstream v1.1.0 needs a manual merge'
# shellcheck disable=SC2016 # literal Markdown backticks
check 'issue names the conflicted file' logged '`base.txt`'
check 'merge aborted' test -z "$(git -C "$S/work" status --porcelain)"
: >"$GH_LOG"
clone
run STUB_LATEST_TAG=v1.1.0 STUB_ISSUE_NUMBER=7
check 'an open issue gets a comment instead of a duplicate' logged 'issue comment 7 --repo me/fork'
check 'no second issue' test "$(grep -c 'issue create' "$GH_LOG")" -eq 0

echo '5. verification fails'
fixture verify patch
before=$(refs "$S/origin.git")
run VERIFY_CMD=false
check 'exits 1' test "$status" -eq 1
check 'origin untouched' test "$(refs "$S/origin.git")" = "$before"
check 'issue filed' logged 'issue create --repo me/fork --title Upstream v1.0.0 failed verification'
: >"$GH_LOG"
clone
run VERIFY_CMD='test -f up.txt && test -f ours.txt'
check 'verification sees the merged tree and passes' test "$status" -eq 0

echo '7. dry run'
fixture dry patch
before=$(refs "$S/origin.git")
run DRY_RUN=true
check 'exits 0' test "$status" -eq 0
check 'origin untouched' test "$(refs "$S/origin.git")" = "$before"
check 'gh only resolved the tag' test "$(grep -vc '^api ' "$GH_LOG")" -eq 0
check 'released=false' grep -qx 'released=false' "$GITHUB_OUTPUT"
run DRY_RUN=true STUB_LATEST_TAG=v1.1.0 VERIFY_CMD=false
check 'a failing dry run files no issue' test "$status" -eq 1 -a "$(grep -c '^issue ' "$GH_LOG")" -eq 0

echo '8. tags that are not plain releases are refused'
fixture badtag patch
before=$(refs "$S/origin.git")
for bad in v1.0.0-rc.1 1.0.0 'v1.0.0;id' ''; do
  run REQUESTED_TAG="$bad" STUB_LATEST_TAG="$bad"
  check "refuses '$bad'" test "$status" -eq 1
done
check 'origin untouched' test "$(refs "$S/origin.git")" = "$before"

echo
if [[ $failures -gt 0 ]]; then echo "$failures check(s) failed"; exit 1; fi
echo 'all checks passed'

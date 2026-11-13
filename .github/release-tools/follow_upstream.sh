#!/usr/bin/env bash
# Bring one upstream release into this fork: merge its tag into the maintained
# branch, verify the result, tag it under the same name and let release.yml
# publish it. Safe to re-run; nothing is pushed until merge and verify pass.
set -euo pipefail

UPSTREAM_REPO=${UPSTREAM_REPO:-ranxi2001/sub2api}
UPSTREAM_URL=${UPSTREAM_URL:-https://github.com/$UPSTREAM_REPO.git}
: "${TARGET_BRANCH:?TARGET_BRANCH is required}" "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
REBUILD=${REBUILD:-false}
DRY_RUN=${DRY_RUN:-false}
PUSH_TRIGGERS_WORKFLOWS=${PUSH_TRIGGERS_WORKFLOWS:-false}
VERSION_FILE=${VERSION_FILE:-backend/cmd/server/VERSION}
BOT=(-c 'user.name=github-actions[bot]' -c 'user.email=41898282+github-actions[bot]@users.noreply.github.com')

output() {
  echo "$1=$2"
  if [[ -n ${GITHUB_OUTPUT:-} ]]; then echo "$1=$2" >>"$GITHUB_OUTPUT"; fi
}

# The run log always gets the full text: forks have Issues disabled by default,
# so filing one is best effort and must not replace the real failure.
report() {
  local title=$1 body=$2 number=''
  echo "::error::$title"
  printf '%s\n' "$body"
  if [[ $DRY_RUN == true ]]; then return 0; fi
  number=$(gh issue list --repo "$GITHUB_REPOSITORY" --state open --search "\"$title\" in:title" \
    --json number,title --jq "map(select(.title == \"$title\")) | .[0].number // empty" 2>/dev/null) || number=''
  if [[ -n $number ]]; then
    gh issue comment "$number" --repo "$GITHUB_REPOSITORY" --body "$body" >/dev/null
  else
    gh issue create --repo "$GITHUB_REPOSITORY" --title "$title" --body "$body" >/dev/null
  fi || echo "::warning::could not file an issue (are Issues enabled on $GITHUB_REPOSITORY?)"
}

tag=${REQUESTED_TAG:-}
if [[ -z $tag ]]; then
  # releases/latest never returns a draft or a prerelease.
  tag=$(gh api "repos/$UPSTREAM_REPO/releases/latest" --jq .tag_name)
fi
if [[ ! $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "::error::refusing to follow '$tag': not a vX.Y.Z release tag" >&2
  exit 1
fi
output tag "$tag"

tag_on_origin=false
status=0
git ls-remote --exit-code --tags origin "refs/tags/$tag" >/dev/null || status=$?
case $status in
  0) tag_on_origin=true ;;
  2) ;;
  *) echo "::error::cannot list tags on origin" >&2; exit 1 ;;
esac
# The tag alone does not prove a release: a run can push it and then fail to
# start release.yml. Only a published release, or one being published, counts.
if [[ $tag_on_origin == true && $REBUILD != true ]]; then
  if gh release view "$tag" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then
    echo "up to date: $tag is already released from $GITHUB_REPOSITORY"
    output released false
    exit 0
  fi
  publishing=$(gh run list --repo "$GITHUB_REPOSITORY" --workflow release.yml --limit 20 \
    --json status --jq 'map(select(.status != "completed")) | length' 2>/dev/null) || publishing=0
  if [[ ${publishing:-0} -gt 0 ]]; then
    echo "waiting: $tag is being published by release.yml"
    output released false
    exit 0
  fi
  echo "resuming: $tag was pushed but never published"
fi

# Upstream tags stay out of refs/tags: every v* tag pushed there would start a
# release, and ours has to point at the merged commit, not at upstream's.
upstream_ref=refs/upstream-tags/$tag
git fetch --quiet --no-tags "$UPSTREAM_URL" "+refs/tags/$tag:$upstream_ref"
upstream_commit=$(git rev-parse --verify "$upstream_ref^{commit}")

git checkout --quiet "$TARGET_BRANCH"
merged=false
if ! git merge-base --is-ancestor "$upstream_commit" HEAD; then
  if ! git "${BOT[@]}" merge --no-ff --no-edit -m "merge: upstream $tag" "$upstream_commit"; then
    conflicted=$(git diff --name-only --diff-filter=U)
    if [[ $conflicted != "$VERSION_FILE" ]]; then
      # shellcheck disable=SC2016 # literal Markdown backticks
      conflicts=$(git diff --name-only --diff-filter=U | sed 's/.*/- `&`/')
      git merge --abort 2>/dev/null || true
      report "Upstream $tag needs a manual merge" "Merging \`$tag\` from $UPSTREAM_REPO into \`$TARGET_BRANCH\` stopped on conflicts:

${conflicts:-(git reported no conflicted paths; see the run log)}

Resolve them locally and push the merge. The next run tags and publishes it.

\`\`\`sh
git fetch origin && git checkout $TARGET_BRANCH && git merge --ff-only origin/$TARGET_BRANCH
git fetch $UPSTREAM_URL '+refs/tags/$tag:$upstream_ref'
git merge --no-ff -m 'merge: upstream $tag' '$upstream_ref^{commit}'
# fix the conflicts, then
git commit --no-edit && git push origin $TARGET_BRANCH
\`\`\`"
      exit 1
    fi
    # release.yml stamps this file from the tag at build time and commits the
    # result back, so both sides bump it and its value on the branch is unused.
    git checkout --theirs -- "$VERSION_FILE"
    git add -- "$VERSION_FILE"
    git "${BOT[@]}" commit --quiet --no-edit
  fi
  merged=true
fi

if [[ -n ${VERIFY_CMD:-} ]]; then
  echo "verifying: $VERIFY_CMD"
  if ! bash -euo pipefail -c "$VERIFY_CMD"; then
    report "Upstream $tag failed verification" "\`$TARGET_BRANCH\` with \`$tag\` from $UPSTREAM_REPO merged in does not pass:

\`\`\`sh
$VERIFY_CMD
\`\`\`

Nothing was pushed. Fix the fork patches against \`$tag\`, push \`$TARGET_BRANCH\`, and the next run publishes it."
    exit 1
  fi
fi

# release.yml publishes this tag's body as the release notes.
subject=$(git for-each-ref --format='%(contents:subject)' "$upstream_ref")
body=$(git for-each-ref --format='%(contents:body)' "$upstream_ref")
ours=$(git log --no-merges --format='- %s' "$upstream_commit..HEAD" | grep -v '^- chore: sync VERSION to ' || true)
notes=$(mktemp)
trap 'rm -f "$notes"' EXIT
{
  printf '%s\n\n' "${subject:-Sub2API $tag}"
  if [[ -n $body ]]; then printf '%s\n\n' "$body"; fi
  if [[ -n $ours ]]; then printf '## Fork patches on top of upstream %s\n\n%s\n' "$tag" "$ours"; fi
} >"$notes"
# The default cleanup mode would drop every Markdown heading as a comment line.
git "${BOT[@]}" tag --force --annotate --cleanup=whitespace --file "$notes" "$tag" HEAD

sha=$(git rev-parse HEAD)
output merged "$merged"
output sha "$sha"
if [[ $DRY_RUN == true ]]; then
  echo "dry run: would push $TARGET_BRANCH and $tag at $sha"
  output released false
  exit 0
fi

if [[ $tag_on_origin == true ]]; then
  # A rebuild replaces the release; one that never got published is fine.
  gh release delete "$tag" --repo "$GITHUB_REPOSITORY" --cleanup-tag --yes || true
  # Pushing an unchanged tag starts nothing, so the old one always goes first.
  git push origin ":refs/tags/$tag" 2>/dev/null || true
fi
if ! git push origin "HEAD:refs/heads/$TARGET_BRANCH"; then
  if [[ $PUSH_TRIGGERS_WORKFLOWS != true ]]; then
    # GITHUB_TOKEN can never be granted the workflows permission, and most
    # upstream releases change files under .github/workflows.
    echo "::error::push rejected. If the merge touches .github/workflows, set the UPSTREAM_SYNC_SSH_KEY secret (private half of a write deploy key)." >&2
  fi
  exit 1
fi
git push origin "refs/tags/$tag"

if [[ $PUSH_TRIGGERS_WORKFLOWS != true ]]; then
  # Pushes made with GITHUB_TOKEN start no workflows, so release.yml has to be
  # dispatched. A PAT push already started it through the tag.
  dispatch=(gh workflow run release.yml --repo "$GITHUB_REPOSITORY" --ref "$TARGET_BRANCH" -f "tag=$tag")
  if [[ ${SIMPLE_RELEASE:-false} == true ]]; then dispatch+=(-f simple_release=true); fi
  "${dispatch[@]}"
fi
output released true

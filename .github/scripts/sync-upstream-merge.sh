#!/usr/bin/env bash
# Create a local ordinary merge of one upstream commit onto BASE_REF.
# Does not fetch, push, or talk to GitHub. Exit 2 reports conflicts and
# leaves the worktree free of conflict markers.
set -euo pipefail

OUTPUT_FILE="${OUTPUT_FILE:-}"
if [[ -n "$OUTPUT_FILE" ]]; then
  : >"$OUTPUT_FILE"
fi

emit() {
  printf '%s\n' "$1"
  if [[ -n "$OUTPUT_FILE" ]]; then
    printf '%s\n' "$1" >>"$OUTPUT_FILE"
  fi
}

fail() {
  echo "::error::$*" >&2
  exit 1
}

channel="${UPSTREAM_CHANNEL:-}"
tag="${UPSTREAM_TAG:-}"
sha="${UPSTREAM_SHA:-}"
base_ref="${BASE_REF:-}"

[[ -n "$channel" ]] || fail "UPSTREAM_CHANNEL is required"
[[ -n "$tag" ]] || fail "UPSTREAM_TAG is required"
[[ -n "$sha" ]] || fail "UPSTREAM_SHA is required"
[[ -n "$base_ref" ]] || fail "BASE_REF is required"
[[ "$sha" =~ ^[0-9a-f]{40}$ ]] || fail "UPSTREAM_SHA must be 40 lowercase hex characters"
[[ "$channel" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-rc$ ]] || fail "UPSTREAM_CHANNEL must be an RC series prefix such as v1.0.0-rc"

escaped=$(printf '%s' "$channel" | sed -e 's/[.[\*^$()+?{|\\]/\\&/g')
[[ "$tag" =~ ^${escaped}\.[0-9]+$ ]] || fail "tag ${tag} is not on channel ${channel}"

git rev-parse --is-inside-work-tree >/dev/null
[[ -z "$(git status --porcelain)" ]] || fail "worktree is dirty"

base_sha=$(git rev-parse "${base_ref}^{commit}")
git cat-file -e "${sha}^{commit}" || fail "upstream commit ${sha} is not available locally"

if git merge-base --is-ancestor "$sha" "$base_sha"; then
  emit "status=skipped"
  emit "reason=already-contained"
  emit "base_sha=${base_sha}"
  emit "upstream_sha=${sha}"
  emit "upstream_tag=${tag}"
  emit "upstream_channel=${channel}"
  exit 0
fi

branch="sync/upstream/${sha}"
export GIT_AUTHOR_NAME="NeedToken Sync"
export GIT_AUTHOR_EMAIL="needtoken-sync@users.noreply.github.com"
export GIT_COMMITTER_NAME="$GIT_AUTHOR_NAME"
export GIT_COMMITTER_EMAIL="$GIT_AUTHOR_EMAIL"

git checkout -q -B "$branch" "$base_sha"

cleanup_failed_branch() {
  git -c advice.detachedHead=false checkout -q --force "$base_sha"
  git branch -D "$branch" >/dev/null 2>&1 || true
}

set +e
git -c commit.gpgsign=false merge --no-ff --no-edit -m "Merge upstream ${tag} (${sha})" "$sha"
merge_code=$?
set -e

if [[ "$merge_code" -ne 0 ]]; then
  if [[ -n "$(git ls-files -u)" ]]; then
    emit "status=conflict"
    emit "base_sha=${base_sha}"
    emit "upstream_sha=${sha}"
    emit "upstream_tag=${tag}"
    emit "upstream_channel=${channel}"
    while IFS= read -r file; do
      [[ -z "$file" ]] && continue
      emit "conflict_file=${file}"
    done < <(git diff --name-only --diff-filter=U | sort -u)
    git merge --abort
    cleanup_failed_branch
    exit 2
  fi
  git merge --abort >/dev/null 2>&1 || true
  cleanup_failed_branch
  fail "git merge failed without unmerged files"
fi

[[ -z "$(git ls-files -u)" ]] || fail "merge left unmerged index entries"
[[ "$(git rev-parse 'HEAD^1')" == "$base_sha" ]] || fail "merge first parent is not the base"
[[ "$(git rev-parse 'HEAD^2')" == "$sha" ]] || fail "merge second parent is not the upstream commit"

emit "status=clean"
emit "branch=${branch}"
emit "base_sha=${base_sha}"
emit "upstream_sha=${sha}"
emit "upstream_tag=${tag}"
emit "upstream_channel=${channel}"
emit "merge_sha=$(git rev-parse HEAD)"

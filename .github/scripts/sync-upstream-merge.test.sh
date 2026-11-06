#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$root/sync-upstream-merge.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

if grep -n -E 'git push|--squash|-X ours|-X theirs|strategy-option=ours|strategy-option=theirs' "$script"; then
  fail "merge script must not push or choose ours/theirs/squash"
fi

gitc() {
  git -C "$1" -c user.name=Test -c user.email=test@example.com -c commit.gpgsign=false "${@:2}"
}

init_repo() {
  local repo="$1"
  git init -b main "$repo" >/dev/null
  git -C "$repo" config commit.gpgsign false
}

run_merge() {
  local repo="$1"
  shift
  (
    cd "$repo"
    env "$@" bash "$script"
  )
}

clean_repo="$tmp/clean"
init_repo "$clean_repo"
printf 'base\n' >"$clean_repo/file.txt"
printf '<<<<<<< not a conflict\n' >"$clean_repo/notes.txt"
git -C "$clean_repo" add file.txt notes.txt
gitc "$clean_repo" commit -m base >/dev/null
base_sha=$(git -C "$clean_repo" rev-parse HEAD)
printf 'fork\n' >"$clean_repo/fork.txt"
git -C "$clean_repo" add fork.txt
gitc "$clean_repo" commit -m fork >/dev/null
main_sha=$(git -C "$clean_repo" rev-parse HEAD)
git -C "$clean_repo" checkout -b upstream "$base_sha" >/dev/null
printf 'upstream\n' >"$clean_repo/upstream.txt"
git -C "$clean_repo" add upstream.txt
gitc "$clean_repo" commit -m upstream >/dev/null
upstream_sha=$(git -C "$clean_repo" rev-parse HEAD)
git -C "$clean_repo" checkout main >/dev/null

out="$tmp/clean.out"
run_merge "$clean_repo" \
  OUTPUT_FILE="$out" \
  BASE_REF=main \
  UPSTREAM_CHANNEL=v1.0.0-rc \
  UPSTREAM_TAG=v1.0.0-rc.4 \
  UPSTREAM_SHA="$upstream_sha" >/dev/null
grep -q '^status=clean$' "$out" || fail "clean merge status"
[[ "$(git -C "$clean_repo" rev-parse 'HEAD^1')" == "$main_sha" ]] || fail "first parent"
[[ "$(git -C "$clean_repo" rev-parse 'HEAD^2')" == "$upstream_sha" ]] || fail "second parent"
[[ "$(git -C "$clean_repo" branch --show-current)" == "sync/upstream/${upstream_sha}" ]] || fail "branch name"
grep -q '<<<<<<< not a conflict' "$clean_repo/notes.txt" || fail "literal marker text was rewritten"
[[ -f "$clean_repo/fork.txt" && -f "$clean_repo/upstream.txt" ]] || fail "both sides missing"
[[ -z "$(git -C "$clean_repo" ls-files -u)" ]] || fail "unmerged entries after clean merge"
author=$(git -C "$clean_repo" log -1 --format='%an <%ae>')
[[ "$author" == "NeedToken Sync <needtoken-sync@users.noreply.github.com>" ]] || fail "author [$author]"

conflict_repo="$tmp/conflict"
init_repo "$conflict_repo"
printf 'base\n' >"$conflict_repo/file.txt"
git -C "$conflict_repo" add file.txt
gitc "$conflict_repo" commit -m base >/dev/null
base_sha=$(git -C "$conflict_repo" rev-parse HEAD)
printf 'fork\n' >"$conflict_repo/file.txt"
git -C "$conflict_repo" add file.txt
gitc "$conflict_repo" commit -m fork >/dev/null
git -C "$conflict_repo" checkout -b upstream "$base_sha" >/dev/null
printf 'upstream\n' >"$conflict_repo/file.txt"
git -C "$conflict_repo" add file.txt
gitc "$conflict_repo" commit -m upstream >/dev/null
upstream_sha=$(git -C "$conflict_repo" rev-parse HEAD)
git -C "$conflict_repo" checkout main >/dev/null
out="$tmp/conflict.out"
set +e
run_merge "$conflict_repo" \
  OUTPUT_FILE="$out" \
  BASE_REF=main \
  UPSTREAM_CHANNEL=v1.0.0-rc \
  UPSTREAM_TAG=v1.0.0-rc.5 \
  UPSTREAM_SHA="$upstream_sha" >/dev/null
code=$?
set -e
[[ "$code" -eq 2 ]] || fail "conflict exit code $code"
grep -q '^status=conflict$' "$out" || fail "conflict status"
grep -q "^base_sha=$(git -C "$conflict_repo" rev-parse HEAD)$" "$out" || fail "conflict base sha"
grep -q "^upstream_sha=${upstream_sha}$" "$out" || fail "conflict upstream sha"
grep -q '^conflict_file=file.txt$' "$out" || fail "conflict file"
[[ "$(cat "$conflict_repo/file.txt")" == "fork" ]] || fail "conflict markers were left behind"
[[ -z "$(git -C "$conflict_repo" status --porcelain)" ]] || fail "conflict worktree dirty"
if git -C "$conflict_repo" show-ref --verify --quiet "refs/heads/sync/upstream/${upstream_sha}"; then
  fail "conflict branch was kept"
fi

skip_repo="$tmp/skip"
init_repo "$skip_repo"
printf 'base\n' >"$skip_repo/file.txt"
git -C "$skip_repo" add file.txt
gitc "$skip_repo" commit -m base >/dev/null
old_sha=$(git -C "$skip_repo" rev-parse HEAD)
printf 'later\n' >"$skip_repo/later.txt"
git -C "$skip_repo" add later.txt
gitc "$skip_repo" commit -m later >/dev/null
out="$tmp/skip.out"
run_merge "$skip_repo" \
  OUTPUT_FILE="$out" \
  BASE_REF=main \
  UPSTREAM_CHANNEL=v1.0.0-rc \
  UPSTREAM_TAG=v1.0.0-rc.1 \
  UPSTREAM_SHA="$old_sha" >/dev/null
grep -q '^status=skipped$' "$out" || fail "skip status"
grep -q '^reason=already-contained$' "$out" || fail "skip reason"
[[ "$(git -C "$skip_repo" branch --show-current)" == "main" ]] || fail "skip moved off main"
if git -C "$skip_repo" show-ref --verify --quiet "refs/heads/sync/upstream/${old_sha}"; then
  fail "skip created a branch"
fi

stable_repo="$tmp/stable"
init_repo "$stable_repo"
printf 'base\n' >"$stable_repo/file.txt"
git -C "$stable_repo" add file.txt
gitc "$stable_repo" commit -m base >/dev/null
sha=$(git -C "$stable_repo" rev-parse HEAD)
set +e
run_merge "$stable_repo" \
  BASE_REF=main \
  UPSTREAM_CHANNEL=v1.0.0-rc \
  UPSTREAM_TAG=v0.13.2 \
  UPSTREAM_SHA="$sha" >/dev/null
code=$?
set -e
[[ "$code" -eq 1 ]] || fail "stable tag exit $code"

echo "sync-upstream-merge.test.sh ok"

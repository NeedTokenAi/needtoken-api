#!/usr/bin/env bash
# Resolve the QuantumNous/new-api RC series without mixing in stable, alpha,
# nightly, or suffixed hotfix tags. GitHub marks current RC releases as
# prerelease=false, so this script does not trust that flag.
set -euo pipefail

select_tag() {
  local channel escaped tag n best_n best_tag canonical
  channel="${UPSTREAM_CHANNEL:-v1.0.0-rc}"
  if [[ ! "$channel" =~ ^v[0-9]+\.[0-9]+\.[0-9]+-rc$ ]]; then
    echo "UPSTREAM_CHANNEL must be an RC series prefix such as v1.0.0-rc (got: ${channel})" >&2
    exit 1
  fi
  escaped=$(printf '%s' "$channel" | sed -e 's/[.[\*^$()+?{|\\]/\\&/g')
  best_n=-1
  best_tag=""
  while IFS= read -r tag || [[ -n "$tag" ]]; do
    tag="${tag//$'\r'/}"
    tag="${tag#"${tag%%[![:space:]]*}"}"
    tag="${tag%"${tag##*[![:space:]]}"}"
    [[ -z "$tag" ]] && continue
    if [[ "$tag" =~ ^${escaped}\.([0-9]+)$ ]]; then
      n=$((10#${BASH_REMATCH[1]}))
      canonical="${channel}.${n}"
      if [[ "$n" -gt "$best_n" ]]; then
        best_n=$n
        best_tag=$tag
      elif [[ "$n" -eq "$best_n" && "$tag" == "$canonical" ]]; then
        # v1.0.0-rc.8 and v1.0.0-rc.08 are the same number; keep the tag we can fetch.
        best_tag=$tag
      fi
    fi
  done
  if [[ -z "$best_tag" ]]; then
    echo "no upstream tag matches channel ${channel}" >&2
    exit 1
  fi
  printf '%s\n' "$best_tag"
}

peel_sha() {
  local line sha ref peeled="" raw="" count=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line//$'\r'/}"
    [[ -z "$line" ]] && continue
    if [[ ! "$line" =~ ^([0-9a-f]{40})[[:space:]]+([^[:space:]]+)$ ]]; then
      echo "unrecognized git ls-remote line" >&2
      exit 1
    fi
    sha="${BASH_REMATCH[1]}"
    ref="${BASH_REMATCH[2]}"
    count=$((count + 1))
    if [[ "$ref" == *'^{}' ]]; then
      peeled=$sha
    else
      raw=$sha
    fi
  done
  if [[ -n "$peeled" ]]; then
    printf '%s\n' "$peeled"
    return 0
  fi
  if [[ -n "$raw" && "$count" -eq 1 ]]; then
    printf '%s\n' "$raw"
    return 0
  fi
  echo "could not peel the upstream tag to one commit" >&2
  exit 1
}

case "${1:-}" in
  select-tag) select_tag ;;
  peel-sha) peel_sha ;;
  *)
    echo "usage: resolve-upstream-channel.sh select-tag|peel-sha" >&2
    exit 1
    ;;
esac

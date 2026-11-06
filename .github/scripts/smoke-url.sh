#!/usr/bin/env bash
# Smoke public NeedToken endpoints. curl --compressed is required because
# /api routes gzip their JSON bodies.
set -euo pipefail

base="${1:-}"
if [[ -z "$base" ]]; then
  echo "::error::smoke base URL is empty" >&2
  exit 1
fi
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

for path in /api/status /api/pricing; do
  body=$(mktemp)
  set +e
  code=$(curl --compressed -sS -o "$body" -w '%{http_code}' \
    --retry 8 --retry-delay 5 --retry-all-errors --max-time 25 \
    "${base%/}${path}")
  curl_code=$?
  set -e
  if [[ "$curl_code" -ne 0 ]]; then
    echo "::error::curl ${path} failed" >&2
    exit 1
  fi
  if [[ "$code" != "200" ]]; then
    echo "::error::${path} returned HTTP ${code}" >&2
    head -c 300 "$body" >&2 || true
    echo >&2
    exit 1
  fi
  python3 "$script_dir/require-success-json.py" "$body"
  echo "ok ${path}"
done

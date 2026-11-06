#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$root/require-success-json.py"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

printf '%s' '{"success":true,"data":[]}' >"$tmp/ok.json"
python3 "$script" "$tmp/ok.json"

printf '%s' '{"success": true, "message": ""}' >"$tmp/spaced.json"
python3 "$script" "$tmp/spaced.json"

for body in '{"success":false}' '{"success":"true"}' '{"success":1}' '[]' 'not-json' ''; do
  printf '%s' "$body" >"$tmp/bad.json"
  if python3 "$script" "$tmp/bad.json" >/dev/null 2>&1; then
    fail "accepted body [$body]"
  fi
done

echo "require-success-json.test.sh ok"

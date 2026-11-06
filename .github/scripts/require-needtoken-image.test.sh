#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$root/require-needtoken-image.sh"
good="asia-southeast1-docker.pkg.dev/suichong/needtoken/needtoken-api"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

got=$(bash "$script" "$good")
[[ "$got" == "$good" ]] || fail "expected the allowed image path"

for bad in \
  "" \
  "calciumion/new-api" \
  "calciumion/new-api:latest" \
  "docker.io/calciumion/new-api:latest" \
  "asia-southeast1-docker.pkg.dev/suichong/needtoken/needtoken-api:latest" \
  "asia-southeast1-docker.pkg.dev/suichong/needtoken/needtoken-api@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" \
  "asia-southeast1-docker.pkg.dev/other/needtoken/needtoken-api" \
  "us-central1-docker.pkg.dev/suichong/needtoken/needtoken-api" \
  "asia-southeast1-docker.pkg.dev/suichong/NeedToken/needtoken-api" \
  "asia-southeast1-docker.pkg.dev/suichong/needtoken/../needtoken-api" \
  "asia-southeast1-docker.pkg.dev/suichong/calciumion/needtoken-api"; do
  if bash "$script" "$bad" >/dev/null 2>&1; then
    fail "accepted forbidden image [$bad]"
  fi
done

echo "require-needtoken-image.test.sh ok"

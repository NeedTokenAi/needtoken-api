#!/usr/bin/env bash
# Accept only a NeedToken image path in the suichong Artifact Registry.
# Tags and digests are applied by the deploy workflow, never taken from this input.
set -euo pipefail

image="${1:-}"
if [[ -z "$image" ]]; then
  echo "::error::image path is empty" >&2
  exit 1
fi
if [[ "$image" == *calciumion* || "$image" == *docker.io* || "$image" == *":latest" ]]; then
  echo "::error::refusing official or floating image ${image}" >&2
  exit 1
fi
if [[ ! "$image" =~ ^asia-southeast1-docker\.pkg\.dev/suichong/[a-z0-9]+([._-][a-z0-9]+)*/[a-z0-9]+([._-][a-z0-9]+)*$ ]]; then
  echo "::error::GCP_CLOUD_RUN_IMAGE must be a tagless image path in asia-southeast1-docker.pkg.dev/suichong" >&2
  exit 1
fi
printf '%s\n' "$image"

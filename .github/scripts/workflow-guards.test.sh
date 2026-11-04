#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_file() {
  local file="$1" needle="$2"
  grep -q -F -- "$needle" "$file" || fail "$file missing [$needle]"
}

forbid_file() {
  local file="$1" needle="$2"
  if grep -q -F -- "$needle" "$file"; then
    fail "$file contains forbidden [$needle]"
  fi
}

deploy=".github/workflows/deploy-cloudrun.yml"
sync=".github/workflows/sync-upstream.yml"
ci=".github/workflows/ci.yml"

require_file "$ci" "branches: [main]"
require_file "$ci" "go test ./model ./middleware ./controller ./router -count=1 -timeout=180s"
require_file "$ci" "NeedToken fulfillment databases"
require_file "$ci" "Ops script checks"
forbid_file "$ci" "id-token:"
forbid_file "$ci" "pull_request_target"

require_file "$deploy" 'workflows: ["CI"]'
require_file "$deploy" "types: [completed]"
require_file "$deploy" "git_sha:"
require_file "$deploy" "--no-traffic"
require_file "$deploy" "--tag=candidate"
require_file "$deploy" "--to-revisions"
require_file "$deploy" "api.needtoken.ai"
require_file "$deploy" "needtoken-api"
require_file "$deploy" "suichong"
require_file "$deploy" "asia-southeast1"
require_file "$deploy" "GCP_WORKLOAD_IDENTITY_PROVIDER"
require_file "$deploy" "GCP_SERVICE_ACCOUNT"
require_file "$deploy" "needs.resolve.outputs.sha"
require_file "$deploy" "require-needtoken-image.sh"
require_file "$deploy" "smoke-url.sh"
forbid_file "$deploy" "pull_request_target"
forbid_file "$deploy" "calciumion/new-api"
forbid_file "$deploy" "credentials_json"
forbid_file "$deploy" "service_account_key"
forbid_file "$deploy" "--set-env-vars"
forbid_file "$deploy" "--env-vars-file"
forbid_file "$deploy" "--set-secrets"
forbid_file "$deploy" "--clear-env-vars"
forbid_file "$deploy" "--remove-env-vars"
forbid_file "$deploy" "--update-env-vars"
forbid_file "$deploy" "--update-secrets"
forbid_file "$deploy" "git push"
forbid_file "$deploy" "SYNC_GITHUB_TOKEN"
forbid_file "$deploy" ":latest"

id_token_count=$(grep -c -F 'id-token: write' "$deploy")
[[ "$id_token_count" -eq 1 ]] || fail "deploy workflow should grant id-token once, got $id_token_count"

require_file "$sync" 'cron: "0 0 * * *"'
require_file "$sync" "UPSTREAM_CHANNEL"
require_file "$sync" "SYNC_GITHUB_TOKEN"
require_file "$sync" "sync/upstream/"
require_file "$sync" "QuantumNous/new-api"
require_file "$sync" 'refs/heads/${branch}:refs/heads/${branch}'
require_file "$sync" "git fetch --no-tags"
require_file "$sync" "contents: read"
forbid_file "$sync" "id-token:"
forbid_file "$sync" "google-github-actions"
forbid_file "$sync" "GCP_"
forbid_file "$sync" "--squash"
forbid_file "$sync" "-X ours"
forbid_file "$sync" "-X theirs"
forbid_file "$sync" "pull_request_target"
forbid_file "$sync" "calciumion"

if grep -n 'git push' "$sync" | grep -E -- '--tags|--all|refs/tags'; then
  fail "sync workflow push must not publish tags"
fi

echo "workflow-guards.test.sh ok"

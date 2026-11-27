#!/usr/bin/env bash
# 校验 needtoken-api 上游同步脚本、稳定版底线过滤、高危路径判定与自动合并门禁。
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=../lib/stable-tag.sh
source "$root/lib/stable-tag.sh"
# shellcheck source=../lib/high-risk-paths.sh
source "$root/lib/high-risk-paths.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

# 1. stable-tag + v1.0.0 版本底线保护（避免把老的 v0.13.2 误合到 v1.0.0-rc.47 上）
if printf '%s\n' v0.13.2 v1.0.0-rc.47 v1.0.0-rc.51 | select_stable_tag v1.0.0 | grep -q .; then
  fail "v0.13.2 and rc tags must not match min_tag v1.0.0"
fi
got=$(printf '%s\n' v0.13.2 v1.0.0-rc.51 v1.0.0 v1.0.2-rc.1 | select_stable_tag v1.0.0)
[[ $got == v1.0.0 ]] || fail "expected v1.0.0, got $got"

# 2. 高危路径分类器
for p in \
  ".github/workflows/ci.yml" \
  ".agents/rules/billing.md" \
  ".agents/rules/needtoken-fork.md" \
  "cloudflare/wrangler.jsonc" \
  "Dockerfile" \
  "model/user.go" \
  "middleware/auth.go" \
  "router/api-router.go" \
  "service/authz/casbin.go" \
  "service/quota.go" \
  "pkg/billingexpr/eval.go" \
  "controller/subscription.go" \
  "controller/auth.go" \
  "controller/needtoken_account_link.go" \
  "controller/needtoken_admin_tokens.go" \
  "controller/needtoken_billing_shortfall.go" \
  "controller/needtoken_sso.go" \
  "controller/needtoken_sso_test.go" \
  "model/needtoken_billing_shortfall.go" \
  "main.go" \
  "service/needtoken_billing_recovery.go" \
  "VERSION" \
  "web/index.html" \
  "web/public/favicon.ico" \
  "web/src/routes/_authenticated/account-link.tsx" \
  "web/src/routes/(auth)/oauth.tsx" \
  "web/src/routes/index.tsx" \
  "web/src/components/layout/components/system-brand.tsx" \
  "web/src/components/layout/components/public-header.tsx" \
  "web/src/hooks/use-top-nav-links.ts" \
  "scripts/init.sql"; do
  is_high_risk_path "$p" || fail "expected high-risk: $p"
done

for p in \
  "README.md" \
  "web/src/i18n/locales/en.json" \
  "relay/channel/openai/adaptor.go" \
  "i18n/locales/en.json"; do
  if is_high_risk_path "$p"; then
    fail "expected low-risk: $p"
  fi
done

# 3. 端到端 mock：sync-upstream no_eligible_tag 安全跳过 + auto-merge 条件检查（native-grants）
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/resolve-none.sh" <<'EOS'
#!/usr/bin/env bash
printf 'status=no_eligible_tag\nchannel=stable-tags\nmin_tag=v1.0.0\n'
EOS
chmod +x "$tmp/resolve-none.sh"

(
  cd "$root/../.."
  GITHUB_REPOSITORY=NeedTokenAi/needtoken-api \
  RESOLVE_UPSTREAM="$tmp/resolve-none.sh" \
  bash "$root/sync-upstream.sh"
) >/dev/null

# 4. auto-merge-upstream 条件检查与低风险 Merge Commit 验证
UPSTREAM_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
BASE_SHA="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
HEAD_SHA="cccccccccccccccccccccccccccccccccccccccc"
HEAD_BRANCH="sync/upstream/${UPSTREAM_SHA}"

cat >"$tmp/gh" <<'EOS'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${MOCK_LOG:?}"
cmd=${1:-}
shift || true
if [[ $cmd == pr && ${1:-} == list ]]; then
  cat "${MOCK_PR_LIST:?}"
  exit 0
fi
if [[ $cmd == api ]]; then
  method="GET"
  jqfilter=""
  endpoint=""
  while [[ $# -gt 0 ]]; do
    case $1 in
      -X) method=$2; shift 2 ;;
      --jq) jqfilter=$2; shift 2 ;;
      --paginate) shift ;;
      -f | -F) shift 2 ;;
      *) [[ -z $endpoint ]] && endpoint=$1; shift ;;
    esac
  done
  case "${method}:${endpoint}" in
    GET:repos/*/commits/*/check-runs) out=$(cat "${MOCK_CHECK_RUNS:?}") ;;
    GET:repos/*/commits/*/status) out='{"total_count":0,"state":"pending"}' ;;
    GET:repos/*/commits/*) out=$(cat "${MOCK_COMMIT:?}") ;;
    GET:repos/*/compare/*) out='{"status":"identical"}' ;;
    GET:repos/*/pulls/*/files) out=$(cat "${MOCK_FILES:?}") ;;
    GET:repos/*/pulls/*) out=$(cat "${MOCK_PR_DETAIL:?}") ;;
    GET:repos/*/issues/*/comments) out='[]' ;;
    POST:repos/*/issues/*/comments) printf '{"id":1}\n'; exit 0 ;;
    PUT:repos/*/pulls/*/merge) printf '{"merged":true}\n'; exit 0 ;;
    DELETE:repos/*/git/refs/heads/*) exit 0 ;;
    *) echo "unexpected api: ${method} ${endpoint}" >&2; exit 1 ;;
  esac
  if [[ -n $jqfilter ]]; then jq -r "$jqfilter" <<<"$out"; else printf '%s\n' "$out"; fi
  exit 0
fi
exit 1
EOS
chmod +x "$tmp/gh"

cat >"$tmp/pr_list.json" <<EOF
[{"number":15,"url":"https://example.test/pr/15","headRefOid":"${HEAD_SHA}","baseRefName":"main","isDraft":false}]
EOF
cat >"$tmp/commit.json" <<EOF
{"parents":[{"sha":"${BASE_SHA}"},{"sha":"${UPSTREAM_SHA}"}]}
EOF
printf '{"changed_files":1}\n' >"$tmp/pr_detail.json"

# 4a. 改动 go.mod 时必须等待 native-grants 通过
printf '[{"filename":"go.mod"}]\n' >"$tmp/files.json"
cat >"$tmp/check_runs.json" <<'EOF'
{"check_runs":[
  {"id":1,"name":"Backend vet, build, and test","status":"completed","conclusion":"success"},
  {"id":2,"name":"Frontend typecheck and test","status":"completed","conclusion":"success"}
]}
EOF
: >"$tmp/gh.log"
env PATH="$tmp:$PATH" \
  GITHUB_REPOSITORY=NeedTokenAi/needtoken-api \
  HEAD_BRANCH="$HEAD_BRANCH" HEAD_SHA="$HEAD_SHA" \
  REQUIRED_CHECKS=$'Backend vet, build, and test\nFrontend typecheck and test' \
  CONDITIONAL_CHECKS=$'go.mod|native-grants' \
  MOCK_LOG="$tmp/gh.log" MOCK_PR_LIST="$tmp/pr_list.json" MOCK_COMMIT="$tmp/commit.json" \
  MOCK_PR_DETAIL="$tmp/pr_detail.json" MOCK_FILES="$tmp/files.json" MOCK_CHECK_RUNS="$tmp/check_runs.json" \
  bash "$root/auto-merge-upstream.sh" >/dev/null
if grep -q "pulls/15/merge" "$tmp/gh.log"; then
  fail "must wait for conditional check native-grants when go.mod changes"
fi

# 4b. native-grants 也通过且均为低风险路径 -> 自动 Merge Commit
cat >"$tmp/check_runs.json" <<'EOF'
{"check_runs":[
  {"id":1,"name":"Backend vet, build, and test","status":"completed","conclusion":"success"},
  {"id":2,"name":"Frontend typecheck and test","status":"completed","conclusion":"success"},
  {"id":3,"name":"native-grants","status":"completed","conclusion":"success"}
]}
EOF
: >"$tmp/gh.log"
env PATH="$tmp:$PATH" \
  GITHUB_REPOSITORY=NeedTokenAi/needtoken-api \
  HEAD_BRANCH="$HEAD_BRANCH" HEAD_SHA="$HEAD_SHA" \
  REQUIRED_CHECKS=$'Backend vet, build, and test\nFrontend typecheck and test' \
  CONDITIONAL_CHECKS=$'go.mod|native-grants' \
  MOCK_LOG="$tmp/gh.log" MOCK_PR_LIST="$tmp/pr_list.json" MOCK_COMMIT="$tmp/commit.json" \
  MOCK_PR_DETAIL="$tmp/pr_detail.json" MOCK_FILES="$tmp/files.json" MOCK_CHECK_RUNS="$tmp/check_runs.json" \
  bash "$root/auto-merge-upstream.sh" >/dev/null
grep -q "PUT repos/NeedTokenAi/needtoken-api/pulls/15/merge -f merge_method=merge -f sha=${HEAD_SHA}" "$tmp/gh.log" \
  || fail "expected merge commit API call once all required + conditional checks pass"

# 4c. 命中计费/高危路径 -> 阻止自动合并
printf '[{"filename":"service/quota.go"}]\n' >"$tmp/files.json"
: >"$tmp/gh.log"
env PATH="$tmp:$PATH" \
  GITHUB_REPOSITORY=NeedTokenAi/needtoken-api \
  HEAD_BRANCH="$HEAD_BRANCH" HEAD_SHA="$HEAD_SHA" \
  REQUIRED_CHECKS=$'Backend vet, build, and test\nFrontend typecheck and test' \
  MOCK_LOG="$tmp/gh.log" MOCK_PR_LIST="$tmp/pr_list.json" MOCK_COMMIT="$tmp/commit.json" \
  MOCK_PR_DETAIL="$tmp/pr_detail.json" MOCK_FILES="$tmp/files.json" MOCK_CHECK_RUNS="$tmp/check_runs.json" \
  bash "$root/auto-merge-upstream.sh" >/dev/null
if grep -q "pulls/15/merge" "$tmp/gh.log"; then
  fail "billing high-risk path must block auto-merge"
fi
grep -q "POST repos/NeedTokenAi/needtoken-api/issues/15/comments" "$tmp/gh.log" \
  || fail "expected risk gate comment on high-risk PR"

# 4d. 命中 NeedToken 定制组件 profile-dropdown.tsx 与 billing_session.go -> 阻止自动合并
source "$root/lib/high-risk-paths.sh"
is_high_risk_path "web/src/components/profile-dropdown.tsx" || fail "profile-dropdown.tsx must be high-risk"
is_high_risk_path "service/billing_session.go" || fail "service/billing_session.go must be high-risk"
is_high_risk_path "service/group.go" || fail "service/group.go must be high-risk"
is_high_risk_path "controller/needtoken_slo.go" || fail "controller/needtoken_slo.go must be high-risk"
is_high_risk_path "controller/needtoken_unified_wallet.go" || fail "controller/needtoken_unified_wallet.go must be high-risk"
is_high_risk_path "model/needtoken_wallet_ledger.go" || fail "model/needtoken_wallet_ledger.go must be high-risk"

echo "needtoken-api upstream-sync ok"

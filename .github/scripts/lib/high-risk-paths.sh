#!/usr/bin/env bash
# 判定 needtoken-api 上游同步 PR 改动路径是否命中高危路径（fail-closed）。
# 涵盖 CI/工作流、Cloudflare/容器部署、数据库模型、鉴权、NeedToken 履约链路与计费规则路径。

is_high_risk_path() {
  local path=$1
  case "$path" in
    # CI / 工作流 / 供应链与核心规则文件
    .github/* | .agents/rules/needtoken-fork.md | .agents/rules/billing.md)
      return 0
      ;;
    # 容器 / Cloudflare 部署边界 / 示例配置
    cloudflare/* | Dockerfile | Dockerfile.* | */Dockerfile | */Dockerfile.* | \
      docker-compose*.yml | compose*.yml | .env.example)
      return 0
      ;;
    # 任意 SQL 脚本 / 迁移
    *.sql | */migrations/* | migrations/*)
      return 0
      ;;
    # 数据库模型、鉴权中间件、路由、Casbin 权限（同时覆盖 NeedToken fulfillment 关键目录）
    model/* | middleware/* | router/* | service/authz/*)
      return 0
      ;;
    # 计费核心路径（与 .agents/rules/billing.md 对齐）
    pkg/billingexpr/* | setting/billing_setting/* | common/quota_math.go | types/price_data.go | \
      relay/request_billing.go | relay/image_handler.go | relay/relay_task.go | \
      relay/helper/price.go | relay/helper/billing_expr_request.go | relay/helper/valid_request.go | \
      relay/common/relay_info.go | service/quota.go | service/text_quota.go | \
      service/image_billing.go | service/tiered_settle.go | service/task_billing.go | \
      service/responses_usage.go | service/log_info_generate.go)
      return 0
      ;;
    # 鉴权 / 账户 / 计费 / 兑换 / 套餐 / 令牌控制器
    controller/auth* | controller/oauth* | controller/user* | controller/billing* | \
      controller/redemption* | controller/subscription* | controller/topup* | \
      controller/token* | controller/webauthn* | controller/totp*)
      return 0
      ;;
    # NeedToken 二开保护区
    controller/needtoken_* | controller/external_grant* | \
      web/src/routes/_authenticated/account-link.tsx)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

filter_high_risk_paths() {
  local line
  while IFS= read -r line; do
    [[ -n $line ]] || continue
    if is_high_risk_path "$line"; then
      printf '%s\n' "$line"
    fi
  done
}

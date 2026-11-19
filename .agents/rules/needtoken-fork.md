# NeedToken Fork 开发与上游同步规则（needtoken-api）

本仓是上游 `QuantumNous/new-api` 的生产 Fork。在遵守上游 `AGENTS.md`（含 `.agents/rules/billing.md`、`web/AGENTS.md` 与“不向 `docs/` 新增文件”等约定）的前提下，所有本地开发与 AI 协助必须同时遵守以下规则。

## 1. Remote 与分支规范

- `origin`：`NeedTokenAi/needtoken-api`（日常推送与提 PR）。
- `upstream`：`QuantumNous/new-api`（只读 `fetch`，本地必须 `git remote set-url --push upstream no_push` 禁止误推）。
- `main` 是唯一发布基线，**禁止本地直接 commit 或 push 到 `main`**；合入 `main` 会触发 Cloudflare Workers Builds 自动构建与发布。
- 日常开发一律在 `dev`（单线迭代）或 `feat/<topic>` / `fix/<topic>`（并行需求）进行，提 PR 合并进 `main`。
- 每次 PR 或上游同步合入 `main` 后，及时将 `origin/main` 合并/对齐回本地 `dev`，避免长期分叉。

## 2. 减少上游冲突的定制隔离原则

- NeedToken 专属定制优先放在**独立新增文件 / 独立目录**（如 `cloudflare/`、独立 controller/service/test 文件），对上游核心文件只做最小侵入式改动。
- **严禁**顺手格式化、重命名或重构与当前任务无关的上游代码。
- 不要直接修改上游 `AGENTS.md`；NeedToken 专属规则维护在本文件（`.agents/rules/needtoken-fork.md`）与主仓 `needtoken/AGENTS.md`。

## 3. 上游同步与自动合并红线

- 自动同步工作流：`.github/workflows/sync-upstream.yml`（默认通道 `stable-tags`，仅跟 `vMAJOR.MINOR.PATCH` 稳定语义化版本，不跟 `/releases/latest`）。
- **版本底线保护**：当前 fork 基线已在 `v1.0.0-rc.47`，因此默认 `UPSTREAM_MIN_TAG=v1.0.0`，自动跳过历史 `v0.13.x` 维护分支，待上游发布 `>= v1.0.0` 正式稳定 tag 后自动拉起同步 PR。
- **上游同步 PR（`sync/upstream/<40位sha>`）严禁 Squash / Rebase，必须使用 `Create a merge commit`**；Squash 会丢弃上游父提交并导致后续反复冲突。
- 非高危路径自动合并门禁：`.github/workflows/auto-merge-upstream.yml` 在受信任的 `main` 上以 `workflow_run` 运行，仅在 `CI`（以及命中的 `NeedToken fulfillment` / `native-grants`）全部通过、双父 merge commit 校验通过、变更文件数未超阈值且未命中 `.github/scripts/lib/high-risk-paths.sh` 高危路径时，自动执行 `Merge Commit` 并触发 Cloudflare 自动构建。
- 命中高危路径（`.github/`、`cloudflare/`、`Dockerfile*`、`*.sql`、`model/`、`middleware/`、`router/`、`service/authz/`、计费核心文件及鉴权/订阅/充值/令牌 controller）或产生冲突时，必须人工审阅与验证后再以 `Merge Commit` 合并；临时关闭自动合并请设置仓库 Variable `UPSTREAM_AUTO_MERGE=false`。
- 必须在仓库 Secrets 配置 `SYNC_GITHUB_TOKEN`（细粒度 PAT，`Contents: write` + `Pull requests: write`），这样自动开出的同步 PR 才能原生触发 `pull_request` CI。

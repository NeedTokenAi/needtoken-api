# NeedToken API 上游同步

上游是 `QuantumNous/new-api`。同步工作流是 `.github/workflows/sync-upstream.yml`。它只打开 pull request，不部署，也没有 GCP 凭证。

调度是每天 `0 0 * * *` UTC，也就是亚洲/上海 08:00。GitHub 的 cron 可能延迟。Actions 里可以手动跑 `Sync upstream`。

## 选定的通道

2026-11-04 查看了 `QuantumNous/new-api` 的 releases 和 tags。当前不能把稳定版和 RC 混在同一次同步里。

| 观察到的发布 | 处理 |
| --- | --- |
| `v1.0.0-rc.48`，发布于 2026-11-04T15:40:15Z | 当前 RC 线的最新编号。标签对象就是提交 `706144769e5bed5a47b78721e3bb11da3cf4c0b9` |
| `v1.0.0-rc.1` 到 `v1.0.0-rc.47` | 同一条 RC 线，只取数字最大的一个 |
| `v1.0.0-rc.19-i18nfix.1`、`v1.0.0-rc.19-i18nfix.2` | 不进入通道。后缀热修不是单调的 `rc.N` |
| `v1.0.0-alpha.1`、`nightly-*` | 不进入通道 |
| `v0.13.2`，发布于 2026-04-27T14:11:42Z | 当前最新的稳定版式标签。RC 同步不使用它 |

GitHub API 把 `v1.0.0-rc.48` 和 `v0.13.2` 的 `prerelease` 都标成 `false`，所以不能靠这个字段区分。Release 的 `target_commitish` 是分支名 `main`，会随着上游移动，也不能当作要合并的提交。

工作流的规则：

1. 仓库变量 `UPSTREAM_CHANNEL` 必须是 `vX.Y.Z-rc` 这种 RC 系列前缀。现在设为 `v1.0.0-rc`。未设置时工作流使用这个默认值，并在日志里写明。
2. 只接受精确标签 `^v1.0.0-rc.[0-9]+$`。`rc.10` 大于 `rc.9`。`v1.0.0-rc.08` 和 `v1.0.0-rc.8` 同时存在时用没有前导零的那个。
3. `git ls-remote` 只查询这一个标签。带 `^{}` 的剥离开提交优先，否则使用轻量标签上的那一个提交。
4. 换成下一条 RC 线时，把变量改成例如 `v1.1.0-rc`。不要设成 `stable`、`latest`、`v0.13` 或完整标签 `v1.0.0-rc.48`。这些值会失败，并且不会退回稳定版。

这个 SHA 是观察时的尖端，不是写死在工作流里的钉。每次运行都重新解析。

## 什么时候不会开 PR

- 该提交已经是 `main` 的祖先。
- 已经有一个打开的、head 以 `sync/upstream/` 开头的 PR。先合并或关闭它。工作流不会自动关掉旧 PR，也不会同时打开第二个。
- 合并冲突。作业失败，摘要里有本仓库 `main` 的 SHA、上游 SHA、标签、通道和冲突文件。冲突标记不会被推上去，临时分支也会删掉。

干净的结果是分支 `sync/upstream/<40位sha>` 上的一个普通 merge commit：第一父提交是当时的 `main`，第二父提交是上游提交。没有 `--squash`，也没有 `-X ours` 或 `-X theirs`。

推送的 refspec 只有 `refs/heads/sync/upstream/<sha>`。获取使用 `git fetch --no-tags`，并且只取这一个上游提交。不要 `git push --tags`，也不要把上游标签批量推到本仓库。

## 合并这个 PR

仓库需要允许 merge commit。合并同步 PR 时选择 **Create a merge commit**。Squash 或 rebase 会丢掉上游提交这个父提交，下一次同步就看不出它已经在 `main` 里，并可能把同一段历史再合并一次。

PR 打开后，`SYNC_GITHUB_TOKEN` 触发的 `pull_request` 会跑 CI。CI 绿了再合并。合并产生的 main 推送再跑 CI，通过后才走 [部署](deploy.md)。同步作业本身不能部署。

## SYNC_GITHUB_TOKEN

Secret 名字是 `SYNC_GITHUB_TOKEN`。用一个只拥有 `NeedTokenAi/needtoken-api` 的 fine-grained PAT，权限是 Contents 读写和 Pull requests 读写。这是每天 08:00 能直接使用的凭证。

GitHub App 的 installation token 大约 60 分钟过期，放进 secret 后活不到下一次 cron。这个工作流不会用 App 私钥现签 token。若必须用 GitHub App，要在别的已审核作业里把新 token 写入 `SYNC_GITHUB_TOKEN`，或者继续使用上面的 PAT。

不要用工作流自带的 `GITHUB_TOKEN` 代替它。GitHub 规定：由 `GITHUB_TOKEN` 触发的事件不会再创建工作流运行（`workflow_dispatch` 和 `repository_dispatch` 除外）。结果是：

- 同步 PR 的 `pull_request` 不会启动 `CI`，必需检查不出现。
- 用这个 token 合并后，push 到 `main` 也不会启动 `CI`。
- 部署等待的是 `CI` 的 `workflow_run`，所以上游提交不会被验证，也不会发布。

PAT 或 GitHub App 用户发出的事件会正常启动 CI。同步工作流的 `permissions` 只有 `contents: read`，写入靠这个 secret。它不读取 `GCP_WORKLOAD_IDENTITY_PROVIDER` 或 `GCP_SERVICE_ACCOUNT`，也不申请 OIDC token。不要把 GCP JSON 密钥放进 `SYNC_GITHUB_TOKEN`。

## 冲突时给 Codex 的材料

失败的作业摘要包含：

- `base_sha`：当时的 `main`
- `upstream_sha`：要合并的上游提交
- `upstream_tag` 和 `upstream_channel`
- `conflict_file` 列表

用这两个 SHA 在本地做一次普通 `git merge`，解决列出的文件，再推到 `sync/upstream/<upstream_sha>` 并打开 PR。不要提交带冲突标记的文件，也不要改用 ours/theirs 策略把某一侧整段丢掉。

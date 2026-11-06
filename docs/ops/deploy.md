# NeedToken API 部署

生产服务是 GCP 项目 `suichong`、区域 `asia-southeast1` 的 Cloud Run 服务 `needtoken-api`，域名 `https://api.needtoken.ai`。镜像只来自本仓库构建出的 Artifact Registry digest。不要改用官方 `calciumion/new-api` 镜像，也不要让 Cloud Build 或 Cloud Run 持续部署并行改这个服务。

本文件只说明流水线。它不部署、不创建密钥、不改 Cloud Run 上已有的环境变量和 Secret。

## 一次 main 推送会怎样

1. `.github/workflows/ci.yml`（工作流名 `CI`）在每个 pull request，以及每次 push 到 `main` 时运行。Deploy 只认 **push 到 main** 的那次成功结果，因为只有那次的 `head_sha` 才是将要上线的提交。
2. `CI` 成功后，`.github/workflows/deploy-cloudrun.yml` 由 `workflow_run` 启动。它核对仓库是 `NeedTokenAi/needtoken-api`、事件是 `push`、分支是 `main`、结论是 `success`、工作流路径是 `.github/workflows/ci.yml`，并且该 SHA 有一次成功的 CI push 记录。
3. SHA 必须等于当时的 `origin/main`。落后的 CI 结果直接跳过，不构建、不改流量。
4. 流水线用 GitHub OIDC 换取短期 GCP 凭证，构建 `linux/amd64` 镜像，推送到本项目 Artifact Registry，标签是完整 git SHA。部署引用 `IMAGE@sha256:...`。
5. `gcloud run services update needtoken-api --image=...@sha256:... --no-traffic --tag=candidate` 只新增 0% 流量的修订。确认该修订没有生产流量，并且容器镜像就是这个 digest 之后，先请求 candidate 标签 URL 的 `/api/status` 和 `/api/pricing`。
6. 再次确认 `main` 没有往前走，然后把该修订切到 100% 流量，再请求 `https://api.needtoken.ai` 的同样两个接口。公网冒烟失败时，流量恢复到更新前的百分比。
7. 同一服务同时只跑一个部署，进行中的部署不会被取消。若 100% 流量的修订已经是这个 git SHA 对应的 digest，本次跳过。

手动部署使用 `workflow_dispatch`，输入 `git_sha`（40 位十六进制）。该 SHA 必须已经在 `main` 历史上，并且曾经有一次成功的 CI **push** 记录。手动部署可以不是当前 tip，用来回滚到仍在 `main` 历史上的旧提交；下一次 push 到 `main` 仍会部署新的 tip。要让回滚留下来，应在 `main` 上 revert，而不是只手动部署旧 SHA。

`workflow_run` 的 `github.sha` 是这份部署工作流所在的默认分支提交，不是 CI 测过的提交。部署 checkout 的是解析出来的 `head_sha`。构建上下文是那个 SHA；冒烟脚本和镜像路径校验来自当前 `main` 上的流水线文件。

## 必须变绿的检查

在 `main` 的规则集里要求这些 **CI 作业名**。名字必须一致：

- `Backend vet, build, and test`
- `Frontend typecheck and test`
- `NeedToken fulfillment databases`
- `Ops script checks`

`NeedToken fulfillment databases` 在每个 pull request 和每次 push 到 `main` 都跑 MySQL 5.7、PostgreSQL 和 SQLite 上的 `./model ./middleware ./controller ./router` 测试。`.github/workflows/needtoken-fulfillment.yml` 仍按路径过滤额外跑一遍，**不能**当作部署门禁，也不要把它的作业 `native-grants` 设成唯一必需检查。路径没命中时这个工作流不会出现，必需检查会一直缺着。

Deploy 工作流不是 pull request 检查。它在 main 的 CI 成功之后运行。

## 镜像

默认镜像路径：

```text
asia-southeast1-docker.pkg.dev/suichong/needtoken/needtoken-api
```

仓库变量 `GCP_CLOUD_RUN_IMAGE` 可以改成同一个 `asia-southeast1-docker.pkg.dev/suichong/<仓库>/<镜像>` 路径，但不能带标签或 digest。其他项目、其他区域、Docker Hub，以及任何包含 `calciumion` 的路径都会被拒绝。

Cloud Run 命令只改镜像、候选标签和流量。工作流不传 `--set-env-vars`、`--update-env-vars`、`--env-vars-file`、`--set-secrets`、`--update-secrets`、`--clear-env-vars` 或 `--remove-env-vars`，因此已有环境变量和 Secret 引用会留下来。也不要在控制台用「编辑并部署新修订」整页覆盖环境变量。

冒烟要求两个接口都返回 HTTP 200，且 JSON 对象的 `success` 是布尔值 `true`：

- `/api/status`
- `/api/pricing`

这两个接口今天是公开的。如果以后把 pricing 收成登录才可用，候选修订的冒烟会失败，流量不会切过去。

## 关掉会打架的自动部署

仓库里没有 `cloudbuild.yaml`。会改掉 `needtoken-api` 的自动部署只能来自 GCP 或 GitHub 上另接的触发器。上线这条流水线之前先关掉它们，否则下一次上游镜像发布或 main 推送会把服务改回别的镜像。

1. 查看 Cloud Build 触发器，删除或停用任何会构建本仓库、部署 `needtoken-api`，或拉取官方镜像的触发器：

   ```bash
   gcloud builds triggers list --project=suichong
   gcloud builds triggers delete TRIGGER_ID --project=suichong
   ```

2. 打开 Cloud Run 服务 `needtoken-api`（`asia-southeast1`）。如果「持续部署」连着某个仓库或 Cloud Build 触发器，断开它。部署来源只保留本工作流。
3. 看当前镜像。结果里如果是 `calciumion/new-api`，或带浮动标签而不是 `asia-southeast1-docker.pkg.dev/suichong/...@sha256:`，先不要按官方文档升级；等本流水线的第一次成功部署替换它。

   ```bash
   gcloud run services describe needtoken-api \
     --project=suichong \
     --region=asia-southeast1 \
     --format='yaml(spec.template.spec.containers[0].image,metadata.annotations)'
   ```

4. 在 GitHub 的 Actions 页面停用 `.github/workflows/docker-build.yml` 和 `.github/workflows/docker-image-branch.yml`。它们仍会把镜像推到 `calciumion/new-api`。不要给这个 fork 配置 `DOCKERHUB_USERNAME` 或 `DOCKERHUB_TOKEN`，也不要手动跑这两个工作流。停用状态存在 GitHub 上，不在 git 里，所以上游同步不会把它重新打开；新 fork 或重新启用后要再停一次。
5. `README`、`docker-compose.yml` 和 `docs/installation/BT.md` 里的 `calciumion/new-api:latest` 是上游示例，不是 `api.needtoken.ai` 的部署方式。不要 `docker pull` 那个镜像，也不要把它写进 Cloud Run。
6. 管理后台的系统更新对话框只打开 `https://github.com/QuantumNous/new-api/releases/tag/...`，不会构建或切换 Cloud Run。不要把它当成一键升级。

`release.yml` 和 `electron-build.yml` 打的是上游发行包，不更新这个 Cloud Run 服务。

## 仍需人工完成的配置

下面这些还没做。不要创建 JSON 密钥，也不要把密钥放进仓库。

### 1. Artifact Registry

```bash
gcloud artifacts repositories create needtoken \
  --repository-format=docker \
  --location=asia-southeast1 \
  --project=suichong \
  --description="NeedToken API images"
```

仓库名可以不是 `needtoken`，但必须在 `suichong` / `asia-southeast1`。若改名，把完整镜像路径设为仓库变量 `GCP_CLOUD_RUN_IMAGE`。

### 2. 部署用服务账号

建议邮箱 `needtoken-api-deploy@suichong.iam.gserviceaccount.com`。只给这三项：

- 上面那个 Artifact Registry 仓库的 `roles/artifactregistry.writer`
- Cloud Run 服务 `needtoken-api` 的 `roles/run.developer`
- **运行时**服务账号上的 `roles/iam.serviceAccountUser`

运行时账号用第一条命令看。为空时 Cloud Run 用的是默认 Compute 服务账号，`serviceAccountUser` 要授在那个账号上。把 `RUNTIME_SA_EMAIL` 换成那个结果：

```bash
gcloud run services describe needtoken-api \
  --project=suichong \
  --region=asia-southeast1 \
  --format='value(spec.template.spec.serviceAccountName)'

gcloud iam service-accounts create needtoken-api-deploy \
  --project=suichong \
  --display-name="NeedToken API Cloud Run deploy"

gcloud artifacts repositories add-iam-policy-binding needtoken \
  --project=suichong \
  --location=asia-southeast1 \
  --member="serviceAccount:needtoken-api-deploy@suichong.iam.gserviceaccount.com" \
  --role="roles/artifactregistry.writer"

gcloud run services add-iam-policy-binding needtoken-api \
  --project=suichong \
  --region=asia-southeast1 \
  --member="serviceAccount:needtoken-api-deploy@suichong.iam.gserviceaccount.com" \
  --role="roles/run.developer"

gcloud iam service-accounts add-iam-policy-binding RUNTIME_SA_EMAIL \
  --project=suichong \
  --member="serviceAccount:needtoken-api-deploy@suichong.iam.gserviceaccount.com" \
  --role="roles/iam.serviceAccountUser"
```

### 3. Workload Identity

把 `PROJECT_NUMBER` 换成 `gcloud projects describe suichong --format='value(projectNumber)'` 的结果。Provider 只接受本仓库 `main` 上的部署工作流，不接受 pull request：

```bash
gcloud iam workload-identity-pools create github \
  --project=suichong \
  --location=global \
  --display-name="GitHub Actions"

gcloud iam workload-identity-pools providers create-oidc github \
  --project=suichong \
  --location=global \
  --workload-identity-pool=github \
  --display-name="GitHub Actions" \
  --issuer-uri="https://token.actions.githubusercontent.com" \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.ref=assertion.ref,attribute.workflow_ref=assertion.workflow_ref" \
  --attribute-condition="assertion.repository=='NeedTokenAi/needtoken-api' && assertion.ref=='refs/heads/main' && assertion.workflow_ref.startsWith('NeedTokenAi/needtoken-api/.github/workflows/deploy-cloudrun.yml@refs/heads/main')"
```

把 `roles/iam.workloadIdentityUser` 授给部署服务账号。`google.subject` 映射自 GitHub 的 `sub`，在 `main` 上的值是 `repo:NeedTokenAi/needtoken-api:ref:refs/heads/main`：

```bash
gcloud iam service-accounts add-iam-policy-binding \
  needtoken-api-deploy@suichong.iam.gserviceaccount.com \
  --project=suichong \
  --role="roles/iam.workloadIdentityUser" \
  --member="principal://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/github/subject/repo:NeedTokenAi/needtoken-api:ref:refs/heads/main"
```

### 4. GitHub 仓库变量

在 `NeedTokenAi/needtoken-api` 的 Actions Variables 里设置：

| 变量 | 值 |
| --- | --- |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | `projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/github/providers/github` |
| `GCP_SERVICE_ACCOUNT` | 部署服务账号邮箱 |
| `GCP_CLOUD_RUN_IMAGE` | 可选。不设则用上面的默认镜像路径 |
| `UPSTREAM_CHANNEL` | `v1.0.0-rc`。见 [上游同步](upstream-sync.md) |

不要新增名为 JSON、密钥文件或 `GCP_SA_KEY` 的 secret。CI 工作流的权限只有 `contents: read`。部署工作流只在真正部署的那个作业上申请 `id-token: write`。Pull request 不会拿到生产身份。仓库里没有 `pull_request_target`。

变量还没设置时，合并后的第一次部署会在认证前失败，Cloud Run 流量保持原样。

### 5. 分支保护

`main` 要求上面四个 CI 作业，并允许 merge commit。上游同步 PR 必须用 “Create a merge commit”，不能 squash。详见 [上游同步](upstream-sync.md)。

可选：给 GitHub Environment `production` 加人工批准，再把部署作业放进这个环境。当前工作流没有绑定环境，避免一个还不存在的环境保护把部署卡死。

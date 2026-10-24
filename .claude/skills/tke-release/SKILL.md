---
name: tke-release
description: >-
  一键发版 tokensolo-sub2api（sub2api 新 fork，GPT 号池上游）到腾讯云 TKE 集群(cls-tokensolo-tke-japan / ns tokensolo):
  打 tag → 等 GitHub Actions 镜像构建 → 本地 kubectl 滚动部署 → 健康验证 → 冒烟 → 监控 → 生成发布报告。
  当用户要"发版/发布/部署 sub2api 到 TKE 或 k8s"、"打 tag 发新版本"、"上线新版本到集群"时使用。
  只有 prod 环境；tag 可选，缺省按 vYYYY.MM.DD.N 规则自动生成下一个版本号。
# 仅人工手动触发(/tke-release),禁止模型自动调用——发版为高副作用操作。
disable-model-invocation: true
# 参数占位提示:tag 可选。sub2api 只有 prod，不接受 env 参数。
argument-hint: "[tag]"
# 最小权限:仅发版所需工具。git/gh/kubectl/curl/make + 读文档/写报告/询问 commit。
allowed-tools:
  - Bash(git *)
  - Bash(gh *)
  - Bash(kubectl *)
  - Bash(curl *)
  - Bash(tccli *)
  - Bash(jq *)
  - Bash(go build *)
  - Bash(go test *)
  - Bash(make *)
  - Bash(cat *)
  - Bash(cd *)
  - Bash(export *)
  - Bash(date *)
  - Bash(sleep *)
  - Read
  - Write
  - Edit
  - AskUserQuestion
  - Agent
---

# TKE 一键发版 (sub2api)

自动化 TKE 发版全流程：`前置检查 → git tag → push → 镜像构建(Actions) → kubectl 滚动部署 → 冒烟测试 → 监控`。

sub2api 是 tokensolo 的 **GPT 号池上游**（地位等同 tokensolo-crs），挂掉 = tokensolo 全部 GPT 系模型不可用。

## 参数

| 参数 | 必填 | 说明 |
|------|------|------|
| `tag` | 否 | 缺省按下方规则自动生成下一个版本号；格式 `vYYYY.MM.DD.N`。 |

调用示例：`/tke-release`、`/tke-release v2026.09.10.2`。

**没有 env 参数**：sub2api 只部署 prod。若用户传了 `staging`/`prod` 字样，`prod` 忽略即可，`staging` 直接报错中止：「sub2api 未部署 staging 环境，只有 prod」。

## ⚠️ 授权与约束

- 平时对线上 TKE 集群是**只读**约束。**本 skill 是用户主动发版，流程内的 `kubectl set image / rollout / undo` 等集群写操作在此次发版中被授权**——仅限本发版流程，不扩展到其他场景。
- **不自动 `git commit`**：预检若发现未提交改动，用 `AskUserQuestion` 询问用户是否提交及 commit message，得到确认后再 commit；tag / push / 部署属发版核心，用户调用即视为授权，自动执行。
- **manifests 在本仓库**：K8s 清单在 `deploy/k8s/prod/`（2026-10-24 从 tokensolo 仓库迁入，单独维护；Ingress 仍在 `../tokensolo/docs/deploy/k8s/manifests/prod/ingress/`）。日常发版只 `kubectl set image`，**不要** `kubectl apply -f` 该目录（会把镜像重置为清单里的初始 tag）。

## 🚨 致命红线（高于所有 Phase）

**在 P3 部署后或 P4 冒烟测试中发现以下任一情况，必须立即 `kubectl rollout undo` 止血**：

- panic（`panic` / `runtime error` / `nil pointer dereference` 等）
- Pod 反复 CrashLoopBackOff / ImagePullBackOff
- 启动日志出现 `Auto setup failed` / `Failed to start server` / 迁移 `checksum mismatch`
- 5xx 高频率出现（不只 1-2 个偶发）
- 100% 请求失败 / 健康检查持续异常 / 关键接口完全不可用

**生产恢复前严禁以下行为**：
- ❌ 拉 panic 栈分析、grep 源码、读 commit diff
- ❌ 解释"为什么会 panic"、推断根因、看 git log
- ❌ "再测一次看看"、"等几秒看是不是偶发"

**正确顺序**（不可颠倒）：
1. **回滚** — `kubectl rollout undo deployment/sub2api`（见 Phase 5 回滚命令）
2. **复测验证恢复** — 至少用一发真实请求确认 200，并查 `kubectl logs` 无 panic
3. **进入 Phase 5 事故响应** — Pod logs / CLS 日志 / GHCR 镜像回滚后照样能拿，不会丢

> 每多 1 分钟分析时间 = 1 分钟 tokensolo 所有 GPT 流量挂掉。先止血，再分析。

---

## 进度面板格式

每个 Phase **开始前**和**完成后**各输出一次：

```
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 TKE 发版进度  sub2api  <tag>  [prod]
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
 ✅ P1  前置检查     完成
 🔄 P2  镜像构建     进行中（约 10-15 分钟）
 ⬜ P3  集群部署     等待
 ⬜ P4  功能冒烟测试  必做
 ⬜ P5  事故响应     仅异常时触发
 ⬜ P6  部署后监控   约 6 分钟异步（6 轮 × 60s）
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
```

图标含义：`✅` 完成 · `🔄` 进行中 · `⬜` 等待 · `❌` 失败/中止

tag 在 Phase 1 确认版本号之前用 `待定`。

---

## 服务参数

| 项 | 值 |
|----|-----|
| 仓库目录 | `tokensolo-sub2api/`（旧 fork `sub2api/` 已停止发版，勿在其中打 tag） |
| GitHub repo | `heiying0917/tokensolo-sub2api`（fork，上游 `Wei-Shaw/sub2api`，基线 0.2.18；旧 fork `XuYang8026/sub2api` 已停用） |
| 分支 | `main` |
| tag 格式 | `vYYYY.MM.DD.N`（当日序号，与 tokensolo 一致） |
| 构建 workflow 名 | `Publish Docker image (GHCR)`（`.github/workflows/docker-image-ghcr.yml`） |
| **不该被触发的 workflow** | `Release`（上游 GoReleaser，只响应 semver tag；`release.yml` 已排除日期 tag） |
| 镜像 | `ghcr.io/heiying0917/tokensolo-sub2api:<tag>`（**含 v**，如 `:v2026.10.23.1`） |
| 拉取密钥 | `ghcr-secret-heiying0917`（由 cpa 首发时创建，同 namespace 复用；旧 `ghcr-secret` 是旧 owner 的 token，拉不到本镜像） |
| Deployment | `sub2api`（**×3 副本**，maxSurge=1 / maxUnavailable=0，逐个滚动全程约 25s；2026-09-11 起） |
| Pod label selector | `app=sub2api` |
| 容器名(set image) | `sub2api` |
| rollout timeout | 300s |
| 健康 URL | `https://sub2api.tokensolo.com/health` → `{"status":"ok"}` |
| 容器内自检 | `kubectl exec -n tokensolo deploy/sub2api -- wget -qO- http://127.0.0.1:8080/health`（镜像只有 wget，没有 curl） |
| KUBECONFIG | `~/.kube/tokensolo.config` |
| Namespace | `tokensolo` |
| manifests | `deploy/k8s/prod/`（本仓库；`secret.yaml` git-ignored 只留本机） |
| 发布报告目录 | `../tokensolo/docs/deploy/report/`（本仓库 `docs/*` 被上游 .gitignore 忽略） |

---

## Phase 1 — 前置检查

**开始时**输出进度面板（P1 标记 🔄，其余 ⬜，tag=待定）。

### Step 1.0 — 生产确认

sub2api 只有 prod，**每次发版都要**用 `AskUserQuestion` 弹窗二次确认：

```
question: "即将发布 sub2api 到生产环境（sub2api.tokensolo.com，tokensolo 的 GPT 号池上游），请再次确认"
header: "⚠️ 生产发版确认"
options:
  - label: "确认，发布到生产（prod）"
    description: "滚动更新 3 副本 Deployment，逐个替换，新 Pod 就绪后才摘旧 Pod"
  - label: "取消"
    description: "中止本次发版"
multiSelect: false
```

用户选"取消"则中止，输出面板 P1 ❌。

### Step 1.1 — 代码预检

每一步失败立即停止，不跳过：

```bash
# 1. 进入仓库
cd tokensolo-sub2api/

# 2. 确认分支
git branch --show-current        # 必须是 main，否则提示用户确认

# 3. 拉最新代码
git fetch origin
git pull origin main

# 4. 拉最新远端 tag（关键！本地 tag 可能落后，会导致版本号推算错误）
git fetch --tags

# 5. 确认工作区干净
git status --short               # 有未提交改动 → AskUserQuestion 询问是否处理（不自动 commit）

# 6. 构建 + 单测（后端）
cd backend
go build ./...
make test-unit 2>&1 | grep -E "^(ok|FAIL|--- FAIL)"
cd ..
```

**测试失败处理**（避免被陈旧测试卡住发版）：

1. 先 `cd backend && go test ./internal/xxx/ -run <TestName> -v` 看错误**性质**：
   - **代码演进、断言陈旧**（上游 merge 后模型列表 / 默认值变了等）→ 直接改测试 assertion 与代码对齐，不要因此停下来
   - **业务代码新引入失败** → ❌ 停止，对比 HEAD~1 验证，报告用户
2. **空输出 ≠ 全绿**：本机 RTK hook 可能把 `go test` 输出压缩成汇总行，`^ok` / `^--- FAIL:` 一行都匹配不到。grep 结果为空时先看 `make test-unit` 退出码，必要时 `rtk proxy "make test-unit"` 取原始输出再 grep。

**pull origin main 冲突处理原则**（本仓库定期 merge upstream，冲突概率高于 tokensolo）：
- **不确定如何解决的冲突**：立即停止，绝不自作主张——把冲突的文件和冲突段落清楚列给用户，等用户确认解法后再继续。
- 冲突全部解决、commit 完成后，才进入打 tag。**带未解决冲突或脏工作树绝不打 tag。**

### Step 1.2 — 确定 tag

```bash
git tag --sort=-creatordate | grep -E '^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$' | head -1
```

格式 `vYYYY.MM.DD.N`，日期取**今天**（`date +%Y.%m.%d`）。N = 今天已有 tag 的最大序号 +1；今天还没有则 N=1。

> 仓库里可能有 merge 上游带进来的 semver tag（`v0.2.18` 等），grep 正则只匹配日期格式，它们不参与推算。新 fork 首个日期 tag 从 `v2026.10.23.1` 起。**不 bump `backend/cmd/server/VERSION`**——版本号由 workflow 通过 build-arg `VERSION` 走 ldflags 注入（`-version` 输出 `Sub2API 2026.09.10.1`）。

用户指定了 tag 则直接用（仍须匹配上面正则，否则不会触发构建 workflow——直接中止并说明）。

**用 `AskUserQuestion` 弹窗确认版本号**（用户指定 tag 时跳过）：

```
question: "即将发布 sub2api <推算版本号> 到 prod，请确认或输入自定义版本号"
header: "确认发布版本"
options:
  - label: "<推算版本号>（推荐）"
    description: "自动推算的下一个版本号，直接使用"
  - label: "自定义版本号（点 Other 输入）"
    description: "若需要跳号，手动输入；必须仍是 vYYYY.MM.DD.N 格式"
multiSelect: false
```

**完成后**输出进度面板（P1 ✅，P2 🔄，其余 ⬜，tag 替换为实际值）。

---

## Phase 2 — 镜像构建

**开始时**输出进度面板（P1 ✅，P2 🔄，其余 ⬜）。

```bash
# 打 tag 前双重确认（两条都必须满足，否则停止）

# 1. 工作区必须干净——有未提交改动直接中止，不自动 commit
if [ -n "$(git status --short)" ]; then
  echo "❌ 工作区有未提交的改动，必须先提交后再发版，中止"
  exit 1
fi

# 2. 本地 main 必须已 push 到 origin/main——有未 push commit 先推
LOCAL=$(git rev-parse HEAD)
REMOTE=$(git rev-parse origin/main)
if [ "$LOCAL" != "$REMOTE" ]; then
  echo "发现本地有未 push 的 commit，先 push..."
  git push origin main
fi

git tag <tag>
git push origin <tag>        # 推 tag → 触发 GitHub Actions 镜像构建
```

```bash
# 等 10 秒让 Actions 建 run，然后定位本 tag 触发的构建
sleep 10
gh run list -R heiying0917/tokensolo-sub2api --workflow "Publish Docker image (GHCR)" -L 5
gh run list -R heiying0917/tokensolo-sub2api -L 5     # ⚠️ 顺带确认 Release(GoReleaser) 没有被这个 tag 触发

# 轮询直到完成（不要用 gh run watch | tail，管道会吞掉退出码造成假成功）
RUN_ID=<上面取到的 run id>
while :; do
  S=$(gh run view $RUN_ID -R heiying0917/tokensolo-sub2api --json status,conclusion --jq '.status + "/" + (.conclusion // "")')
  echo "$(date +%H:%M:%S) $S"
  case $S in completed/*) break;; esac
  sleep 60
done
# 以最终 conclusion 为准：completed/success 才继续；否则终止发版并报告
```

```bash
# 镜像确认（tag 含 v）
gh api "/users/heiying0917/packages/container/tokensolo-sub2api/versions?per_page=5" \
  --jq '.[].metadata.container.tags[]' | grep -x "<tag>"
# ⚠️ gh token 无 read:packages 时上面报 HTTP 403（2026-09-12 实测）——改用 manifest + 构建日志双重确认：
docker manifest inspect ghcr.io/heiying0917/tokensolo-sub2api:<tag> | jq -r '.config.digest // .manifests[0].digest'
gh run view $RUN_ID -R heiying0917/tokensolo-sub2api --log | grep -oE 'ghcr.io/heiying0917/tokensolo-sub2api:[A-Za-z0-9._-]+' | sort -u   # 应含 <tag> 与 latest
```

- 构建失败 → 停止，不部署；`gh run view $RUN_ID --log-failed` 看原因。
- 若 `gh run list` 里看到 `Release` 也被这个 tag 触发了 → `release.yml` 的负向过滤失效，立即 `gh run cancel` 它并报告用户（它会按 semver 解析日期 tag 失败或推出垃圾镜像 tag）。
- 平均约 10-15 分钟（前端 vue-tsc + vite 与 Go 编译都在 Docker 内；GHA cache 命中后更快）。

**完成后**输出进度面板（P1 ✅，P2 ✅，P3 🔄，其余 ⬜）。

---

## Phase 3 — 集群部署

**开始时**输出进度面板（P1 ✅，P2 ✅，P3 🔄，其余 ⬜）。

```bash
export KUBECONFIG=~/.kube/tokensolo.config
IMAGE=ghcr.io/heiying0917/tokensolo-sub2api:<tag>

# 记录旧镜像（回滚 / 报告用）
kubectl get deploy/sub2api -n tokensolo -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# ⚠️ 首次从旧 fork 镜像（ghcr.io/xuyang8026/sub2api）切换时，线上 Deployment 的拉取密钥还是旧 owner 的 ghcr-secret，
#    set image 不会改 imagePullSecrets，会 ImagePullBackOff。先确认，不是 ghcr-secret-heiying0917 就用一次 patch 同时换密钥与镜像（只触发一次滚动）：
kubectl get deploy/sub2api -n tokensolo -o jsonpath='{.spec.template.spec.imagePullSecrets[*].name}{"\n"}'
# kubectl patch deployment/sub2api -n tokensolo --type=strategic -p "{\"spec\":{\"template\":{\"spec\":{\"imagePullSecrets\":[{\"name\":\"ghcr-secret-heiying0917\"}],\"containers\":[{\"name\":\"sub2api\",\"image\":\"$IMAGE\"}]}}}}"

# 滚动更新（3 副本 + maxUnavailable=0 + maxSurge=1：逐个替换，新 Pod Ready 后才摘旧 Pod，全程约 25s）
kubectl set image deployment/sub2api sub2api=$IMAGE -n tokensolo
kubectl rollout status deployment/sub2api -n tokensolo --timeout=300s

# 健康检查
curl -fsS --retry 6 --retry-delay 5 --retry-connrefused https://sub2api.tokensolo.com/health
```

> 迁移在新 Pod 启动时自动执行（带 PG advisory lock），迁移完成前进程不监听端口，startupProbe 最多等 5 分钟。`rollout status` 超时通常就是迁移卡住或启动失败——看日志，别盲等。**迁移运行器成功时不打任何 INFO 日志**（只在失败 / checksum 不匹配时报错），日志里 grep 不到 `migrat` 是正常的：`Server started on` 出现 = 迁移已应用（2026-09-12 实测，迁移 238）。

**部署后立即验证（两步都要做）**：

```bash
KC=~/.kube/tokensolo.config

# 1. 日志巡检（部署后 2 分钟内）——抓 panic / 启动失败 / 迁移异常
# ⚠️ 滚动刚结束时 deployment/sub2api 可能选到 Terminating 的旧 Pod
# 注意 Terminating 中的 Pod phase 仍是 Running，field-selector 过滤不掉，必须再按 deletionTimestamp 过滤
# 3 副本：必须逐个 Pod 检查。⚠️ zsh 下 `for P in $PODS` 不分词（把三个名字当一个），要用 while read
kubectl --kubeconfig $KC get pods -n tokensolo -l app=sub2api -o json | jq -r '.items[] | select(.metadata.deletionTimestamp==null and .status.phase=="Running") | .metadata.name' > /tmp/sub2api-pods.txt
while IFS= read -r POD; do
  echo "--- $POD lines=$(kubectl --kubeconfig $KC logs -n tokensolo "$POD" --tail=300 2>&1 | wc -l | tr -d ' ')"   # 先确认日志非空
  kubectl --kubeconfig $KC logs -n tokensolo "$POD" --tail=300 2>&1 \
    | grep -iE "panic|nil pointer|runtime error|Auto setup failed|Failed to start server|checksum mismatch" | head -20
  kubectl --kubeconfig $KC logs -n tokensolo "$POD" --tail=300 2>&1 | grep -c "Server started on"   # 应为 1
done < /tmp/sub2api-pods.txt

# 2. Pod 状态确认（label 是 app=sub2api）
kubectl --kubeconfig $KC get pods -n tokensolo -l app=sub2api -o wide
```

- 健康检查 200 且日志无 panic → 继续 Phase 4
- 发现 `panic` / `nil pointer` / Pod 反复 CrashLoopBackOff / 高频 5xx → **立即执行致命红线（见 Phase 5 回滚命令）→ 复测 → 进入 Phase 5。禁止先分析日志详情。**
- 零星 `"level":"ERROR"` 业务错误（上游账号 401/429、模型不支持等）属正常波动，继续 Phase 4

**完成后必须先输出一句醒目结论**：「✅ 部署已完成，服务健康（健康检查 200 / 日志零 panic），后续仅剩冒烟测试与后台监控」——不能只靠进度面板体现。P4/P6 可能持续十几分钟且中途交互稀疏，缺这句用户会把等待期误判为流程卡死。然后输出进度面板（P1-P3 ✅，P4 🔄 即将启动）。

---

## Phase 4 — 功能冒烟测试（必做）

> ⚠️ **本 Phase 必做，不存在"按需触发"。** 发版风险不能由 commit message 字面判断，尤其本仓库经常整批 merge 上游（几十个 commit），必须用真实请求过一遍。

**开始时**输出进度面板（P1-P3 ✅，P4 🔄，P5-P6 ⬜）。

### Step 4.0 — 获取测试 API Key（必须）

**第一步**：优先从环境变量 `SUB2API_SMOKE_API_KEY` 读取（sub2api 后台生成的 `sk-` 开头 API Key）：

```bash
TOKEN="${SUB2API_SMOKE_API_KEY:-}"
# settings.local.json 里新加的 env 要重启会话才注入，为空时直接从文件读（不打印值）
[ -z "$TOKEN" ] && TOKEN=$(jq -r '.env.SUB2API_SMOKE_API_KEY // empty' .claude/settings.local.json 2>/dev/null)
if [ -n "$TOKEN" ]; then
  echo "✅ 已获取测试 key（前缀 ${TOKEN:0:6}…），直接使用"
fi
```

- **拿到值**（env 或文件）：直接使用，跳过弹窗询问，继续 Step 4.1
- **两处都为空**：用 `AskUserQuestion` 弹窗向用户索要：

```
question: "Phase 4 冒烟测试需要一个 sub2api 的真实 API Key（未找到环境变量 SUB2API_SMOKE_API_KEY，可在 .claude/settings.local.json 中预配置以后自动跳过此步）"
header: "测试 Key"
options:
  - label: "粘贴 API Key（点 Other 输入）"
    description: "推荐：用真实 key 跑一发，唯一能发现 P0 级 panic 的方式"
  - label: "跳过冒烟测试（高风险，需二次确认）"
    description: "仅在确认本次发版纯文档/纯前端时选择。跳过 = 你为线上挂掉负全责"
multiSelect: false
```

- 用户选**跳过**：在进度面板标 `P4 ⚠️ 跳过（高风险）`，必须再次显式确认"我接受跳过冒烟测试的全部风险"才能进入 P6
- 用户选**粘贴**：用提供的 key 继续 Step 4.1+

### Step 4.1 — 执行测试

```bash
BASE_URL=https://sub2api.tokensolo.com
TOKEN=<API Key>

# --- 健康检查（先确认服务可达）---
curl -s $BASE_URL/health

# --- OpenAI Chat Completions（基线必测）---
curl -s -w '\nHTTP %{http_code}\n' $BASE_URL/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}],"max_tokens":10}'

# --- OpenAI Responses（/v1/responses，Codex 使用的接口）---
curl -s -w '\nHTTP %{http_code}\n' $BASE_URL/v1/responses \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.5","input":"hi","max_output_tokens":16}'
```

**各接口成功标志**：

| 接口 | 成功标志 | 核心字段 |
|------|---------|---------|
| `GET /health` | HTTP 200 | `{"status":"ok"}` |
| `POST /v1/chat/completions` | `object: "chat.completion"` | `choices[0].message.content` |
| `POST /v1/responses` | `object: "response"` | `output[0].content[0].text` |

**判定规则（关键）**：sub2api 是号池，请求能否 200 取决于后台是否有可用账号、该 key 所属分组是否绑定了模型。

| 结果 | 判定 |
|------|------|
| 200 正常回复 | ✅ |
| 4xx 且 body 是**结构化业务错误**（无可用账号 / 模型不在分组 / 额度不足 / key 无效） | ✅ 网关路径通畅，记为"业务限制"，不算失败；换 key/模型补测到 200 更好 |
| 5xx / 空响应 / 连接拒绝 / 响应 body 含 `panic` | ❌ 致命，走红线回滚 |
| 首发（号池尚未配置账号）| 只要 `/health` 200 + 业务 4xx 结构正常即视为通过，在报告"未覆盖场景"中注明 |

从本次 commit / merge 内容提炼测试重点，追加对应场景（正向 + 边界 + 错误语义）。

**输出测试报告**（每次必须包含此表格）：

| 场景 | 请求模型/接口 | 预期 | 实际响应 | HTTP | 结论 |
|------|-------------|------|---------|------|------|
| 基线正常请求 | gpt-5.5 | 200 正常 | ... | 200 | ✅ |

附：**未覆盖场景**（列出 + 说明无法覆盖的原因）

### ⚠️ 测试中发现致命错误的处理（强制顺序，不可颠倒）

任一冒烟请求返回 panic / 持续 5xx / 100% 失败时：

1. **立即停止后续测试**，不要"再试一次"也不要"换个接口看看"
2. **立即回滚止血**（见 Phase 5 回滚命令）— 此刻 tokensolo 的 GPT 流量全挂，**禁止做任何代码分析 / 看 panic 栈 / 查日志详情**
3. **复测验证恢复**：用同样的 curl 请求重发，确认 200 + `kubectl logs` 无 panic
4. **进入 Phase 5 事故响应** — 那里才是分析根因的地方

**完成后**输出进度面板（P1-P4 ✅，P5 ⬜ 仅异常时触发，P6 🔄 即将启动）。

---

## Phase 5 — 事故响应（仅 P3/P4 发现致命错误时进入）

**进入前置条件**（必须全部满足）：
- ✅ 已回滚到上一个稳定版本（命令见下方）
- ✅ 已用真实请求复测确认集群恢复（HTTP 200 + 无 panic）
- ✅ 已在进度面板把 P3 / P4 标 ❌

如果以上任何一项未完成，**回到致命红线流程，先做完再来 Phase 5**。

### 回滚命令

```bash
export KUBECONFIG=~/.kube/tokensolo.config
kubectl rollout undo deployment/sub2api -n tokensolo
kubectl rollout status deployment/sub2api -n tokensolo --timeout=300s
# 验证恢复
curl -fsS https://sub2api.tokensolo.com/health
kubectl get deploy/sub2api -n tokensolo -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'   # 应回到旧 tag
```

> ⚠️ 回滚**不会回退数据库迁移**（forward-only）。若新版本迁移已改表结构而旧版本不兼容，回滚后仍可能报错——此时看日志确认是否迁移相关，属 Phase 5 根因分析范畴。

### Step 5.1 — 现场证据收集（集群已恢复，安全分析）

```bash
KC=~/.kube/tokensolo.config

# 1. 拉 panic Pod 的完整日志（回滚后旧 Pod 可能已终止，先取最近日志）
kubectl --kubeconfig $KC logs -n tokensolo deployment/sub2api --previous --tail=300 2>&1 \
  | grep -A 50 "panic\|Recovery\|Fatal"

# 2. 查看 Pod 事件（CrashLoopBackOff 原因）
kubectl --kubeconfig $KC describe pods -n tokensolo -l app=sub2api | grep -A 10 "Events:"

# 3. 拉 GHCR 问题版本镜像本地复现
docker pull ghcr.io/heiying0917/tokensolo-sub2api:<panic-tag>
CID=$(docker create ghcr.io/heiying0917/tokensolo-sub2api:<panic-tag>)
docker cp $CID:/app/sub2api /tmp/sub2api-panic
docker rm $CID
echo "0xXXXXXXX" | go tool addr2line /tmp/sub2api-panic

# 4. 本地复现（核心）——切到 panic commit，本地起后端用真实 key 触发
git checkout <panic-commit>
```

### Step 5.2 — 根因分析与修复

按以下顺序输出（不自作主张修代码前，先把 4 个块写完给用户看）：

```
**问题描述**
复现步骤：[具体请求 + 参数]
实际：[HTTP 状态码 + error code + message]
预期：[应该返回什么]

**影响范围**
受影响的模型 / 分组 / 场景：[...]
触发条件：[...]

**根因分析**
代码路径：[文件:行号]
关键变量：[...]
来源：[fork 自改 / 上游 merge 带入 —— 上游带入的需检查上游 issue 是否已有修复]
为什么 P1 测试没拦下：[...]（必填，用于反推 SOP 是否有缺口）

**修复方向**
[简要描述修复思路]
```

用户确认修复方向后才动手 Edit。

### Step 5.3 — 修复 + 测试覆盖 + SOP 加固

| 步骤 | 内容 |
|---|---|
| 1. 写测试**先于**写代码 | 先加一个能 catch 该 bug 的测试（regression test）。**反向验证**：把修复回退到 bug 版本，确认测试 FAIL；再恢复修复，确认 PASS |
| 2. 修复代码 | 最小化改动；上游 bug 优先 cherry-pick 上游修复 |
| 3. 加防御兜底 | 同类型路径的全局护栏 |
| 4. 更新 SOP | 如果 P1 测试范围 / Phase 顺序 / 红线有缺口，**这次必须补**，不要"下次再说" |
| 5. 重新发版 | 跑完整 6 个 Phase（包括强制 P4 冒烟），tag 用同日下一序号 |

### Step 5.4 — 事故归档

在 `../tokensolo/docs/deploy/report/` 写事故复盘 md（文件名 `YYYY-MM-DD-sub2api-<short-desc>.md`），包含：
- 时间线（精确到分钟，含挂掉总时长）
- 根因 + 误判记录（SOP / 测试 / Skill 哪里失守）
- 改进项（已落地的具体 commit / SOP 行号）

**完成后**输出进度面板（P5 ✅ 已修复），并问用户是否继续重发新版本。

---

## Phase 6 — 部署后监控（约 6 分钟异步，6 轮 × 60 秒）

**触发时机**：P4 冒烟测试成功后立即启动，不阻塞用户交互。ScheduleWakeup 异步执行，有异常时主动通知。
（P4 发现致命错误已触发回滚 → P5 事故响应，此时 P6 不启动；新版本重发后再走完整流程。）

**开始时**输出进度面板（P1-P4 ✅，P5 ⬜，P6 🔄），告知用户监控已在后台运行，无需等待。

记录部署完成时刻为监控起始时间（RFC3339，如 `2026-09-10T14:03:00+08:00`），用 TaskCreate 创建监控任务，启动 60 秒间隔循环检查（ScheduleWakeup 最小间隔 60s）。

### 主信号：kubectl logs（每轮必做）

sub2api 日志是 zap JSON，`level` 取值**大写**（`"level":"ERROR"` / `"WARN"` / `"INFO"`，2026-09-10 首发实测），与 tokensolo 的 `type:access` + `status` 字段体系不同。**每轮从监控起点查到当前**（固定起点，不用滑动窗口）：

```bash
KC=~/.kube/tokensolo.config
SINCE=<监控起始 RFC3339>
# ⚠️ 滚动刚结束时 deployment/sub2api 可能选到 Terminating 的旧 Pod
# 注意 Terminating 中的 Pod phase 仍是 Running，field-selector 过滤不掉，必须再按 deletionTimestamp 过滤
# 3 副本：把所有存活 Pod 自起点以来的日志合并到一个文件再统计（zsh 下不要 for P in $PODS）
kubectl --kubeconfig $KC get pods -n tokensolo -l app=sub2api -o json | jq -r '.items[] | select(.metadata.deletionTimestamp==null and .status.phase=="Running") | .metadata.name' > /tmp/sub2api-pods.txt
: > /tmp/sub2api-p6-all.log
while IFS= read -r POD; do kubectl --kubeconfig $KC logs -n tokensolo "$POD" --since-time=$SINCE 2>&1 >> /tmp/sub2api-p6-all.log; done < /tmp/sub2api-pods.txt
L=/tmp/sub2api-p6-all.log

# 0. sanity：日志非空（空输出 ≠ 干净）
wc -l < $L

# 1. 致命信号（任一命中 → 走红线）
grep -iE "panic|nil pointer|runtime error|Fatal" $L | head

# 2. ERROR 级日志计数 + 按 msg 聚合（看趋势与类型）
grep '"level":"ERROR"' $L | tee /tmp/sub2api-p6-errors.log | wc -l
jq -r '.msg // "?"' /tmp/sub2api-p6-errors.log 2>/dev/null | sort | uniq -c | sort -rn | head -10

# 2b. 请求量 / 客户端 5xx（status 字段可能是字串，先 tostring）+ WARN 分布
grep -c '"msg":"http request completed"' $L
grep '"msg":"http request completed"' $L | jq -r 'select((((.status // .status_code)|tostring)|tonumber? // 0) >= 500) | "\(.time[11:19]) \(.path)"'
grep '"level":"WARN"' $L | jq -r '.msg // "?"' | sort | uniq -c | sort -rn | head -6

# 3. Pod 重启计数（应保持 0）
kubectl --kubeconfig $KC get pods -n tokensolo -l app=sub2api -o jsonpath='{range .items[*]}{.metadata.name}{" restarts="}{.status.containerStatuses[0].restartCount}{"\n"}{end}'
```

### 辅助：CLS（字段已于 2026-09-10 首发实测）

- **Region** `ap-tokyo` · **TopicId** `6bd9c8ca-5939-41ee-a7a6-20100177d917` · 主题 `tke_tokensolo`（整 namespace 混合）
- **服务过滤用 `service:sub2api`**（KV 索引，与 tokensolo 的 `service:tokensolo` 同一字段）；**`pod_name` 未建索引**，`pod_name:sub2api*` 直接报 `can not search on this field`
- `level` 可做检索条件也可 SELECT/GROUP BY，值大写（`level:ERROR`）；**`msg` 不可 SELECT**（`Column 'msg' cannot be resolved`），要看内容用原始检索从 `Results[].LogJson` 取
- 默认 `tccli cls SearchLog | jq`；`--UseNewAnalysis True` 必须大写；`AnalysisRecords` 是 **JSON 字符串数组**，取值用 `jq -r '(.AnalysisRecords[0] // empty) | fromjson | .cnt'`
- 本机代理可能把 tccli 拦成 `407 Proxy Authentication Required`，`jq` 随即 parse error——**查询失败 ≠ 0 错误**。先裸调，被拦再 `rtk proxy "env -u http_proxy -u https_proxy ... no_proxy='*' tccli ..."`

```bash
TOPIC=6bd9c8ca-5939-41ee-a7a6-20100177d917
# sanity：窗口内有 sub2api 日志
tccli cls SearchLog --TopicId $TOPIC --From $FROM --To $TO --UseNewAnalysis True --Limit 5 \
  --Query 'service:sub2api | SELECT count(*) AS cnt' | jq -r '(.AnalysisRecords[0] // empty) | fromjson | .cnt'
# 按 level 分布
tccli cls SearchLog --TopicId $TOPIC --From $FROM --To $TO --UseNewAnalysis True --Limit 10 \
  --Query 'service:sub2api | SELECT level, count(*) AS cnt GROUP BY level' | jq -r '.AnalysisRecords[]? | fromjson | "\(.cnt)\t\(.level)"'
# ERROR 明细（msg 只能从原始日志取）
tccli cls SearchLog --TopicId $TOPIC --From $FROM --To $TO --UseNewAnalysis True --Limit 20 \
  --Query 'service:sub2api AND level:ERROR' | jq -r '.Results[]? | .LogJson | fromjson | "\(.time)\t\(.caller)\t\(.msg[0:120])"'
```

> CLS 在本 skill 中只是辅助（有约 1 分钟采集延迟），kubectl 为准。

### 执行流程

1. 记录监控起点 `SINCE`（RFC3339）
2. 用 TaskCreate 创建监控任务，记录起点和初始累计状态（空）；环境没有 TaskCreate 时省略，状态全部放进 ScheduleWakeup 的 prompt 里携带
3. 用 ScheduleWakeup（delaySeconds=60）启动循环，每轮 prompt 携带：起点、上轮累计错误列表、当前轮次 / 6
   - ⚠️ **每轮 ScheduleWakeup 一律传 `noop:false`**：连续 `noop:true` 的轮次输出会被终端折叠，状态行等于没发
4. 每轮唤醒后：跑主信号 0-3 → 与上轮对比提取**新增** → 相关性判断 → **输出一行状态行** → 未到第 6 轮继续 ScheduleWakeup；第 6 轮输出汇总报告，TaskUpdate 完成
5. 任一轮出现致命信号 / 疑似版本引入 → 立即报告并停止监控，进入 Phase 5

### 每轮状态行格式（必须输出，不可省略）

```
[P6 监控 轮次 N/6 | HH:MM] 累计 X 条 ERROR，重启 0 — [正常 / ⚠️ 新增 Y 条，见下方]
```

### 相关性判断准则

| 错误特征 | 判断 |
|---------|------|
| 上游账号侧错误（401/403/429、`account ... unavailable`、限流、额度耗尽） | 号池状态问题，非版本引入，记录继续 |
| 单 key / 单用户业务错误（模型不在分组、余额不足） | 用户侧问题，非版本引入，继续 |
| 与本次改动模块匹配的 error（改了调度就看调度、改了计费就看 billing） | ⚠️ 疑似本次引入，停止监控，进入 Phase 5 |
| 发版前同类 error 已持续存在（对比发版前等长窗口 `--since-time` 查旧 Pod 日志已不可得时，用 CLS 对比） | pre-existing，记录说明后继续 |
| panic / Pod 重启 / DB、Redis 连接失败 | ❌ 致命，走红线 |

### 最终汇总报告格式

```
**TKE 部署后监控报告 sub2api（HH:MM - HH:MM，6 轮累计）**

| 时间 | level | msg（聚合） | 次数 | 与本次变更相关 |
|------|-------|------------|------|--------------|
| ...  | error | ...        | ...  | ✅ 无关 / ⚠️ 疑似 / 🔵 pre-existing |

总计：X 条 ERROR，Pod 重启 0
结论：[正常 / 发现疑似版本引入问题，已进入 Phase 5 事故响应]
```

---

## 发布报告模板

**发版完成后写入 `../tokensolo/docs/deploy/report/<YYYY-MM-DD>-sub2api-tke-prod-<tag>.md`**（tokensolo 代码仓库；写完提示用户在该仓库 commit，本 skill 不代为 commit），各 `{{}}` 用实际值替换，状态列统一用 `✅ 成功 / ❌ 失败 / ⚠️ 注意 / ⏭️ 跳过`：

```markdown
# 发布报告 — sub2api {{tag}}

> 环境:prod · 集群:cls-tokensolo-tke-japan / ns tokensolo · 时间:{{YYYY-MM-DD HH:MM}}

## 概览
| 项 | 值 |
|----|----|
| 服务 | sub2api（GPT 号池上游） |
| 环境 | prod |
| 版本 tag | {{tag}} |
| 镜像 | ghcr.io/heiying0917/tokensolo-sub2api:{{tag}} |
| 上游基线 | {{backend/cmd/server/VERSION 内容，如 0.2.18}} |
| 发布结果 | {{✅ 成功 / ❌ 失败(已回滚)}} |

## 各阶段结果
| 阶段 | 状态 | 详情 |
|------|------|------|
| 合并 main | {{状态}} | {{无冲突 / 已解决冲突: 文件}} |
| commit | {{状态}} | {{commit hash / 无新提交}} |
| 打 tag & push | {{状态}} | {{tag}} |
| 镜像构建 (Actions) | {{状态}} | run-id {{id}}，耗时 {{x}}s；Release workflow 未触发 {{✅}} |
| 集群部署 | {{状态}} | {{rollout 摘要}} |
| 健康检查 | {{状态}} | https://sub2api.tokensolo.com/health → HTTP {{code}} |
| 冒烟测试 | {{状态}} | {{接口 / 业务限制说明 / 跳过原因}} |
| 部署后监控 | {{状态}} | {{6 轮累计 error 数 / 结论}} |

## 部署详情
| Deployment | 旧镜像 | 新镜像 | rollout |
|-----------|--------|--------|---------|
| sub2api | {{old-tag}} | {{new-tag}} | {{状态}} |

## 验证
| 检查项 | 结果 |
|--------|------|
| Pod Running / 重启 0 | {{✅ 1/1}} |
| 镜像 tag 已更新 | {{状态}} |
| 日志无 panic / 迁移无异常 | {{✅ / ⚠️ 发现: ...}} |
| API 冒烟 | {{✅ / ⚠️ 跳过（高风险）}} |

## 回滚记录（仅失败时填，成功则写"无"）
| 项 | 值 |
|----|----|
| 回滚原因 | {{reason}} |
| 回滚结果 | {{✅ 已恢复到 prev-tag}} |
```

---

## 注意事项

1. **发版基于 main 分支**——打 tag 前确认本地 main 与 origin/main 一致（Phase 2 自动检查并 push）。
2. **两套 tag 共存**：日期 tag（本 skill）触发 `Publish Docker image (GHCR)`；semver tag 触发上游 `Release`（GoReleaser）。日常只打日期 tag；merge 上游时 `git fetch upstream --tags` 带进来的 `v0.2.x` 不要 push 到 origin。
3. **manifests / 报告都在 `../tokensolo/` 仓库**（tokensolo 代码仓库的本机目录）：首次部署、改 ConfigMap、切 SKIP_SETUP、从旧 fork 镜像切换见 `deploy/k8s/prod/README.md`。
4. **发版路径**：主路径用本 skill（含 Phase 4 冒烟 + Phase 6 监控）。`tke-manual-release.yml` 仅保留 `workflow_dispatch` 作为紧急备用（需仓库 secret `KUBECONFIG_TKE`，与 tokensolo-cpa 同一份），无冒烟，用后补测。
6. **与旧 fork 的关系**：旧 fork `XuYang8026/sub2api`（镜像 `ghcr.io/xuyang8026/sub2api`）的发版历史与事故复盘仍在 `../tokensolo/docs/deploy/report/`，本仓库从 `v2026.10.23.1` 起接管；不要再在旧 fork 目录打 tag。
5. 与 tokensolo 的 tke-release skill 保持结构一致；通用坑（gh run watch、noop、RTK hook、tccli 代理）两边同步更新。

---

## 易踩坑速查

| 坑 | 正确做法 |
|----|---------|
| 本地 tag 过时，版本号推算错 | Phase 1 必须先 `git fetch --tags` |
| 推算 tag 时被上游 semver tag（`v0.2.18`）干扰 | grep 只认 `^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9]+$` |
| 打 semver 格式 tag | 触发的是 GoReleaser，镜像 tag 不含 v，`set image` 会 ImagePullBackOff。只用日期格式 |
| 首次切新 fork 镜像后 ImagePullBackOff | 线上 Deployment 拉取密钥仍是旧 owner 的 `ghcr-secret`；按 Phase 3 用 patch 同时换成 `ghcr-secret-heiying0917` 与新镜像 |
| 镜像 tag 写成不含 v 的 `2026.10.23.1` | 本 skill 的镜像 tag **含 v**：`ghcr.io/heiying0917/tokensolo-sub2api:v2026.10.23.1` |
| 日期 tag 同时触发了 `Release` workflow | `release.yml` 负向过滤失效——`gh run cancel` 它，修过滤，再继续 |
| 有未提交改动就打 tag | Phase 2 强制检查工作区干净，否则中止 |
| 本地 commit 未 push 就打 tag | Phase 2 自动 push main 后再打 tag |
| merge 上游产生冲突自行处理 | 不确定的冲突立即停止，把冲突段落列给用户确认解法 |
| 未等 GitHub Actions 完成就部署 | 轮询 `gh run view --json status,conclusion` 到 `completed/success` 才执行 Phase 3 |
| `gh run watch ... \| tail` 网络错误变假成功 | 管道让 tail 的退出码顶替了 gh 的。改用轮询 `gh run view`，以 conclusion 为准 |
| `kubectl apply -f prod/sub2api/` 当发版 | 会把镜像重置为清单里的初始 tag。发版只 `set image`；改配置只 apply `configmap.yaml` |
| `kubectl exec ... curl` 报 not found | 镜像只有 busybox `wget`：`wget -qO- http://127.0.0.1:8080/health` |
| `rollout status` 超时就盲等或直接回滚 | 先看 `kubectl logs`：迁移中（正常，等）vs `Auto setup failed`/CrashLoop（回滚） |
| 回滚后仍报错以为回滚失败 | 迁移 forward-only 不会回退；看是否表结构不兼容，属 Phase 5 |
| Phase 4 跳过冒烟测试 | 必做。用 `AskUserQuestion` 主动索要 key，不要被动等用户给 |
| 首发号池没账号，冒烟 4xx 判失败 | 结构化业务 4xx = 网关通畅，✅；只有 5xx/panic/连接拒绝才是失败 |
| 发现 panic 后先去拉栈分析 | **立即 rollout undo 止血**！证据回滚后照样能拿到，分析放 Phase 5 |
| 日志 grep panic 无输出就判「无 panic」 | 先 `wc -l` 确认日志非空；selector/容器名写错时 grep 也是空 |
| `kubectl get pods -l app=sub2api-xxx` 查出空 | label 是 `app=sub2api` |
| P6 用滑动时间窗口 | 固定起点 `--since-time=$SINCE` 累计查到当前 |
| P6 监控轮传 `noop:true` 导致用户看不到状态行 | 每轮一律 `noop:false`；ScheduleWakeup 最小间隔 60s |
| P3 部署成功后只更新进度面板 | 必须先输出醒目的「✅ 部署已完成」声明再贴面板 |
| P1 `make test-unit \| grep` 空输出当全绿 | RTK hook 会压缩 go test 输出；看退出码或 `rtk proxy` 取原始输出 |
| CLS 用 `pod_name:sub2api*` 过滤 | `pod_name` 未建索引，直接报 QueryError。服务过滤用 `service:sub2api`；`msg` 不可 SELECT，要看内容走原始检索 `Results[].LogJson` |
| grep `"level":"error"` 小写拿到 0 | zap level 是大写：`"level":"ERROR"` / `WARN`；CLS 检索同理 `level:ERROR` |
| 滚动刚结束 `kubectl logs deployment/sub2api` 看到的是旧 Pod 日志 | 选到了 Terminating 的旧 Pod。`--field-selector=status.phase=Running` **过滤不掉它**（终止中 phase 仍是 Running），要用 `-o json \| jq 'select(.metadata.deletionTimestamp==null)'`（2026-09-10 v2026.09.10.2 踩到） |
| P4 `$SUB2API_SMOKE_API_KEY` 为空就弹窗要 key | `settings.local.json` 新加的 env 当前会话不生效；先 `jq -r '.env.SUB2API_SMOKE_API_KEY' .claude/settings.local.json` 兜底再弹窗（2026-09-10） |
| CLS `AnalysisRecords[0].cnt` 报 Cannot index string | 元素是 JSON 字符串：`(.AnalysisRecords[0] // empty) \| fromjson \| .cnt` |
| 本机代理把 tccli 拦成 407 | 先裸调；确认 407 再 `rtk proxy "env -u http_proxy -u https_proxy ... no_proxy='*' tccli ..."` |
| skill 改完 `git status` 看不到 | 上游 .gitignore 忽略 `.claude`，本 fork 已反选 `.claude/skills/**`；若被上游 merge 覆盖回去，重新加反选 |
| 发布报告写到本仓库 `docs/` | 被上游 .gitignore 忽略，写到 `../tokensolo/docs/deploy/report/` |
| 在旧 fork `sub2api/` 目录里打 tag | 旧 fork 已停用，镜像推到旧 owner；只在 `tokensolo-sub2api/` 操作 |
| zsh 下 `for P in $PODS` 把三个 Pod 名当一个字符串 | kubectl 报 NotFound、日志计数恒 0、`fatal=none` 是假的。用 `while IFS= read -r P; do …; done < /tmp/sub2api-pods.txt` 逐 Pod 处理（2026-09-11、09-12 两次踩到） |
| Deployment 按 ×1 单副本理解 | 实际 **3 副本** maxSurge=1 / maxUnavailable=0，P3 / P6 都要覆盖全部 Pod |
| GHCR versions API 报 `403 read:packages` | gh token 无该 scope；用 `docker manifest inspect` + `gh run view --log` grep 推送 tag 兜底（2026-09-12） |
| 日志 grep 不到迁移行就怀疑迁移没跑 | 迁移运行器成功时静默，只在失败时报错；`Server started on` 出现即迁移已应用（2026-09-12） |
| CLS 用 `status:502` 做条件恒得 0 | `status` 字段未建索引；客户端 5xx 用 kubectl 日志里 `"msg":"http request completed"` 的 status 字段统计（2026-09-12） |
| tccli 报 `ClientNetworkError` / `SSL: UNEXPECTED_EOF` | 网络抖动，不是 407；重试 2-3 次（含去代理版本），仍失败则以 kubectl 为准继续，不阻塞轮次；jq 解析 `LogJson` 会因栈里的换行报错，改用 grep -o 取字段（2026-09-12） |
| P4 等用户确认拖了数小时，P6 第 1 轮窗口覆盖几小时 WARN | 起点仍固定为 rollout 完成时刻不动；量级判断用 CLS 与部署前**等长**窗口对比（`service:sub2api AND level:WARN`、`msg:"…"` 均可做条件），同量级 = pre-existing（2026-09-12） |
| kubectl 报 `Unable to connect to the server: EOF`，本轮日志行数骤降 | 本机到 TKE API Server 链路抖动，不是服务问题（health 仍 200）。合并日志少了某个 Pod 时计数全部作废：逐 Pod 重试 3-4 次、确认每个 Pod 都有输出（`[ -s file ]`）再统计（2026-09-12 P6 第 5 轮踩到） |
| 脚本里 `for path in …` 之后 curl/kubectl 全部 `command not found` | zsh 的 `path` 是 `PATH` 的数组别名，给它赋值会清空 PATH。循环变量换别的名字（如 `route`、`ep`）（2026-09-12 v2026.09.12.2 P3 踩到） |

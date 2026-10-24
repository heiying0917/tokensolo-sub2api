# sub2api（GPT 号池上游）TKE 部署清单

sub2api 作为 tokensolo 的 **GPT 号池上游渠道**，地位等同 tokensolo-crs（Claude 号池）。
本目录（`tokensolo-sub2api/deploy/k8s/prod/`，随 sub2api 仓库单独维护，2026-10-24 从 tokensolo 仓库 `docs/deploy/k8s/manifests/prod/sub2api/` 迁入）
是它在 TKE 集群 `cls-tokensolo-tke-japan` / ns `tokensolo` 的部署清单；日常发版走本仓库的 `/tke-release` skill，
本文只覆盖**首次部署**与运维要点。

| 项 | 值 |
|---|---|
| 源码仓库 | `heiying0917/tokensolo-sub2api`（fork 自 Wei-Shaw/sub2api，基线 0.2.18；2026-10-23 起替代旧 fork `XuYang8026/sub2api`） |
| 镜像 | `ghcr.io/heiying0917/tokensolo-sub2api:<tag>`，tag 格式 `vYYYY.MM.DD.N`（含 v，与 tokensolo 一致） |
| 拉取密钥 | `ghcr-secret-heiying0917`（与 cpa 共用，见第 7 节） |
| Deployment / Service / 容器名 | `sub2api` × 3，Pod label `app=sub2api`，端口 8080 |
| 健康端点 | `GET /health` → `{"status":"ok"}`（纯进程存活，不查 DB） |
| 对外域名 | `https://sub2api.tokensolo.com`（Ingress 是所有服务共用的，仍在 tokensolo 仓库 `docs/deploy/k8s/manifests/prod/ingress/ingress-all.yaml`） |
| 集群内地址（tokensolo 渠道推荐） | `http://sub2api.tokensolo.svc.cluster.local:8080` |
| 数据层 | PG：TencentDB `172.16.1.76` 独立 database `sub2api`；Redis：TencentDB `172.16.1.111` **DB=2** |

Redis 实例 DB 分配（三服务共用 1GB `noeviction` 实例，扩容只能升规格）：

| DB | 服务 |
|---|---|
| 0 | tokensolo-crs |
| 1 | tokensolo（含 `preconsume:*` 资金账本） |
| 2 | sub2api |

## 1. 数据库预备（以 PG 实例管理员执行，一次性）

预建库可绕开应用 `CREATE DATABASE` 的权限需求；`pg_trgm` 供模糊搜索索引使用（缺失时迁移只降级不失败，但建议预装）。

```sql
CREATE USER sub2api WITH PASSWORD '<强密码>';
CREATE DATABASE sub2api OWNER sub2api;
\c sub2api
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

可用临时 debug Pod 连库（同 ops-runbook 做法）：

```bash
export KUBECONFIG=~/.kube/tokensolo.config
kubectl run pg-client --rm -it --image=postgres:18-alpine -n tokensolo -- \
  psql "postgres://<管理员>:<密码>@172.16.1.76:5432/postgres"
```

## 2. Secret（`secret.yaml`，git-ignored，本目录手工创建）

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: sub2api
  namespace: tokensolo
type: Opaque
stringData:
  DATABASE_HOST: "172.16.1.76"
  DATABASE_USER: "sub2api"
  DATABASE_PASSWORD: "<第 1 步的密码>"
  REDIS_HOST: "172.16.1.111"
  REDIS_PASSWORD: "<与 tokensolo/crs 相同的 Redis 密码>"
  JWT_SECRET: "<openssl rand -hex 32，≥32 字节>"
  TOTP_ENCRYPTION_KEY: "<openssl rand -hex 32>"
  ADMIN_PASSWORD: "<后台管理员初始密码>"
```

注意：

- **`JWT_SECRET` 只在首发写入**：应用把它持久化到 DB 表 `security_secrets`，之后以 DB 值为准，改 Secret 不生效（需改库）。
- **`ADMIN_PASSWORD` 必须常驻**：users 表为空时应用会自动生成随机密码打到 stdout；有任何用户后 `ADMIN_EMAIL`/`ADMIN_PASSWORD` 都不再起作用。
- `TOTP_ENCRYPTION_KEY` 不固定则每次重启随机，管理员 2FA 会失效。

## 3. 首次部署（两阶段）

```bash
export KUBECONFIG=~/.kube/tokensolo.config
# 在 tokensolo-sub2api 仓库根目录执行；Ingress 在 tokensolo 仓库（同级目录 ../tokensolo）
M=deploy/k8s/prod
INGRESS=../tokensolo/docs/deploy/k8s/manifests/prod/ingress/ingress-all.yaml

# ① 首发：AUTO_SETUP 建表 / 迁移 / 建管理员
kubectl apply -f $M/   # secret.yaml 是 git-ignored 的本机文件，须先按第 2 节创建
kubectl rollout status deploy/sub2api -n tokensolo --timeout=300s
kubectl logs -n tokensolo deploy/sub2api | grep -E "Auto setup completed|Admin user|Server started"
kubectl exec -n tokensolo deploy/sub2api -- wget -qO- http://127.0.0.1:8080/health   # 镜像只有 wget 没有 curl

# ② 切稳态：SKIP_SETUP=true（走主路径迁移：10 分钟超时 + 退避重试；不再连维护库、不再落盘密码）
#    编辑 configmap.yaml 把 SKIP_SETUP 改为 "true" 后：
kubectl apply -f $M/configmap.yaml
kubectl rollout restart deploy/sub2api -n tokensolo
kubectl rollout status deploy/sub2api -n tokensolo --timeout=300s

# ③ 域名：DNS 加 CNAME  sub2api.tokensolo.com → lb-5j4mtrgo-ufy36l5ktjhxgaqo.clb.jp-tencentclb.com
#    生效后 apply ingress，cert-manager HTTP-01 自动签发
kubectl apply -f $INGRESS
kubectl get certificate -n tokensolo sub2api-tls      # READY=True 后即可 https 访问
curl -fsS https://sub2api.tokensolo.com/health
```

注意：tokensolo 仓库的 `docs/deploy/k8s/scripts/bootstrap.sh prod` 只在目录存在时 apply sub2api，清单迁出后**不再包含 sub2api**；全新集群需在 bootstrap 之后按本节单独部署。

## 4. 运营访问与渠道配置

- 后台：`https://sub2api.tokensolo.com`，用 `ADMIN_EMAIL` / `ADMIN_PASSWORD` 登录；证书未就绪前临时
  `kubectl port-forward -n tokensolo svc/sub2api 18080:8080` 后访问 `http://localhost:18080`。
- tokensolo 渠道 Base URL 推荐集群内地址 `http://sub2api.tokensolo.svc.cluster.local:8080`（省一跳 CLB，不依赖公网证书）；API Key 在 sub2api 后台生成。

## 5. 日常运维

```bash
export KUBECONFIG=~/.kube/tokensolo.config
kubectl get pods -n tokensolo -l app=sub2api -o wide
kubectl logs -n tokensolo deploy/sub2api --tail=200 | grep -E '"level":"ERROR"|panic'   # zap level 为大写
kubectl rollout undo deploy/sub2api -n tokensolo          # 回滚上一版
kubectl scale deploy/sub2api -n tokensolo --replicas=N    # 扩缩容（改完记得同步 deployment.yaml 的 replicas）
```

日志随 CLS 采集规则 `tokensolo-tke`（整 namespace）进入主题 `tke_tokensolo`，按 **`service:sub2api`** 过滤（`pod_name` 未建索引；`level` 大写且可 SELECT，`msg` 不可 SELECT 只能从原始日志取）。

## 6. 已知限制

- 上游优雅停机硬编码 5s（`backend/cmd/server/main.go`），滚动更新时超过 5s 的在途流式请求会被切断（3 副本 + maxSurge=1 逐个滚动，影响面为 1/3）。
- 多实例安全性（已核实）：迁移 PG advisory lock；OAuth token 刷新走 `OAuthRefreshAPI` Redis 分布式锁 `oauth:refresh_lock:*`；重型后台任务（聚合/清理/备份/告警/订阅到期等）用 Redis leader lock 单飞；并发计数、会话限制、调度缓存在 Redis；设置变更经 outbox 跨实例失效。未加锁的轻量定时任务（账号/代理到期标记、幂等键清理等）是幂等的，多跑无害。
- `kubectl apply -f deploy/k8s/prod/` 会把镜像重置为 `deployment.yaml` 里的初始 tag；日常只用 `kubectl set image`（`/tke-release` 已封装），切 ConfigMap 时只 apply `configmap.yaml`。

## 7. 切换到新 fork 镜像（2026-10-23，一次性）

sub2api 的源码仓库从 `XuYang8026/sub2api` 换到 `heiying0917/tokensolo-sub2api`，镜像前缀随之变为 `ghcr.io/heiying0917/tokensolo-sub2api`。
线上 Deployment 当前仍跑旧 fork 镜像、拉取密钥仍是旧 owner 的 `ghcr-secret`，首次切换要注意两点：

- **不要 `kubectl apply -f deploy/k8s/prod/` 整目录**（第 6 节：会把镜像重置为 `deployment.yaml` 里的初始 tag，且 ConfigMap 的 `SKIP_SETUP` 状态可能被覆盖），只改 Deployment 的镜像与拉取密钥。
- **`kubectl set image` 不会改 `imagePullSecrets`**。`ghcr-secret-heiying0917` 已由 cpa 首发时在同 namespace 创建，直接复用；但旧 `ghcr-secret` 拉不到新 owner 的私有包，只换镜像不换密钥会 `ImagePullBackOff`。

```bash
export KUBECONFIG=~/.kube/tokensolo.config
TAG=<tag>    # 新 fork 的首个日期 tag，如 v2026.10.23.1
IMAGE=ghcr.io/heiying0917/tokensolo-sub2api:$TAG

kubectl get secret -n tokensolo ghcr-secret-heiying0917                                   # 必须已存在（cpa 创建）
kubectl get deploy/sub2api -n tokensolo -o jsonpath='{.spec.template.spec.imagePullSecrets[*].name}{"\n"}'

# 方式 A（推荐）：一次 patch 同时换密钥与镜像，只触发一次滚动
kubectl patch deployment/sub2api -n tokensolo --type=strategic \
  -p "{\"spec\":{\"template\":{\"spec\":{\"imagePullSecrets\":[{\"name\":\"ghcr-secret-heiying0917\"}],\"containers\":[{\"name\":\"sub2api\",\"image\":\"$IMAGE\"}]}}}}"

# 方式 B：密钥已经是 ghcr-secret-heiying0917 时（后续日常发版），只需 set image，/tke-release 已封装
kubectl set image deployment/sub2api sub2api=$IMAGE -n tokensolo

kubectl rollout status deploy/sub2api -n tokensolo --timeout=300s
curl -fsS https://sub2api.tokensolo.com/health
```

切换完成后把 `deployment.yaml` 的 `image` 同步为实际 tag（本目录已预置 `ghcr-secret-heiying0917` 与占位 tag），后续发版照常走新 fork 的 `/tke-release`。

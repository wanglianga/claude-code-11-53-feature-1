# AgriSync — 作业数据跨地域同步与出境审计服务

跨国农机运营集团的作业数据平台。播种机、收割机、施肥机在各区域上传作业轨迹、地块面积、油耗与设备告警；
**区域节点本地确认**，复制编排器按**数据类别 × 驻留政策**生成跨区副本，聚合服务**在区域内**计算可出境指标，
出境审计服务在**交付前**重新核验授权与范围，容灾登记册记录每个区域的**主备角色**和**最后已确认复制位置**。

技术栈：Go 1.26 + PostgreSQL 18。后端服务，提供 OpenAPI 3.0.3 文档与内置（离线打包）Swagger UI。

---

## 1. 设计要点

### 1.1 区域隔离是物理的

控制面库 `agrisync_control` 与四个区域库 `agrisync_region_{de_bb,br_mt,us_ia,fr_nor}` 是**独立数据库**。
区域写入只触碰本区域库，控制面库永远不存受限原始轨迹（唯一例外是政策显式允许出境的区域）。因此
"集团只能取得经批准的聚合结果" 不是靠查询过滤，而是靠数据库边界。

### 1.2 本地确认、政策过滤后的异步复制

一次上传在**一个本地事务**里完成：作业记录 + 本地日志（gapless 序列）+ 事务性 outbox + 回执。

- outbox 按类别拆事件：`operation_summary`、`raw_trajectory`、`device_alarm`、`aggregate_metric`；
- 复制按 `residency_policies(region, category, outbound_allowed)` 逐事件过滤，受限轨迹**连控制面都进不去**；
- 副本先落 `unconfirmed`，接收方确认后才变 `confirmed` 并对集团可见，同时推进 DR 登记册的
  `last_confirmed_seq` 与复制游标 `confirmed_seq`；
- 复制失败/链路断开时游标不动，重试从同一位置继续。所有副本写入都是 **upsert**（主键 + `(region, device, segment)` 唯一索引），
  所以"至少一次"重投不会让面积重复累加。

### 1.3 重复上传按"设备序号 + 区域来源"去重

- 片段级：`unique (device_serial, segment_id)`，作用域是接收区域（区域来源隐含在库边界内，跨区副本显式带 `source_region`）；
- 批次级：`(batch_id, 内容 SHA-256)` 相同则返回原始回执（`replayed_batch=true`），内容不同则 `409`；
- 断网补传时重传已接收片段 → `duplicates` 计数，面积/油耗不变。

### 1.4 断网期间农场继续记账，集团统计显式滞后

- 链路关闭（`POST /replication/links/{region}` 或故障注入）不影响本区域写入；
- 聚合报告的 `coverage` 明确列出 `included_regions`、`excluded_regions`、`stale_regions`（延迟区域）、
  每区域 `data_cutoff` / `lag_seconds`、以及所使用的 `replication_seq`（复制版本）；
- 未确认区域不计入合计，只作为"被排除且滞后"出现。

### 1.5 故障切换只从"已确认位置"继续

`POST /dr/{region}/failover`：

1. 提升备节点，`epoch+1`；
2. 删除该区域所有 `unconfirmed` 副本（旧主未确认的缓存**不得**被当作已发布数据）；
3. 复制游标回绕到 DR 登记册的 `last_confirmed_seq`，状态 `pending_reconcile`；
4. 本地日志/本地记账**不截断**——新主节点继续接受现场写入；
5. 续传从 `last_confirmed_seq + 1` 开始，因 upsert 幂等，不会重复计入。

### 1.6 审批撤回后交付必须重新核验

`POST /aggregates/{run}/deliver` 在**发送前**重新读取当前审批（状态/版本/有效期/区域范围/指标范围）与当前驻留政策，
并对**将要发送的确切字节**做结构扫描（禁止 `lat/lon/points/trajectory/...`）。任一检查失败：

- 交付状态 `blocked`，`artifact=null`、`artifact_sha256=""`（**不返回任何聚合内容**）；
- 写入 `delivery.blocked` 审计（`decision=deny`，含失败检查项与原因）；
- 同一聚合运行不能二次交付（`aggregate_run` 已达 `delivered` 时返回 `409`）。

### 1.7 审计链可证明"某份跨境报表从未包含受限原始轨迹"

- 出境文件按**原始字节**存 `text`（不是 `jsonb`，避免键重排/数字归一化破坏哈希），交付时记录 SHA-256；
- `GET /deliveries/{id}/verify` 重新计算哈希、扫描受限字段、校验对应审计条目与整条哈希链；
- 审计账本为哈希链（`prev_hash` + 规范化 payload），落库前对 `detail` 做 JSON 归一化、时间截断到微秒，
  保证"写入时哈希 == 读回后重算哈希"；任何越权改库都会被 `GET /audit/verify` 定位到具体行号。

### 1.8 区域内空间聚合：原始轨迹不出境，县级/网格粒度可证明

部分国家禁止精确设备轨迹离境，但集团仍需按县比较播种、收割、施肥的已完成面积。空间聚合**全部在区域库内完成**，
集团只能取得获准粒度（县级或批准分辨率网格）的汇总及其不确定范围：

- **空间目录与政策同一来源**：`boundary_versions`（带 SHA-256 的版本化行政边界）+ `spatial_policies`
  （边界版本 × 分配方法 × 输出粒度的唯一绑定）；目录、切段/栅格、出境聚合与审计交付引用同一行政策与边界版本。
- **两种区域内分配方法**（纯 Go 几何引擎 `internal/geo`，无 PostGIS 依赖）：
  - `intersection`：轨迹折线与批准行政边界**精确相交**切段，按段长 × 作业宽度分摊面积，解释细、适合地块少的精确记录；
  - `raster`：在批准分辨率（最小 100 m）上栅格化累计网格覆盖，批量汇总更快、输出粒度易审计，县界/窄地块处存在量化误差。
- **不确定范围不猜边**：每个采样点带定位精度半径；不确定圆盘跨越多县（或触到边界）的片段标记 `uncertainty_crosses_boundary`
  并列为**待分配**，同时给出候选县与县级保守上界 `area_ha_max`；作业宽度缺失 → `working_width_missing` 待分配，
  补传宽度后重算；边界版本无效 → 全部面积 `boundary_version_invalid` 待分配；同一片段重复接收 → 去重日志计数，绝不加倍。
- **出境产物消毒**：outbox 新增 `spatial_summary` 类别，复制仍逐事件按驻留政策过滤；产物只含县码/不透明网格号
  （网格地理锚点留在区域内）、面积/时长/覆盖长度、待分配原因与边界/政策哈希，**不含顺序点、坐标或未批准细网格**；
  交付前对确切字节再做受限字段扫描与哈希校验（`POST /spatial/deliveries`、`GET /spatial/deliveries/{id}/verify`）。
- **修订可追溯**：边界更正上传新版本（相同几何幂等返回原版本），旧版本 `superseded` 保留；重算生成新摘要并
  `supersedes` 链接旧摘要，历史集团报表仍能说明当时采用的边界版本与粒度。

---

## 2. 快速启动

### 2.1 Docker Compose（一键）

```bash
docker compose up --build
# API:        http://localhost:8080
# Swagger UI: http://localhost:8080/docs
# OpenAPI:    http://localhost:8080/openapi.json
```

Compose 起 `postgres:18-alpine`（`ccdev/ccdev`，映射 5432）与 API 容器；API 启动时自动建库、建表、写入区域/政策/DR 登记册。
镜像地址可通过 `POSTGRES_IMAGE`、`GO_IMAGE`、`RUNTIME_IMAGE` 覆盖以适配镜像代理：

```bash
GO_IMAGE=<mirror>/golang:1.26-bookworm RUNTIME_IMAGE=<mirror>/distroless-static:nonroot docker compose up --build
```

### 2.2 本地开发（容器内已预装运行时）

```bash
# 依赖：PostgreSQL 18 已在 127.0.0.1:5432（库/用户/密码均为 ccdev）
/opt/postgresql-18/bin/pg_isready -h 127.0.0.1 -p 5432
export PATH=/opt/go/bin:$PATH
go build ./...
go run ./cmd/server            # 默认 :8080

# 可选：后台每 5s 自动复制（默认关闭，保证回放脚本确定性）
REPLICATION_INTERVAL=5s go run ./cmd/server
```

无 Docker 时同样可用 `make run`。数据库账号仅用于隔离开发/测试，不作为生产配置。

### 2.3 一键验证

```bash
make verify            # go build + vet + 单元测试 + 起临时服务 + 161 项验收断言
# 或对已运行实例：
./scripts/replay.sh http://localhost:8080
```

`scripts/replay.sh` 输出逐条 `PASS/FAIL`，失败时退出码非 0。`scripts/demo.sh` 是给人读的分阶段演示（不做断言）。

回放脚本有少量断言需要直接查库（去重后的面积、副本行数、越权改库），默认使用
`/opt/postgresql-18/bin/psql` 连接 `127.0.0.1:5432`（容器内预装）；在其他环境运行时可覆盖：
`PSQL=/usr/bin/psql PGHOST=... PGPORT=... PGUSER=... PGPASSWORD=... ./scripts/replay.sh http://localhost:8080`。
在 Docker Compose 场景下把 `PGHOST` 指向映射出来的 5432 即可（`docker compose up` 已映射）。

---

## 3. 测试账号（Bearer Token）

除 `/healthz`、`/openapi.json`、`/docs`、`/swagger-ui/*` 外，所有接口都需要 `Authorization: Bearer <token>`。

| 角色 | Token | 权限 |
| --- | --- | --- |
| 平台管理员 `admin` | `demo-admin-token` | 链接开关、批量复制、主备切换、重置演示数据 |
| 集团分析 `group_analyst` | `demo-analyst-token` | 提交出境申请、计算聚合、交付；**无**区域原始记录读取通道 |
| 数据管理员 `approver` | `demo-approver-token` | 审批 / 驳回 / 撤回；**不能**自行提交申请或交付 |
| 审计员 `auditor` | `demo-auditor-token` | 只读：审计、校验 |
| 区域操作 `region_operator` | `demo-operator-de-bb` | 仅 DE-BB 的写入与本地查询 |
| 区域操作 | `demo-operator-br-mt` | 仅 BR-MT |
| 区域操作 | `demo-operator-us-ia` | 仅 US-IA |
| 区域操作 | `demo-operator-fr-nor` | 仅 FR-NOR |

Swagger UI 中点击 **Authorize** 填入上述任一 token 即可调用。

---

## 4. 样例数据（`samples/`）

| 文件 | 用途 |
| --- | --- |
| `regions/de-bb.json` 等 4 个 | 每区域首批 4 个作业片段（含轨迹与告警） |
| `offline/br-mt-offline-backfill.json` | BR-MT 断网期间继续作业的 3 个片段（本地提交，链路恢复后补传） |
| `offline/br-mt-retry-overlap.json` | 断网补传时**重叠重传**：3 个已接收片段 + 1 个新片段（验证去重与面积不重复） |
| `dr/us-ia-post-switch.json` | 主备切换后在新主节点继续上传 |
| `approvals/wide.json` | 四区域 × 全指标的出境申请 |
| `approvals/narrow.json` | 三区域（DE-BB/BR-MT/US-IA）× 3 指标的窄范围申请 |
| `approvals/spatial.json` | BR-MT × `spatial_area_ha` 的空间出境申请 |
| `spatial/br-mt-boundaries-v1.json`、`-v2.json` | BR-MT 两县边界目录 v1 与修订版 v2（县界东移 0.001°） |
| `spatial/br-mt-batch-a.json` | 跨县直线、县界低精度（60 m）、缺作业宽度三个片段 |
| `spatial/br-mt-batch-b.json` | 重传 SP-CROSS-1（去重）+ 新增 SP-CLEAN-2 |
| `spatial/policy-*.json` | 精确相交/县级、栅格/县级、栅格/网格、边界 v2 四套空间政策 |

重新生成：`python3 scripts/gen_samples.py`、`python3 scripts/gen_spatial_samples.py`。

---

## 5. 验收回放（`scripts/replay.sh`）

脚本对运行中的服务做**可断言**的回放，覆盖题目要求的四类验收场景与审计链：

| 场景 | 断言示例 |
| --- | --- |
| 区域断网 | 断网后本地仍 `accepted=3`、日志 `entries=head=7` 且 `gapless=true`、`pending_outbox=7`；集团聚合 `excluded_regions/stale_regions = BR-MT`，合计只含其余三区 |
| 复制重试 | 重叠重传 `duplicates=3`；失败注入时游标 `confirmed_seq` 不动、`retry_count` 递增；重试成功后集团副本面积/行数 **等于** 区域自身面积/行数 |
| 主备切换 | 注入确认失败→存在 4 行 `unconfirmed`；failover 丢弃这 4 行、`resumed_from_seq == DR last_confirmed_seq`、新主 `epoch=2`、本地日志仍 `gapless`、续传后面积无重复；另注入一行崩溃写入者的孤儿副本，切换后同样被清除且计入报告 |
| 审批撤回 | 撤回后交付 `status=blocked`、`artifact=null`、无 `artifact_sha256`、原因为 `approval_status`；窄范围申请越权指标在聚合阶段 `403` |
| 出境审计 | 存储字节重算哈希一致、`restricted_fields` 为空、审计条目与整链校验通过；**越权改库后** `audit/verify` 返回 `valid=false` 且 `broken_at` 指向被改行 |
| 权限边界 | 无/错 token 401；区域操作员跨区 403；分析员自批 403；操作员触发全局复制/切换 403；集团分析读取区域原始记录 403；审计员可读区域记录用于审计 |
| 区域内空间聚合 | 跨县直线按长度×宽度分摊入两县；低精度界片段待分配并列出双候选县；宽度缺失待分配、补传后重算；重复片段去重不加倍；边界修订生成链接新摘要、旧摘要保留原粒度；无效边界全部待分配；栅格策略只出批准网格、县级策略不出网格；集团副本字节无 `lat/lon/points` 键；空间交付哈希/受限字段/审计链校验通过，撤回后阻断且 artifact 为 null |

一次完整运行的实际结果（`make verify`，2026-11-20 在本容器内执行）：

```
checks passed: 161, failed: 0
ALL CHECKS PASSED
```

---

## 6. API 一览

完整定义见 `GET /openapi.json`（OpenAPI 3.0.3，45 路径），交互文档见 `/docs`。

| 分组 | 接口 |
| --- | --- |
| 区域 | `POST /regions/{region}/ingest`、`GET /regions/{region}/status`、`GET /regions/{region}/operations`、`GET /regions/{region}/journal/verify`、`GET /regions` |
| 区域空间 | `GET/POST /regions/{region}/spatial/boundaries`、`POST .../boundaries/invalidate`、`GET/POST .../spatial/policies`、`GET/POST .../spatial/summaries`、`GET .../spatial/summaries/{sid}`、`GET .../spatial/summaries/{sid}/ledger`、`POST .../spatial/width-backfill`、`GET .../spatial/dedup-log` |
| 集团空间 | `GET /spatial/summaries`、`GET/POST /spatial/deliveries`、`GET /spatial/deliveries/{id}/verify` |
| 复制 | `POST /replication/run`、`POST /replication/batch`、`GET /replication/cursors`、`GET /replication/runs`、`GET/POST /replication/links[/{region}]`、`GET /replication/policies`、`GET /replication/replicas` |
| 出境申请 | `GET/POST /outbound/requests`、`GET /outbound/requests/{id}`、`POST .../decide`、`POST .../revoke` |
| 聚合 | `GET/POST /aggregates`、`GET /aggregates/{id}` |
| 交付 | `POST /aggregates/{id}/deliver`、`GET /deliveries/{id}`、`GET /deliveries/{id}/verify` |
| 容灾 | `GET /dr`、`GET /dr/{region}`、`POST /dr/{region}/failover`、`POST /dr/{region}/failback` |
| 审计 | `GET /audit`、`GET /audit/verify`、`GET /audit/{subject}` |
| 演示 | `POST /demo/reset`（`DEMO_MODE=false` 时禁用）、`GET /demo/summary` |

复制故障注入（仅用于验收演练）：`{"fault":"link_down"|"copy_fail"|"confirm_fail"}`。

---

## 7. 手工走一遍（curl）

```bash
A='Authorization: Bearer demo-admin-token'
curl -s -XPOST localhost:8080/demo/reset -H "$A"
curl -s -XPOST localhost:8080/regions/BR-MT/ingest \
  -H 'Authorization: Bearer demo-operator-br-mt' -H 'Content-Type: application/json' \
  --data-binary @samples/regions/br-mt.json
curl -s -XPOST localhost:8080/replication/batch -H "$A" -H 'Content-Type: application/json' -d '{}'
curl -s localhost:8080/replication/replicas -H "$A"        # 只有 US-IA 有 raw_trajectories
curl -s localhost:8080/replication/policies -H "$A"        # 每个区域每类别的出境政策
```

出境链路（申请 → 审批 → 聚合 → 交付 → 验证）：

```bash
AN='Authorization: Bearer demo-analyst-token'; AP='Authorization: Bearer demo-approver-token'
APR=$(curl -s -XPOST localhost:8080/outbound/requests -H "$AN" -H 'Content-Type: application/json' \
  --data-binary @samples/approvals/wide.json | jq -r .id)
curl -s -XPOST localhost:8080/outbound/requests/$APR/decide -H "$AP" -H 'Content-Type: application/json' \
  -d '{"decision":"approved"}' | jq -c '{status,version}'
RUN=$(curl -s -XPOST localhost:8080/aggregates -H "$AN" -H 'Content-Type: application/json' \
  -d "{\"approval_id\":\"$APR\",\"period_from\":\"2026-11-01T00:00:00Z\",\"period_to\":\"2026-11-30T00:00:00Z\",\"metric_keys\":[\"operation_count\",\"completion_rate\",\"area_ha\"]}" | jq -r .id)
DLV=$(curl -s -XPOST localhost:8080/aggregates/$RUN/deliver -H "$AN" | tee /tmp/d.json | jq -r .id)
jq -c '{status,included:.artifact.included_regions,excluded:.artifact.excluded_regions,sha:.artifact_sha256}' /tmp/d.json
curl -s localhost:8080/deliveries/$DLV/verify -H 'Authorization: Bearer demo-auditor-token' | jq -c .
curl -s localhost:8080/audit/verify -H 'Authorization: Bearer demo-auditor-token' | jq -c .
```

空间链路（区域内聚合 → 政策过滤复制 → 集团交付，全程无原始坐标出境）：

```bash
OPR='Authorization: Bearer demo-operator-br-mt'
# 1) 区域内：边界目录（版本化+哈希）与空间政策（方法×粒度）
curl -s -XPOST localhost:8080/regions/BR-MT/spatial/boundaries -H "$OPR" \
  -H 'Content-Type: application/json' --data-binary @samples/spatial/br-mt-boundaries-v1.json
curl -s -XPOST localhost:8080/regions/BR-MT/spatial/policies -H "$OPR" \
  -H 'Content-Type: application/json' --data-binary @samples/spatial/policy-intersection-county.json
# 2) 区域内：上传跨县/低精度/缺宽度片段（重复片段自动去重）
curl -s -XPOST localhost:8080/regions/BR-MT/ingest -H "$OPR" \
  -H 'Content-Type: application/json' --data-binary @samples/spatial/br-mt-batch-a.json
# 3) 区域内：计算可出境摘要（bins + pending + excluded_fragments，原始点不出区域库）
SPS=$(curl -s -XPOST localhost:8080/regions/BR-MT/spatial/summaries -H "$OPR" \
  -H 'Content-Type: application/json' \
  -d '{"period_from":"2026-11-06T00:00:00Z","period_to":"2026-11-30T00:00:00Z"}' | jq -r .summary_id)
curl -s localhost:8080/regions/BR-MT/spatial/summaries/$SPS/ledger -H "$OPR" | jq -c .
# 4) 复制只放行 spatial_summary；raw_trajectory 仍被驻留政策阻断
curl -s -XPOST localhost:8080/replication/run -H "$A" -H 'Content-Type: application/json' -d '{"region":"BR-MT"}'
# 5) 集团侧只能读到消毒后的批准粒度产物
curl -s 'localhost:8080/spatial/summaries?region=BR-MT' -H 'Authorization: Bearer demo-analyst-token' | jq -c .
```

---

## 8. 配置项

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `HTTP_ADDR` | `:8080` | 监听地址 |
| `PGHOST` / `PGPORT` / `PGUSER` / `PGPASSWORD` / `PGSSLMODE` | `127.0.0.1` / `5432` / `ccdev` / `ccdev` / `disable` | PostgreSQL 连接 |
| `PG_ADMIN_URL` | 空 | 建库用连接串（默认用 `PG_ADMIN_DB=postgres` 推导） |
| `CONTROL_DB` | `agrisync_control` | 控制面库名 |
| `REGION_DB_PREFIX` | `agrisync_region_` | 区域库前缀（后接小写化区域码） |
| `REGIONS` | `DE-BB,BR-MT,US-IA,FR-NOR` | 启用的区域 |
| `REPLICATION_BATCH` | `500` | 单次复制最大事件数 |
| `REPLICATION_INTERVAL` | 空（关闭） | 后台复制间隔，如 `5s`；关闭时复制完全由 API 驱动 |
| `DELIVERY_CHANNEL` | `secure_store` | 交付通道标签（写入交付记录与审计） |
| `DEMO_MODE` | `true` | `false` 时禁用 `/demo/reset` |
| `TOKEN_*` | 见第 3 节 | 各角色令牌，生产环境必须覆盖 |

---

## 9. 目录结构

```
cmd/server/            服务入口（含 -healthcheck 子命令，供 distroless 健康检查）
internal/api/          HTTP 路由、鉴权、OpenAPI 文档、后台复制器、空间 API
internal/geo/          纯 Go 空间几何引擎（投影、精确相交切段、不确定判定、Amanatides-Woo 栅格化）
internal/service/      区域写入/去重、复制、聚合、空间目录/切段/出境聚合、审批与交付、容灾、审计链
internal/db/           PostgreSQL 连接池（每库一个）、schema、种子、重置
internal/config/       区域/令牌/连接串配置
internal/model/        通用模型与空间模型（区域内视图与集团消毒产物分离）
web/swagger-ui/        内置 Swagger UI 资产（离线可用，Apache-2.0，见 LICENSE/NOTICE）
samples/               多区域、断网补传、重试重叠、切换后续传、不同授权范围、空间五场景
scripts/               gen_samples.py、gen_spatial_samples.py、patch_openapi.py、replay.sh（断言）、demo.sh、verify.sh
docs/architecture.md   数据流与失败模式说明
```

## 10. 已知边界

- 交付通道 `secure_store` 是本地归档语义（文件存于 `deliveries.artifact`），不包含真实外部传输；接入真实接收方时在
  `Deliver` 的 `sent` 分支替换即可，审计与复核逻辑不变。
- 驻留政策目前通过种子数据维护（只读 API 暴露），政策变更是治理动作，不在演示界面内修改。
- DR 是登记册 + 本地主备角色指针的模拟：单进程内切换，不包含真实流复制；但"只从已确认位置续传"的约束是真实执行的。
- 后台复制默认关闭以确保回放确定性；开启后与回放脚本并行会有竞态（脚本会自行重新复制并断言，通常仍能通过，但不建议）。

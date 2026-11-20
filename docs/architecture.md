# 架构与失败模式

## 1. 数据流

```
              ┌─────────────────────────── 区域 (每个区域独立 PostgreSQL 数据库) ───────────────────────────┐
 农机/网关 ──▶ │ POST /regions/{r}/ingest                                                                   │
              │   ┌─ 单事务 ─────────────────────────────────────────────────────────────────────────────┐ │
              │   │ operation_events (唯一: device_serial, segment_id)  ← 本地权威记录                    │ │
              │   │ journal_entries   (seq 连续, 无空洞)                     ← 本地记录连续性证据         │ │
              │   │ outbox_events     (seq 连续; 每操作拆 summary/trajectory/alarm 事件)                    │ │
              │   │ ingest_receipts   (batch_id + 内容 SHA-256)                                           │ │
              │   └───────────────────────────────────────────────────────────────────────────────────────┘ │
              │ 聚合时在区域内计算 ─▶ metric_snapshots + outbox(aggregate_metric)                          │
              └───────────────────────────────────────┬────────────────────────────────────────────────────┘
                                                      │ 复制编排器 (政策过滤 + 逐事件)
                                                      ▼
              ┌────────────────────────── 控制面库 agrisync_control ──────────────────────────────────────┐
              │ residency_policies  ← 逐类别出境判定                                                       │
              │ replica_op_summaries / replica_trajectories / replica_alarms / replica_metrics            │
              │   status = unconfirmed → confirmed                                                       │
              │ replication_cursors (last_seq, confirmed_seq)   replication_runs (含 fault/错误)          │
              │ dr_registry (primary/standby/active, epoch, last_confirmed_seq)                          │
              │ outbound_requests (审批, version)   aggregate_runs / aggregate_run_regions (coverage)     │
              │ deliveries (artifact 原始字节 + sha256)   audit_events (哈希链)                           │
              └──────────────────────────────────────────────────────────────────────────────────────────┘
```

关键不变式：

| 不变式 | 实现 |
| --- | --- |
| 受限原始轨迹不离境 | 区域库独存原始点；复制逐事件按 `residency_policies` 过滤；交付前再扫描实际字节 |
| 集团可见 = 已确认 | 副本 `unconfirmed` 不参与聚合；聚合只读 `status='confirmed'` 的 `replica_metrics` |
| 去重不重复计面积 | 主键 `(source_region, entity_id)` + 唯一索引 `(source_region, device_serial, segment_id)`，全部 upsert |
| 本地记录连续 | `journal_entries` 序列在写事务内按行锁分配，`/journal/verify` 用 `generate_series` 找空洞 |
| 切换只从已确认位置续传 | failover 删除 `unconfirmed`、把游标回绕到 `dr_registry.last_confirmed_seq` |
| 撤回即阻断 | deliver 事务外重新读取审批与政策；`artifact` 仅在全部检查通过时生成 |
| 报表可自证 | artifact 按字节存 `text` + SHA-256；`/deliveries/{id}/verify` 复算并校验审计条目；`/audit/verify` 校验整链 |
| 空间产物无原始轨迹 | 切段/栅格只在区域库执行；outbox 仅放行 `spatial_summary` 消毒产物；集团库无边界环、无逐点精度、无顺序点；交付前再扫字节 |
| 模糊片段不猜边 | 不确定圆盘跨县 → `uncertainty_crosses_boundary` 待分配并记录候选县与县级上界；宽度缺失/边界无效/目录外同样待分配；重复片段只去重不加倍 |
| 修订可追溯 | 边界版本与政策不可变更新（`superseded` 保留）；新摘要 `supersedes` 旧摘要，历史报表保留原边界哈希与粒度 |

## 2. 失败模式与行为

### 2.1 区域断网（链路故障）

- 写入路径完全不触碰控制面 → 现场仍可连续记账，`journal_head == journal_entries`。
- outbox 事件堆积（`pending_outbox` 可见），但本地记录、去重、回执全部正常。
- 集团聚合：该区域 `replica_metrics` 无本窗口确认快照 → `excluded_regions` + `stale_regions`，合计不含该区；
  `data_cutoff` 取所有纳入区域的最早确认截止点。
- 链路恢复后 `POST /replication/run` 从 `last_seq` 继续，一次扫完积压；重叠重传按片段去重。

### 2.2 复制重试 / 至少一次投递

- `copy_fail`：apply 事务回滚，游标不动，`retry_count++`，`replication_runs` 记录失败原因。
- `confirm_fail`：数据已写但 ack 丢失 → 行存在且保持 `unconfirmed`（对集团不可见），游标/DR 不前进。
- 崩溃后重投：相同 `(region, entity_id)` upsert；聚合只读确认快照，不会二次累加面积。

### 2.3 主备切换

- 只能从 `last_confirmed_seq` 续传；`unconfirmed` 行被删除（旧主未确认缓存绝不视为已发布）。
- 本地日志与本地记账不截断，新主继续接受写入，`epoch` 递增并写入 `dr_events` 与审计。
- 续传后再次确认，`replica_*` 回到与区域库一致的行数/面积。

### 2.4 审批在聚合运行中被撤回

- 聚合运行时记录 `approval_version`；交付时重新读取审批：状态、版本、有效期、区域范围、指标范围。
- 任一不一致 → `blocked`；`artifact=null`，不写任何出境内容；审计为 `delivery.blocked`（`deny`）。
- 指标超出审批范围在聚合阶段即 `403`；区域超出审批范围在聚合阶段被范围裁剪，交付阶段再校验一次。

### 2.5 审计链

- 链式哈希：`hash = sha256(canonical{seq, occurred_at(μs), actor, role, action, subject, region, decision, detail, prev_hash})`。
- `detail` 入库前 JSON 归一化（等价于 jsonb 往返后的形态），避免写读差异。
- 任何行被改写：`prev_hash` 与上一条不符或内容哈希不符 → `/audit/verify` 返回 `valid=false`、`broken_at=<seq>`。

### 2.6 区域内空间聚合与出境粒度证明

```
区域库（原始轨迹 + 边界环 + 精度，均不出境）
  boundary_versions/boundary_counties（版本化，boundary_sha256）
  spatial_policies（boundary_version × method(intersection|raster) × grain(county|grid)）
  operation_spatial_meta（working_width_m、accuracy_m、point_accuracy_m）
        │
        ▼  ComputeSpatialSummary（纯 Go 引擎 internal/geo）
  ┌─ intersection：折线×多边形精确相交，段长×宽度按县分摊
  └─ raster：Amanatides-Woo 网格遍历，批准分辨率累计，顶点模糊性向格子传播
        │
        ├─ 明确片段 → spatial_summaries.bins（县码/不透明网格号 + 面积/时长/覆盖长度/上界）
        ├─ 模糊/缺宽度/无效边界/目录外 → pending + excluded_fragments（候选县，无坐标）
        └─ spatial_segment_ledger（逐片段长度/面积/候选县/内容哈希）+ segment_dedup_log（去重证据）
        │ outbox(spatial_summary) —— 驻留政策过滤（raw_trajectory 仍阻断）
        ▼
控制面库 replica_spatial_summaries（artifact 原始字节 + sha256，confirmed 后集团可见）
        │ POST /spatial/deliveries（审批/驻留/粒度/受限字段/哈希逐项复核）
        ▼
spatial_deliveries + 审计链；GET /spatial/deliveries/{id}/verify 复算证明
```

待分配规则（面积先挂起，绝不任选一侧）：

| 触发条件 | pending reason | 解除方式 |
| --- | --- | --- |
| 定位不确定圆盘跨越多个县/触到边界 | `uncertainty_crosses_boundary` | 更高精度轨迹补传后重算；期间以 `area_ha_max` 给保守上界 |
| 作业宽度缺失（`working_width_m=0/null`） | `working_width_missing` | `POST .../spatial/width-backfill` 后重算 |
| 政策绑定的边界版本被宣告无效 | `boundary_version_invalid` | 发布新边界与新政策后重算 |
| 轨迹落在边界目录覆盖范围外 | `outside_boundary_catalog` | 扩充边界目录后重算 |
| 同一片段重复接收 | 不入汇总，写 `segment_dedup_log` | 去重计数随摘要输出 `duplicate_count` |

边界更正或轨迹补传都生成**新摘要行**并 `supersedes` 旧行；县级与网格粒度各有独立谱系，历史集团报表始终能说明其采用的
`boundary_version_id`、`boundary_sha256`、`method` 与 `output_grain`。

## 3. 权限模型

| 角色 | 可做 | 不可做 |
| --- | --- | --- |
| `region_operator`（绑定单一区域） | 本区域上传、本区域状态/作业/日志校验、本区域复制 | 跨区读写、全局复制、切换、聚合、审批 |
| `group_analyst` | 提交出境申请、聚合、交付 | 审批/撤回、切换、重置、读取区域原始记录 |
| `approver` | 审批、驳回、撤回 | 提交申请、交付、聚合 |
| `auditor` | 读审计、校验 | 任何写操作 |
| `admin` | 链路开关、全局复制、主备切换、演示重置 | 仍受交付前置检查约束（不能绕过审计） |

自批被禁止：`decide` 需要 `approver` 角色，`group_analyst` 调用返回 `403`。

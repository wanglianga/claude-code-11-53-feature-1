#!/usr/bin/env python3
"""Extend openapi.json with the in-region spatial aggregation surface."""
import json
import os

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
PATH = os.path.join(ROOT, "internal", "api", "openapi.json")

with open(PATH, encoding="utf-8") as fh:
    spec = json.load(fh)

# --- tag -----------------------------------------------------------------
if not any(t["name"] == "Spatial" for t in spec["tags"]):
    spec["tags"].append({
        "name": "Spatial",
        "description": "区域内空间聚合：边界目录版本、空间政策、轨迹切段/栅格覆盖、可出境县级/网格摘要与交付证明",
    })

# --- OperationInput gains spatial metadata -------------------------------
op_input = spec["components"]["schemas"]["OperationInput"]["properties"]
op_input["working_width_m"] = {"type": "number", "minimum": 0,
                               "description": "作业宽度（米）。缺失或 0 时该片段面积进入待分配（working_width_missing），补传后再汇总"}
op_input["accuracy_m"] = {"type": "number", "minimum": 0,
                          "description": "默认定位不确定半径（米）；不确定范围跨越多个县时相关片段列为待分配"}
op_input["point_accuracy_m"] = {"type": "array", "items": {"type": "number", "minimum": 0},
                                "description": "逐采样点定位不确定半径（米），长度须与 points 一致；存在时覆盖 accuracy_m"}

bearer = [{"bearerAuth": []}]
region_param = {"$ref": "#/components/parameters/Region"}
json_ct = {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}

def op(summary, tag, req_body=None, params=None, ok_schema=None, ok_desc="", created=False):
    o = {
        "tags": [tag],
        "summary": summary,
        "security": bearer,
        "responses": {
            str(201 if created else 200): {
                "description": ok_desc or summary,
                "content": {"application/json": {"schema": ok_schema}} if ok_schema else {},
            },
            "400": {"description": "请求无效", "content": json_ct},
            "401": {"description": "未认证", "content": json_ct},
            "403": {"description": "无权限（集团分析无区域原始数据通道）", "content": json_ct},
            "404": {"description": "资源不存在", "content": json_ct},
        },
    }
    if params:
        o["parameters"] = params
    if req_body:
        o["requestBody"] = {"required": True, "content": {"application/json": {"schema": req_body}}}
    return o

S = "#/components/schemas/"
def ref(name):
    return {"$ref": S + name}

paths = spec["paths"]

# --- regional routes -----------------------------------------------------
paths["/regions/{region}/spatial/boundaries"] = {
    "get": op("列出区域边界目录版本", "Spatial", params=[region_param],
              ok_schema={"type": "object", "properties": {"region": {"type": "string"},
                         "boundary_versions": {"type": "array", "items": ref("BoundaryVersion")}}},
              ok_desc="边界版本列表（含状态与几何哈希，不含环坐标）"),
    "post": op("上传新边界版本（旧版本保留并标记 superseded）", "Spatial",
               req_body=ref("BoundaryUpload"), created=True, ok_schema=ref("BoundaryVersion"),
               ok_desc="新建的边界版本"),
}
paths["/regions/{region}/spatial/boundaries/invalidate"] = {
    "post": op("宣告边界版本无效（依据该版本的新摘要全部待分配）", "Spatial",
               req_body={"type": "object", "required": ["version_id"],
                         "properties": {"version_id": {"type": "string"}}},
               ok_schema=ref("BoundaryVersion"), ok_desc="失效后的边界版本"),
}
paths["/regions/{region}/spatial/policies"] = {
    "get": op("列出空间政策（边界版本×方法×输出粒度的唯一绑定）", "Spatial", params=[region_param],
              ok_schema={"type": "object", "properties": {"region": {"type": "string"},
                         "policies": {"type": "array", "items": ref("SpatialPolicy")}}}),
    "post": op("创建空间政策（同区域仅一个 active）", "Spatial",
               req_body=ref("SpatialPolicyInput"), created=True, ok_schema=ref("SpatialPolicy")),
}
paths["/regions/{region}/spatial/summaries"] = {
    "get": op("列出本区域空间摘要（含被排除模糊片段，仅区域内可见）", "Spatial", params=[region_param],
              ok_schema={"type": "object", "properties": {"region": {"type": "string"},
                         "summaries": {"type": "array", "items": ref("SpatialSummary")}}}),
    "post": op("在区域内计算空间摘要（精确相交或栅格化，结果进 outbox 待政策过滤复制）", "Spatial",
               req_body=ref("SpatialSummaryInput"), created=True, ok_schema=ref("SpatialSummary")),
}
paths["/regions/{region}/spatial/summaries/{sid}"] = {
    "get": op("读取一个空间摘要的区域内完整视图", "Spatial",
              params=[region_param, {"name": "sid", "in": "path", "required": True,
                                     "schema": {"type": "string"}}],
              ok_schema=ref("SpatialSummary")),
}
paths["/regions/{region}/spatial/summaries/{sid}/ledger"] = {
    "get": op("读取摘要的逐片段切段台账（县分摊长度/面积、模糊长度、候选县）", "Spatial",
              params=[region_param, {"name": "sid", "in": "path", "required": True,
                                     "schema": {"type": "string"}}],
              ok_schema={"type": "object", "properties": {
                  "region": {"type": "string"}, "summary_id": {"type": "string"},
                  "ledger": {"type": "array", "items": ref("SpatialSegmentLedgerRow")}}}),
}
paths["/regions/{region}/spatial/width-backfill"] = {
    "post": op("补传作业宽度（待分配片段在下次摘要时重新计入）", "Spatial",
               req_body={"type": "object", "required": ["items"],
                         "properties": {"items": {"type": "array",
                                                  "items": ref("WidthBackfill")}}},
               ok_schema={"type": "object", "properties": {"region": {"type": "string"},
                          "updated_segments": {"type": "integer"}}}),
}
paths["/regions/{region}/spatial/dedup-log"] = {
    "get": op("重复接收日志（去重证据，证明重复片段未被加倍）", "Spatial", params=[region_param],
              ok_schema={"type": "object", "properties": {"region": {"type": "string"},
                         "dedup_log": {"type": "array", "items": ref("DedupLogRow")}}}),
}

# --- group routes --------------------------------------------------------
paths["/spatial/summaries"] = {
    "get": op("集团侧：只返回已确认的消毒后空间摘要（无坐标、无顺序点、无未批准细网格）", "Spatial",
              params=[{"name": "region", "in": "path", "required": False,
                       "schema": {"type": "string"}}],
              ok_schema={"type": "object", "properties": {
                  "spatial_summaries": {"type": "array", "items": ref("SpatialArtifact")}}}),
}
paths["/spatial/deliveries"] = {
    "get": op("集团侧：空间交付历史", "Spatial",
              ok_schema={"type": "object", "properties": {
                  "deliveries": {"type": "array", "items": ref("SpatialDelivery")}}}),
    "post": op("交付前复核并发送空间摘要（审批/驻留/粒度/受限字段/哈希逐项检查）", "Spatial",
               req_body=ref("SpatialDeliveryInput"), ok_schema=ref("SpatialDelivery"),
               ok_desc="sent=200 语义（阻断返回 409 且 artifact 为 null）"),
}
paths["/spatial/deliveries/{id}/verify"] = {
    "get": op("复算交付字节哈希、重扫受限字段并校验审计链", "Spatial",
              params=[{"name": "id", "in": "path", "required": True,
                       "schema": {"type": "string"}}],
              ok_schema=ref("SpatialArtifactVerification")),
}

# Fix: /spatial/summaries region param is a query param, not path.
paths["/spatial/summaries"]["get"]["parameters"] = [{
    "name": "region", "in": "query", "required": False, "schema": {"type": "string"},
    "description": "可选区域过滤，例如 BR-MT",
}]

# --- schemas -------------------------------------------------------------
sch = spec["components"]["schemas"]
sch.update({
    "CountyRingInput": {
        "type": "object", "required": ["county_code", "ring"],
        "properties": {
            "county_code": {"type": "string", "example": "BR-MT-5103"},
            "county_name": {"type": "string"},
            "ring": {"type": "array", "minItems": 3,
                     "description": "[[lat, lon], ...] 闭合多边形（首尾无需重复）；原始坐标只存区域库",
                     "items": {"type": "array", "minItems": 2, "maxItems": 2,
                               "items": {"type": "number"}}},
        },
    },
    "BoundaryUpload": {
        "type": "object", "required": ["counties"],
        "properties": {"note": {"type": "string"},
                       "counties": {"type": "array", "items": ref("CountyRingInput")}},
    },
    "BoundaryVersion": {
        "type": "object",
        "properties": {
            "version_id": {"type": "string"}, "region_code": {"type": "string"},
            "status": {"type": "string", "enum": ["active", "superseded", "invalid"]},
            "boundary_sha256": {"type": "string", "description": "规范化环坐标的 SHA-256，摘要据此声明所用几何"},
            "effective_from": {"type": "string", "format": "date-time"},
            "superseded_by": {"type": "string"}, "note": {"type": "string"},
            "county_count": {"type": "integer"}, "created_at": {"type": "string", "format": "date-time"},
        },
    },
    "SpatialPolicyInput": {
        "type": "object",
        "required": ["boundary_version_id", "output_grain", "method"],
        "properties": {
            "boundary_version_id": {"type": "string"},
            "output_grain": {"type": "string", "enum": ["county", "grid"],
                             "description": "获准出境粒度；county=仅县级，grid=批准分辨率网格"},
            "method": {"type": "string", "enum": ["intersection", "raster"],
                       "description": "intersection=线段与边界精确相交按长度×宽度分摊；raster=批准分辨率栅格累计"},
            "grid_size_m": {"type": "integer", "minimum": 100,
                            "description": "raster 方法的批准网格边长（米），不得小于 100"},
            "default_accuracy_m": {"type": "number", "minimum": 0},
        },
    },
    "SpatialPolicy": {
        "type": "object",
        "properties": {
            "policy_id": {"type": "string"}, "region_code": {"type": "string"},
            "boundary_version_id": {"type": "string"},
            "output_grain": {"type": "string", "enum": ["county", "grid"]},
            "method": {"type": "string", "enum": ["intersection", "raster"]},
            "grid_size_m": {"type": "integer"}, "default_accuracy_m": {"type": "number"},
            "status": {"type": "string", "enum": ["active", "superseded"]},
            "created_at": {"type": "string", "format": "date-time"},
        },
    },
    "SpatialSummaryInput": {
        "type": "object",
        "required": ["period_from", "period_to"],
        "properties": {
            "policy_id": {"type": "string", "description": "空则使用区域当前 active 政策"},
            "period_from": {"type": "string", "format": "date-time"},
            "period_to": {"type": "string", "format": "date-time"},
            "operation_type": {"type": "string", "enum": ["seeding", "harvesting", "fertilizing", ""],
                                "description": "空表示全部作业类型"},
        },
    },
    "SpatialBin": {
        "type": "object",
        "properties": {
            "unit_type": {"type": "string", "enum": ["county", "grid"]},
            "unit_code": {"type": "string", "description": "县级为县码；网格为不透明单元号（无坐标锚点出境）"},
            "county_code": {"type": "string"}, "operation_type": {"type": "string"},
            "segment_count": {"type": "integer"}, "area_ha": {"type": "number"},
            "area_ha_max": {"type": "number", "description": "含候选模糊面积的保守上界"},
            "uncertainty_ha": {"type": "number"}, "duration_hours": {"type": "number"},
            "covered_length_m": {"type": "number"}, "grid_cells_touched": {"type": "integer"},
        },
    },
    "PendingByReason": {
        "type": "object",
        "properties": {
            "reason": {"type": "string", "enum": [
                "uncertainty_crosses_boundary", "working_width_missing", "boundary_version_invalid",
                "outside_boundary_catalog", "no_trajectory", "degenerate_trajectory"],
                "description": "待分配原因（实际输出带 |作业类型 后缀）"},
            "area_ha": {"type": "number"}, "duration_hours": {"type": "number"},
            "segment_count": {"type": "integer"},
        },
    },
    "SpatialArtifact": {
        "type": "object",
        "description": "唯一允许出境的空间载荷：批准粒度 bin + 待分配证明 + 边界/政策版本；不含任何坐标或顺序点",
        "properties": {
            "schema": {"type": "string"}, "summary_id": {"type": "string"},
            "source_region": {"type": "string"}, "policy_id": {"type": "string"},
            "boundary_version_id": {"type": "string"}, "boundary_sha256": {"type": "string"},
            "method": {"type": "string"}, "output_grain": {"type": "string"},
            "grid_size_m": {"type": "integer"},
            "period_from": {"type": "string", "format": "date-time"},
            "period_to": {"type": "string", "format": "date-time"},
            "operation_type": {"type": "string"}, "generated_at": {"type": "string", "format": "date-time"},
            "bins": {"type": "array", "items": ref("SpatialBin")},
            "pending": {"type": "array", "items": ref("PendingByReason")},
            "pending_total_ha": {"type": "number"}, "allocated_total_ha": {"type": "number"},
            "declared_total_ha": {"type": "number"}, "source_segment_count": {"type": "integer"},
            "duplicate_count": {"type": "integer"}, "supersedes_summary_id": {"type": "string"},
            "contains_raw_trajectories": {"type": "boolean", "enum": [False]},
            "notes": {"type": "array", "items": {"type": "string"}},
        },
    },
    "ExcludedFragment": {
        "type": "object",
        "description": "区域内可见的被排除/待分配片段证据（无坐标）",
        "properties": {
            "operation_id": {"type": "string"}, "segment_id": {"type": "string"},
            "device_serial": {"type": "string"},
            "status": {"type": "string", "enum": ["allocated", "pending", "excluded"]},
            "reason": {"type": "string"}, "ambiguous_length_m": {"type": "number"},
            "pending_area_ha": {"type": "number"},
            "candidate_counties": {"type": "array", "items": {"type": "string"}},
            "working_width_m": {"type": "number"},
        },
    },
    "SpatialSummary": {
        "type": "object",
        "properties": {
            "summary_id": {"type": "string"}, "region_code": {"type": "string"},
            "policy_id": {"type": "string"}, "boundary_version_id": {"type": "string"},
            "boundary_sha256": {"type": "string"}, "method": {"type": "string"},
            "output_grain": {"type": "string"}, "grid_size_m": {"type": "integer"},
            "period_from": {"type": "string", "format": "date-time"},
            "period_to": {"type": "string", "format": "date-time"},
            "operation_type": {"type": "string"},
            "status": {"type": "string", "enum": ["computed", "superseded", "replicated"]},
            "supersedes_summary_id": {"type": "string"}, "data_cutoff": {"type": "string", "format": "date-time"},
            "bins": {"type": "array", "items": ref("SpatialBin")},
            "pending": {"type": "array", "items": ref("PendingByReason")},
            "excluded_fragments": {"type": "array", "items": ref("ExcludedFragment")},
            "source_segment_count": {"type": "integer"}, "duplicate_count": {"type": "integer"},
            "artifact_sha256": {"type": "string"}, "outbox_seq": {"type": "integer"},
            "source_node": {"type": "string"}, "created_at": {"type": "string", "format": "date-time"},
        },
    },
    "SpatialSegmentLedgerRow": {
        "type": "object",
        "properties": {
            "operation_id": {"type": "string"}, "segment_id": {"type": "string"},
            "device_serial": {"type": "string"},
            "status": {"type": "string", "enum": ["allocated", "pending", "excluded"]},
            "pending_reason": {"type": "string"}, "boundary_version_id": {"type": "string"},
            "policy_id": {"type": "string"}, "method": {"type": "string"},
            "working_width_m": {"type": "number"}, "declared_area_ha": {"type": "number"},
            "total_length_m": {"type": "number"}, "allocated_length_m": {"type": "number"},
            "ambiguous_length_m": {"type": "number"}, "allocated_area_ha": {"type": "number"},
            "pending_area_ha": {"type": "number"}, "duration_s": {"type": "integer"},
            "candidate_counties": {"type": "array", "items": {"type": "string"}},
            "content_sha256": {"type": "string"},
        },
    },
    "DedupLogRow": {
        "type": "object",
        "properties": {
            "device_serial": {"type": "string"}, "segment_id": {"type": "string"},
            "batch_id": {"type": "string"}, "content_sha256": {"type": "string"},
            "received_at": {"type": "string", "format": "date-time"},
        },
    },
    "WidthBackfill": {
        "type": "object", "required": ["device_serial", "segment_id", "working_width_m"],
        "properties": {
            "device_serial": {"type": "string"}, "segment_id": {"type": "string"},
            "working_width_m": {"type": "number", "exclusiveMinimum": 0},
        },
    },
    "SpatialDeliveryInput": {
        "type": "object",
        "required": ["source_region", "summary_id", "approval_id"],
        "properties": {
            "source_region": {"type": "string"}, "summary_id": {"type": "string"},
            "approval_id": {"type": "string",
                            "description": "审批须包含 spatial_area_ha 指标且覆盖该区域"},
        },
    },
    "SpatialDelivery": {
        "type": "object",
        "properties": {
            "id": {"type": "string"}, "source_region": {"type": "string"},
            "summary_id": {"type": "string"}, "approval_id": {"type": "string"},
            "status": {"type": "string", "enum": ["sent", "blocked"]},
            "channel": {"type": "string"},
            "checks": {"type": "array", "items": ref("Check")},
            "blocked_reason": {"type": "string"},
            "artifact": {"description": "阻断时为 null", "anyOf": [ref("SpatialArtifact"), {"type": "null"}]},
            "artifact_sha256": {"type": "string"}, "audit_seq": {"type": "integer"},
            "requested_by": {"type": "string"}, "created_at": {"type": "string", "format": "date-time"},
            "sent_at": {"type": "string", "format": "date-time", "nullable": True},
        },
    },
    "SpatialArtifactVerification": {
        "type": "object",
        "properties": {
            "delivery_id": {"type": "string"}, "status": {"type": "string"},
            "recomputed_sha256": {"type": "string"}, "recorded_sha256": {"type": "string"},
            "sha256_matches": {"type": "boolean"},
            "restricted_fields": {"type": "array", "items": {"type": "string"}},
            "audit_seq": {"type": "integer"}, "audit_entry_valid": {"type": "boolean"},
            "audit_chain_valid": {"type": "boolean"}, "audit_chain_head": {"type": "string"},
            "verified_at": {"type": "string", "format": "date-time"}, "artifact_bytes": {"type": "integer"},
        },
    },
})

with open(PATH, "w", encoding="utf-8") as fh:
    json.dump(spec, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
print("paths now:", len(spec["paths"]), "schemas now:", len(spec["components"]["schemas"]))

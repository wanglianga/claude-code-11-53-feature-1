#!/usr/bin/env bash
# Human-oriented walkthrough: prints the group's state at each stage so the
# behaviour is readable, without assertions. Use scripts/replay.sh to verify.
set -euo pipefail
BASE="${1:-http://127.0.0.1:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
S="$ROOT/samples"
A="Authorization: Bearer demo-admin-token"
AN="Authorization: Bearer demo-analyst-token"
AP="Authorization: Bearer demo-approver-token"
AU="Authorization: Bearer demo-auditor-token"
post() { curl -sS -XPOST -H "$2" -H 'Content-Type: application/json' ${3:+--data-binary "$3"} "$BASE$1"; }
get()  { curl -sS -H "$2" "$BASE$1"; }

hr() { printf '\n\033[1m--- %s ---\033[0m\n' "$1"; }

hr "reset"
post /demo/reset "$A" | jq -c .

hr "each region uploads locally (8 work segments each incl. raw trajectories)"
for r in de-bb br-mt us-ia fr-nor; do
  code=$(echo "$r" | tr 'a-z' 'A-Z')
  post "/regions/$code/ingest" "Authorization: Bearer demo-operator-$r" "@$S/regions/$r.json" \
    | jq -c '{region,accepted,duplicates,first_journal_seq,last_journal_seq,active_node}'
done

hr "replication: summaries/alarms leave every region; raw trajectories only where policy allows"
post /replication/batch "$A" '{}' | jq -c '.runs[]|{source_region,status,copied:.copied_by_category,blocked:.blocked_by_category}'
get /replication/replicas "$A" | jq -c .

hr "BR-MT link goes down; the farm keeps working and the journal stays continuous"
post /replication/links/BR-MT "$A" '{"status":"offline","reason":"demo: backbone cut"}' >/dev/null
post /regions/BR-MT/ingest "Authorization: Bearer demo-operator-br-mt" "@$S/offline/br-mt-offline-backfill.json" \
  | jq -c '{accepted,first_journal_seq,last_journal_seq}'
get /regions/BR-MT/status "Authorization: Bearer demo-operator-br-mt" | jq -c '{journal_head,journal_entries,operation_count,pending_outbox,replication_link,handover_ready}'
get /regions/BR-MT/journal/verify "Authorization: Bearer demo-operator-br-mt" | jq -c .

hr "the group report says exactly which region is missing"
APR=$(post /outbound/requests "$AN" "@$S/approvals/wide.json" | jq -r .id)
post "/outbound/requests/$APR/decide" "$AP" '{"decision":"approved","note":"demo"}' >/dev/null
RUN=$(post /aggregates "$AN" "$(jq -n --arg a "$APR" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",metric_keys:["operation_count","completed_count","completion_rate","area_ha","fuel_l","duration_hours"]}')" | jq -r .id)
get "/aggregates/$RUN" "$AN" | jq -c '{included:.coverage.included_regions,excluded:.coverage.excluded_regions,delayed:.coverage.stale_regions,cutoff:.coverage.data_cutoff,totals:.result}'

hr "link restored; overlapping retry upload dedupes and replication confirms the backlog"
post /replication/links/BR-MT "$A" '{"status":"online","reason":""}' >/dev/null
post /regions/BR-MT/ingest "Authorization: Bearer demo-operator-br-mt" "@$S/offline/br-mt-retry-overlap.json" \
  | jq -c '{accepted,duplicates,first_journal_seq,last_journal_seq}'
post /replication/run "$A" '{"region":"BR-MT"}' | jq -c '{status,copied:.copied_by_category,blocked:.blocked_by_category}'
get /replication/cursors "$A" | jq -c '.cursors[]|select(.source_region=="BR-MT")|{scanned_seq,confirmed_seq,retry_count,copied_total,blocked_total}'

hr "approval revoked after the run: delivery is blocked and leaks nothing"
APR2=$(post /outbound/requests "$AN" "@$S/approvals/narrow.json" | jq -r .id)
post "/outbound/requests/$APR2/decide" "$AP" '{"decision":"approved"}' >/dev/null
RUN2=$(post /aggregates "$AN" "$(jq -n --arg a "$APR2" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",metric_keys:["operation_count","area_ha"]}')" | jq -r .id)
post "/outbound/requests/$APR2/revoke" "$AP" '{"reason":"withdrawn during delivery window"}' >/dev/null
post "/aggregates/$RUN2/deliver" "$AN" | jq -c '{status,blocked_reason,artifact,checks:[.checks[]|select(.passed==false)|.name]}'

hr "authorized delivery, then verify the audit chain around it"
APR3=$(post /outbound/requests "$AN" "@$S/approvals/wide.json" | jq -r .id)
post "/outbound/requests/$APR3/decide" "$AP" '{"decision":"approved"}' >/dev/null
RUN3=$(post /aggregates "$AN" "$(jq -n --arg a "$APR3" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",metric_keys:["operation_count","completion_rate","area_ha"]}')" | jq -r .id)
DLV=$(post "/aggregates/$RUN3/deliver" "$AN" | tee /tmp/dlv.json | jq -r .id)
jq -c '{status,sha:.artifact_sha256,included:.artifact.included_regions,excluded:.artifact.excluded_regions,data_cutoff:.artifact.data_cutoff,notes:.artifact.notes}' /tmp/dlv.json
get "/deliveries/$DLV/verify" "$AU" | jq -c .
get /audit/verify "$AU" | jq -c .

hr "primary/standby switch resumes at the confirmed position"
get /dr/US-IA "$A" | jq -c '.registry|{active_node,standby_node,epoch,last_confirmed_seq}'
post /dr/US-IA/failover "$A" '{"reconcile":true}' | jq -c '{from_node,to_node,epoch,confirmed_seq,discarded_unconfirmed_rows,resumed_from_seq}'
get /regions/US-IA/status "Authorization: Bearer demo-operator-us-ia" | jq -c '{active_node,epoch,journal_head,journal_entries,handover_ready}'
post /dr/US-IA/failback "$A" '{}' | jq -c '{from_node,to_node,epoch}'

hr "in-region spatial aggregation: cross-county, low accuracy, dedupe, revision"
OPR="Authorization: Bearer demo-operator-br-mt"
post /regions/BR-MT/spatial/boundaries "$OPR" "@$S/spatial/br-mt-boundaries-v1.json" | jq -c '{version_id,boundary_sha256,county_count}'
post /regions/BR-MT/spatial/policies "$OPR" "@$S/spatial/policy-intersection-county.json" | jq -c '{policy_id,method,output_grain}'
post /regions/BR-MT/ingest "$OPR" "@$S/spatial/br-mt-batch-a.json" | jq -c '{accepted,duplicates}'
post /regions/BR-MT/ingest "$OPR" "@$S/spatial/br-mt-batch-b.json" | jq -c '{accepted,duplicates}'
SPS=$(post /regions/BR-MT/spatial/summaries "$OPR" '{"period_from":"2026-11-06T00:00:00Z","period_to":"2026-11-30T00:00:00Z"}' | tee /tmp/sps.json | jq -r .summary_id)
jq -c '{bins:.bins,pending:.pending,duplicates:.duplicate_count}' /tmp/sps.json
get "/regions/BR-MT/spatial/summaries/$SPS/ledger" "$OPR" | jq -c '.ledger[]|{segment_id,status,allocated_area_ha,pending_area_ha,pending_reason}'
post /replication/run "$A" '{"region":"BR-MT"}' | jq -c '{status,copied:.copied_by_category,blocked:.blocked_by_category}'
get "/spatial/summaries?region=BR-MT" "$AN" | jq -c '.spatial_summaries[0]|{summary_id,output_grain,bins:(.bins|length),pending_total_ha,contains_raw_trajectories}'

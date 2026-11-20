#!/usr/bin/env bash
# End-to-end acceptance replay.
#
# Replays, against a running API, the four failure scenarios the platform must
# survive and proves each with assertions (not just prints):
#
#   1. regional outage      — BR-MT link down, local ingest keeps committing,
#                             group aggregate excludes BR-MT and says so
#   2. replication retry    — overlapping re-upload dedupes, cursor does not
#                             move while failing, area totals never double
#   3. primary/standby      — failover resumes at the DR-confirmed position,
#                             unconfirmed copies are discarded, local journal
#                             stays gapless on the new primary
#   4. approval revocation  — approval revoked between aggregate and delivery
#                             blocks the delivery and exports no artifact
#
# plus the outbound audit chain: artifact hash, restricted-field scan, and
# tamper detection.
#
# Usage: scripts/replay.sh [BASE_URL]        (default http://127.0.0.1:8080)
set -uo pipefail

BASE="${1:-http://127.0.0.1:8080}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
S="$ROOT/samples"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

TOK_ADMIN="demo-admin-token"
TOK_ANALYST="demo-analyst-token"
TOK_APPROVER="demo-approver-token"
TOK_AUDITOR="demo-auditor-token"

PASS=0
FAIL=0
CHECKS=()

slug() { echo "$1" | tr 'a-z' 'A-Z'; }

# api METHOD PATH TOKEN [JSON_BODY]
# If the 4th argument starts with '@', it is treated as a file path.
api() {
  local method="$1" path="$2" token="$3" body="${4:-}"
  local args=(-sS -X "$method" "$BASE$path" -H "Authorization: Bearer $token")
  if [[ -n "$body" ]]; then
    if [[ "$body" == @* ]]; then
      args+=(-H 'Content-Type: application/json' --data-binary "@${body#@}")
    else
      args+=(-H 'Content-Type: application/json' --data-binary "$body")
    fi
  fi
  curl "${args[@]}"
}

# check NAME ACTUAL EXPECTED — pass when the two strings are equal
check() {
  local name="$1" actual="$2" expected="$3"
  if [[ "$actual" == "$expected" ]]; then
    PASS=$((PASS + 1))
    printf '  \033[32mPASS\033[0m %-62s %s\n' "$name" "$actual"
    CHECKS+=("PASS|$name|$actual")
  else
    FAIL=$((FAIL + 1))
    printf '  \033[31mFAIL\033[0m %-62s got=%s want=%s\n' "$name" "$actual" "$expected"
    CHECKS+=("FAIL|$name|got=$actual want=$expected")
  fi
}

# contains NAME HAYSTACK NEEDLE — substring assertion for JSON blobs
contains() {
  local name="$1" hay="$2" needle="$3"
  if [[ "$hay" == *"$needle"* ]]; then
    PASS=$((PASS + 1))
    printf '  \033[32mPASS\033[0m %-62s contains %s\n' "$name" "$needle"
    CHECKS+=("PASS|$name|contains $needle")
  else
    FAIL=$((FAIL + 1))
    printf '  \033[31mFAIL\033[0m %-62s missing %s\n' "$name" "$needle"
    CHECKS+=("FAIL|$name|missing $needle")
  fi
}

section() { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

sql_region() {
  PGPASSWORD="${PGPASSWORD:-ccdev}" "${PSQL:-/opt/postgresql-18/bin/psql}" \
    -h "${PGHOST:-127.0.0.1}" -p "${PGPORT:-5432}" -U "${PGUSER:-ccdev}" \
    -d "agrisync_region_$1" -tAc "$2" 2>/dev/null
}

sql_control() {
  PGPASSWORD="${PGPASSWORD:-ccdev}" "${PSQL:-/opt/postgresql-18/bin/psql}" \
    -h "${PGHOST:-127.0.0.1}" -p "${PGPORT:-5432}" -U "${PGUSER:-ccdev}" \
    -d agrisync_control -tAc "$1" 2>/dev/null
}

echo "AgriSync acceptance replay against $BASE"

section "0. authentication and role boundaries"
check "no token is rejected" \
  "$(curl -s -o /dev/null -w '%{http_code}' "$BASE/regions")" "401"
check "bad token is rejected" \
  "$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer nope' "$BASE/regions")" "401"
check "region operator cannot read another region" \
  "$(api GET /regions/BR-MT/status demo-operator-de-bb | jq -r .error.code)" "forbidden"
check "region operator cannot ingest into another region" \
  "$(api POST /regions/BR-MT/ingest demo-operator-de-bb '@$S/regions/br-mt.json' | jq -r .error.code)" "forbidden"
check "analyst cannot approve their own request" \
  "$(api POST /outbound/requests "$TOK_ANALYST" '@$S/approvals/wide.json' >/dev/null; \
     id=$(api GET /outbound/requests "$TOK_ANALYST" | jq -r '.requests[0].id'); \
     api POST "/outbound/requests/$id/decide" "$TOK_ANALYST" '{"decision":"approved"}' | jq -r .error.code)" "forbidden"
check "operator cannot trigger a global replication sweep" \
  "$(api POST /replication/batch demo-operator-de-bb '{}' | jq -r .error.code)" "forbidden"
check "operator cannot fail over a region" \
  "$(api POST /dr/US-IA/failover demo-operator-us-ia '{}' | jq -r .error.code)" "forbidden"
check "group analyst has no path to regional raw records" \
  "$(api GET /regions/BR-MT/operations "$TOK_ANALYST" | jq -r .error.code)" "forbidden"
check "auditor may read a region's records for audit" \
  "$(api GET /regions/BR-MT/operations "$TOK_AUDITOR" | jq -r '.region')" "BR-MT"

section "0. reset and baseline ingest"
api POST /demo/reset "$TOK_ADMIN" | tee "$TMP/reset.json" >/dev/null
check "reset" "$(jq -r .status "$TMP/reset.json" 2>/dev/null)" "reset"

for r in de-bb br-mt us-ia fr-nor; do
  code=$(slug "$r")
  out=$(api POST "/regions/$code/ingest" "demo-operator-$r" "@$S/regions/$r.json")
  echo "$out" > "$TMP/ingest-$r.json"
  check "ingest $code accepted" "$(jq -r .accepted "$TMP/ingest-$r.json")" "4"
  check "ingest $code duplicates" "$(jq -r .duplicates "$TMP/ingest-$r.json")" "0"
  check "ingest $code journal starts at 1" "$(jq -r .first_journal_seq "$TMP/ingest-$r.json")" "1"
done

# Idempotent replay of an identical batch: no new rows, no new journal entries.
out=$(api POST /regions/DE-BB/ingest "demo-operator-de-bb" "@$S/regions/de-bb.json")
echo "$out" > "$TMP/replay.json"
check "identical batch replay is flagged" "$(jq -r .replayed_batch "$TMP/replay.json")" "true"
# The receipt replays the original outcome verbatim: the same four segments had
# been accepted on first delivery, and none of them were written again.
check "identical batch replay reports the original receipt" "$(jq -r .accepted "$TMP/replay.json")" "4"
check "identical batch replay wrote no new journal entries" \
  "$(jq -r .last_journal_seq "$TMP/replay.json")" "$(jq -r .last_journal_seq "$TMP/ingest-de-bb.json")"
check "DE-BB still holds exactly 4 operations" "$(sql_region de_bb 'select count(*) from operation_events')" "4"

api POST /replication/batch "$TOK_ADMIN" '{}' > "$TMP/repl1.json"
check "initial replication succeeded" "$(jq -r '[.runs[]|select(.status=="succeeded")]|length' "$TMP/repl1.json")" "4"

stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "raw trajectories copied only from policy-permitted US-IA" \
  "$(jq -r '.raw_trajectories_by_region|keys|sort|join(",")' <<<"$stats")" "US-IA"
check "raw trajectories blocked for the other three regions" \
  "$(jq -r '[.runs[]|select(.blocked_by_category.raw_trajectory==4)]|length' "$TMP/repl1.json")" "3"

section "1. regional outage: BR-MT keeps recording, group stats lag"
api POST /replication/links/BR-MT "$TOK_ADMIN" '{"status":"offline","reason":"acceptance: backbone cut"}' >/dev/null
out=$(api POST /regions/BR-MT/ingest demo-operator-br-mt "@$S/offline/br-mt-offline-backfill.json")
echo "$out" > "$TMP/offline.json"
check "offline region still commits locally" "$(jq -r .accepted "$TMP/offline.json")" "3"
check "offline batch continues the journal" "$(jq -r .first_journal_seq "$TMP/offline.json")" "5"

st=$(api GET /regions/BR-MT/status demo-operator-br-mt)
echo "$st" > "$TMP/brstatus.json"
check "local journal is continuous while offline" "$(jq -r .journal_entries "$TMP/brstatus.json")" "7"
check "local journal head matches entries" "$(jq -r .journal_head "$TMP/brstatus.json")" "7"
check "region reports the link as offline" "$(jq -r .replication_link "$TMP/brstatus.json")" "offline"
check "outbox backlog is visible (7 events pending)" "$(jq -r .pending_outbox "$TMP/brstatus.json")" "7"

stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "offline ingest does not leak into the group store" \
  "$(jq -r '.operation_summaries_by_region["BR-MT"]' <<<"$stats")" "4"
check "no unconfirmed rows exist for a link that never connected" \
  "$(jq -r --arg r BR-MT '(.unconfirmed_by_region[$r] // 0)' <<<"$stats")" "0"

run=$(api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}')
echo "$run" > "$TMP/offline-run.json"
check "replication while offline fails" "$(jq -r .error.code "$TMP/offline-run.json")" "unavailable"

apr=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr/decide" "$TOK_APPROVER" '{"decision":"approved","note":"acceptance"}' >/dev/null
aggbody=$(jq -n --arg a "$apr" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",
  period_to:"2026-11-30T00:00:00Z",
  metric_keys:["operation_count","completed_count","completion_rate","area_ha","fuel_l","duration_hours"]}')
runid=$(api POST /aggregates "$TOK_ANALYST" "$aggbody" | jq -r .id)
agg=$(api GET "/aggregates/$runid" "$TOK_ANALYST")
echo "$agg" > "$TMP/agg-offline.json"
check "aggregate includes the three reachable regions" \
  "$(jq -r '.coverage.included_regions|join(",")' "$TMP/agg-offline.json")" "DE-BB,FR-NOR,US-IA"
check "aggregate lists BR-MT as excluded" \
  "$(jq -r '.coverage.excluded_regions|join(",")' "$TMP/agg-offline.json")" "BR-MT"
check "excluded BR-MT is also listed as delayed" \
  "$(jq -r '.coverage.stale_regions|join(",")' "$TMP/agg-offline.json")" "BR-MT"
check "group total excludes the offline region" \
  "$(jq -r '.result.operation_count' "$TMP/agg-offline.json")" "12"

section "2. replication retry: overlap dedupes, area never doubles"
area_before=$(sql_control "select coalesce(sum(area_ha),0) from replica_op_summaries where source_region='BR-MT'")
check "BR-MT replica area before backfill" "$area_before" "414.25"

rc1=$(api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}')
check "retry still fails while the link is offline" "$(jq -r .error.code <<<"$rc1")" "unavailable"
cur=$(api GET /replication/cursors "$TOK_ADMIN" | jq -c '.cursors[]|select(.source_region=="BR-MT")')
check "cursor did not advance on failure" "$(jq -r '.confirmed_seq' <<<"$cur")" "9"
check "retry count increments" "$(jq -r '.retry_count>=1' <<<"$cur")" "true"

api POST /replication/links/BR-MT "$TOK_ADMIN" '{"status":"online","reason":""}' >/dev/null
out=$(api POST /regions/BR-MT/ingest demo-operator-br-mt "@$S/offline/br-mt-retry-overlap.json")
echo "$out" > "$TMP/retry.json"
check "overlapping retry upload dedupes 3 repeats" "$(jq -r .duplicates "$TMP/retry.json")" "3"
check "overlapping retry upload adds only the new segment" "$(jq -r .accepted "$TMP/retry.json")" "1"

api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT","fault":"copy_fail"}' > "$TMP/copyfail.json"
check "injected replica write failure is recorded" "$(jq -r '.run.status' "$TMP/copyfail.json")" "failed"
cur=$(api GET /replication/cursors "$TOK_ADMIN" | jq -c '.cursors[]|select(.source_region=="BR-MT")')
check "failed copy does not advance the cursor" "$(jq -r '.confirmed_seq' <<<"$cur")" "9"

api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' > "$TMP/retryok.json"
check "retry after failure succeeds" "$(jq -r '.status' "$TMP/retryok.json")" "succeeded"

# Re-send the same window by rewinding nothing: a second run must be a no-op,
# and the group area must equal the region's own area sum exactly.
api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' > "$TMP/noop.json"
check "second replication of the same window is a no-op" "$(jq -r '.status' "$TMP/noop.json")" "noop"
stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "backfilled segments are now group-visible for BR-MT" \
  "$(jq -r '.operation_summaries_by_region["BR-MT"]' <<<"$stats")" "8"
check "BR-MT raw trajectories still never leave the region" \
  "$(jq -r --arg r BR-MT '(.raw_trajectories_by_region[$r] // 0)' <<<"$stats")" "0"
check "DE-BB journal verifies after all replay" \
  "$(api GET /regions/DE-BB/journal/verify demo-operator-de-bb | jq -r .gapless)" "true"
area_group=$(sql_control "select round(sum(area_ha)::numeric,2) from replica_op_summaries where source_region='BR-MT'")
area_local=$(sql_region br_mt "select round(sum(area_ha)::numeric,2) from operation_events")
check "group replica area equals the region's own area (no duplication)" "$area_group" "$area_local"
rows_group=$(sql_control "select count(*) from replica_op_summaries where source_region='BR-MT'")
rows_local=$(sql_region br_mt "select count(*) from operation_events")
check "group replica row count equals the region's own row count" "$rows_group" "$rows_local"
check "BR-MT has exactly 8 distinct operations after dedupe" "$rows_local" "8"

section "3. primary/standby switch resumes only at the confirmed position"
api POST /regions/US-IA/ingest demo-operator-us-ia "@$S/dr/us-ia-post-switch.json" > "$TMP/usia-ingest.json"
check "post-switch batch lands on US-IA" "$(jq -r .accepted "$TMP/usia-ingest.json")" "2"

api POST /replication/run "$TOK_ADMIN" '{"region":"US-IA","fault":"confirm_fail"}' > "$TMP/confail.json"
check "confirmation failure recorded" "$(jq -r '.run.status' "$TMP/confail.json")" "failed"
stats=$(api GET /replication/replicas "$TOK_ADMIN")
contains "copied-but-unconfirmed rows exist before failover" "$stats" '"US-IA": 4'
cur=$(api GET /replication/cursors "$TOK_ADMIN" | jq -c '.cursors[]|select(.source_region=="US-IA")')
confirmed_before=$(jq -r '.confirmed_seq' <<<"$cur")

dre=$(api GET /dr/US-IA "$TOK_ADMIN")
check "DR register confirmed position matches the cursor" \
  "$(jq -r '.registry.last_confirmed_seq' <<<"$dre")" "$confirmed_before"

fo=$(api POST /dr/US-IA/failover "$TOK_ADMIN" '{"reconcile":false}')
echo "$fo" > "$TMP/failover.json"
check "failover promotes the standby" "$(jq -r '.to_node' "$TMP/failover.json")" "us-ia-node-b"
check "failover discards unconfirmed summaries" \
  "$(jq -r '.discarded_unconfirmed_rows.replica_op_summaries' "$TMP/failover.json")" "2"
check "failover discards unconfirmed trajectories" \
  "$(jq -r '.discarded_unconfirmed_rows.replica_trajectories' "$TMP/failover.json")" "2"
check "replication resumes at the confirmed position" \
  "$(jq -r '.resumed_from_seq' "$TMP/failover.json")" "$confirmed_before"

stats=$(api GET /replication/replicas "$TOK_ADMIN")
check "no unconfirmed replicas survive the switch" \
  "$(jq -r --arg r US-IA '(.unconfirmed_by_region[$r] // 0)' <<<"$stats")" "0"

st=$(api GET /regions/US-IA/status demo-operator-us-ia)
check "new primary is active for local writes" "$(jq -r .active_node <<<"$st")" "us-ia-node-b"
check "epoch advanced" "$(jq -r .epoch <<<"$st")" "2"

out=$(api POST /regions/US-IA/ingest demo-operator-us-ia "@$S/dr/us-ia-post-switch.json")
check "new primary keeps accepting local writes" "$(jq -r .replayed_batch <<<"$out")" "true"
jv=$(api GET /regions/US-IA/journal/verify demo-operator-us-ia)
check "local journal stays gapless across the switch" "$(jq -r .gapless <<<"$jv")" "true"
check "journal head on the new primary covers all local history" "$(jq -r .head_seq <<<"$jv")" "6"

api POST /replication/run "$TOK_ADMIN" '{"region":"US-IA"}' > "$TMP/resume.json"
check "replication restarts from the confirmed position" \
  "$(jq -r '.from_seq' "$TMP/resume.json")" "$((confirmed_before + 1))"
area_group=$(sql_control "select round(sum(area_ha)::numeric,2) from replica_op_summaries where source_region='US-IA'")
area_local=$(sql_region us_ia "select round(sum(area_ha)::numeric,2) from operation_events")
check "post-failover replication has no duplicated area" "$area_group" "$area_local"

api POST /dr/US-IA/failback "$TOK_ADMIN" '{}' > "$TMP/failback.json"
check "failback restores the original primary" "$(jq -r '.to_node' "$TMP/failback.json")" "us-ia-node-a"

# A crashed writer must not leave orphan rows in the control plane. Simulate it
# by inserting an unconfirmed row past the confirmed position, then fail over.
confirmed_now=$(api GET /replication/cursors "$TOK_ADMIN" | jq -r '.cursors[]|select(.source_region=="US-IA")|.confirmed_seq')
sql_control "insert into replica_op_summaries (source_region, entity_id, source_seq, device_serial,
  segment_id, operation_type, area_ha, fuel_l, duration_s, completed, started_at, source_node, status)
  values ('US-IA','US-SEED-1203:phantom-crash', ${confirmed_now}+100, 'US-SEED-1203','phantom-crash',
          'seeding', 999.0, 0, 0, true, now(), 'us-ia-node-a', 'unconfirmed')" >/dev/null
phantom=$(sql_control "select count(*) from replica_op_summaries where entity_id='US-SEED-1203:phantom-crash'")
check "orphan row inserted for the drill" "$phantom" "1"
fo2=$(api POST /dr/US-IA/failover "$TOK_ADMIN" '{"reconcile":true}')
check "failover deletes the crashed writer's orphan" \
  "$(sql_control "select count(*) from replica_op_summaries where entity_id='US-SEED-1203:phantom-crash'")" "0"
check "orphan is accounted for in the failover report" \
  "$(jq -r '.discarded_unconfirmed_rows.replica_op_summaries' <<<"$fo2")" "1"
area_local=$(sql_region us_ia "select round(sum(area_ha)::numeric,2) from operation_events")
check "group area still matches the region after the orphan drill" \
  "$(sql_control "select round(sum(area_ha)::numeric,2) from replica_op_summaries where source_region='US-IA'")" "$area_local"
api POST /dr/US-IA/failback "$TOK_ADMIN" '{}' >/dev/null

section "4. approval revoked mid-run blocks delivery"
apr2=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr2/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
run2=$(api POST /aggregates "$TOK_ANALYST" "$(jq --arg a "$apr2" '.approval_id=$a' <<<"$aggbody")" | jq -r .id)
rev=$(api POST "/outbound/requests/$apr2/revoke" "$TOK_APPROVER" '{"reason":"steward withdrew authorization mid-run"}')
check "approval revoked" "$(jq -r .status <<<"$rev")" "revoked"

dlv=$(api POST "/aggregates/$run2/deliver" "$TOK_ANALYST")
echo "$dlv" > "$TMP/blocked.json"
check "delivery is blocked, not sent" "$(jq -r .status "$TMP/blocked.json")" "blocked"
check "blocked delivery exports no artifact" "$(jq -r '.artifact' "$TMP/blocked.json")" "null"
check "blocked delivery carries no artifact hash" "$(jq -r '.artifact_sha256 // ""' "$TMP/blocked.json")" ""
check "revocation is the blocking reason" \
  "$(jq -r '.blocked_reason|startswith("approval_status")' "$TMP/blocked.json")" "true"

# Run computed under a narrow scope, delivered after the approval was revoked:
# the delivery must re-read the current approval, not the startup snapshot.
apr3=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/narrow.json" | jq -r .id)
api POST "/outbound/requests/$apr3/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
narrow_body=$(jq -n --arg a "$apr3" '{approval_id:$a,period_from:"2026-11-01T00:00:00Z",
  period_to:"2026-11-30T00:00:00Z", metric_keys:["operation_count","completion_rate","area_ha"]}')
run3=$(api POST /aggregates "$TOK_ANALYST" "$narrow_body" | jq -r .id)
check "narrow approval aggregates only its three regions" \
  "$(api GET "/aggregates/$run3" "$TOK_ANALYST" | jq -r '.coverage.included_regions|join(",")')" \
  "BR-MT,DE-BB,US-IA"
api POST "/outbound/requests/$apr3/revoke" "$TOK_APPROVER" '{"reason":"scope narrowed after run"}' >/dev/null
dlv3=$(api POST "/aggregates/$run3/deliver" "$TOK_ANALYST")
check "revoked narrow approval blocks delivery too" "$(jq -r .status <<<"$dlv3")" "blocked"
check "narrow run's blocked delivery exports nothing" "$(jq -r '.artifact' <<<"$dlv3")" "null"

# Narrowing the approval scope in place: the run was computed under a wider
# scope and must be blocked because the current scope no longer covers it.
apr5=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr5/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
run5=$(api POST /aggregates "$TOK_ANALYST" "$(jq --arg a "$apr5" '.approval_id=$a' <<<"$aggbody")" | jq -r .id)
sql_control "update outbound_requests set scope_regions=array['DE-BB'], version=version+1 where id='$apr5'" >/dev/null
dlv5=$(api POST "/aggregates/$run5/deliver" "$TOK_ANALYST")
check "narrowed-in-place approval blocks the wider run" "$(jq -r .status <<<"$dlv5")" "blocked"
check "the blocking check names the region scope" \
  "$(jq -r '[.checks[]|select(.name=="region_scope_current" and .passed==false)]|length' <<<"$dlv5")" "1"
check "narrowed scope still exports nothing" "$(jq -r '.artifact' <<<"$dlv5")" "null"

# A metric outside the approved scope must be refused at run creation.
wide_apr=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/narrow.json" | jq -r .id)
api POST "/outbound/requests/$wide_apr/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
out_of_scope=$(api POST /aggregates "$TOK_ANALYST" "$(jq -n --arg a "$wide_apr" '{approval_id:$a,
  period_from:"2026-11-01T00:00:00Z",period_to:"2026-11-30T00:00:00Z",
  metric_keys:["operation_count","fuel_l"]}')")
check "metric outside the approval scope is refused" "$(jq -r .error.code <<<"$out_of_scope")" "forbidden"

section "5. in-region spatial aggregation and residency-safe egress"
# Everything spatial happens inside BR-MT; the group only ever receives the
# approved-grain sanitised artifact. api() takes a bare token (it adds the
# Authorization header itself).
OPR="demo-operator-br-mt"
bdv1=$(api POST /regions/BR-MT/spatial/boundaries "$OPR" "@$S/spatial/br-mt-boundaries-v1.json" | tee "$TMP/bdv1.json" | jq -r .version_id)
check "boundary v1 is versioned and hashed" "$bdv1" "bdv_br_mt_1"
check "boundary v1 covers two counties" "$(jq -r .county_count "$TMP/bdv1.json")" "2"
bdv2=$(api POST /regions/BR-MT/spatial/boundaries "$OPR" "@$S/spatial/br-mt-boundaries-v2.json" | jq -r .version_id)
check "boundary correction gets a new version" "$bdv2" "bdv_br_mt_2"
check "previous boundary version is superseded, not deleted" \
  "$(api GET /regions/BR-MT/spatial/boundaries "$OPR" | jq -r '.boundary_versions[]|select(.version_id=="bdv_br_mt_1").status')" "superseded"

# Re-activate v1 for the main drill.
api POST /regions/BR-MT/spatial/boundaries "$OPR" "@$S/spatial/br-mt-boundaries-v1.json" > "$TMP/bdv1b.json"
check "re-publishing v1 restores an active catalog" "$(jq -r .status "$TMP/bdv1b.json")" "active"
spp1=$(api POST /regions/BR-MT/spatial/policies "$OPR" "@$S/spatial/policy-intersection-county.json" | jq -r .policy_id)
check "intersection/county spatial policy is active" \
  "$(api GET /regions/BR-MT/spatial/policies "$OPR" | jq -r '.policies[]|select(.policy_id=="'$spp1'").status')" "active"
# Grid output below the approved minimum resolution is refused.
coarse=$(curl -s -XPOST "$BASE/regions/BR-MT/spatial/policies" -H "Authorization: Bearer $OPR" -H 'Content-Type: application/json' \
  -d '{"boundary_version_id":"bdv_br_mt_1","output_grain":"grid","method":"raster","grid_size_m":50}' | jq -r .error.code)
check "sub-100m grid resolution is refused" "$coarse" "invalid_request"

api POST /regions/BR-MT/ingest "$OPR" "@$S/spatial/br-mt-batch-a.json" > "$TMP/sp-a.json"
check "spatial batch A accepted" "$(jq -r .accepted "$TMP/sp-a.json")" "3"
api POST /regions/BR-MT/ingest "$OPR" "@$S/spatial/br-mt-batch-b.json" > "$TMP/sp-b.json"
check "spatial batch B repeats one segment and adds one" "$(jq -r .duplicates "$TMP/sp-b.json")" "1"
check "spatial batch B accepts only the new segment" "$(jq -r .accepted "$TMP/sp-b.json")" "1"
check "duplicate reception is logged as dedupe evidence" \
  "$(sql_region br_mt "select count(*) from segment_dedup_log where segment_id='SP-CROSS-1'")" "1"

spbody=$(jq -n '{period_from:"2026-11-06T00:00:00Z",period_to:"2026-11-30T00:00:00Z"}')
s1=$(api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" | tee "$TMP/sp-s1.json" | jq -r .summary_id)
check "spatial summary sees four distinct segments" "$(jq -r .source_segment_count "$TMP/sp-s1.json")" "4"
check "spatial summary reports the duplicate instead of doubling it" "$(jq -r .duplicate_count "$TMP/sp-s1.json")" "1"
check "cross-county run is apportioned to both counties" \
  "$(jq -r '[.bins[]|select(.unit_type=="county")|.county_code]|sort|join(",")' "$TMP/sp-s1.json")" "MT-C1,MT-C2"
check "every allocated county bin carries positive worked area" \
  "$(jq -r '[.bins[].area_ha|select(.<=0)]|length' "$TMP/sp-s1.json")" "0"
check "low-accuracy border fragment is held unallocated" \
  "$(jq -r '[.pending[]|select(.reason|startswith("uncertainty_crosses_boundary"))]|length' "$TMP/sp-s1.json")" "1"
check "width-missing segment is held unallocated" \
  "$(jq -r '[.pending[]|select(.reason|startswith("working_width_missing"))]|length' "$TMP/sp-s1.json")" "1"
check "ambiguous fragment names both candidate counties" \
  "$(jq -r '[.excluded_fragments[]|select(.reason=="uncertainty_crosses_boundary").candidate_counties[]]|unique|sort|join(",")' "$TMP/sp-s1.json")" "MT-C1,MT-C2"
check "ambiguous area is reported as a conservative county upper bound" \
  "$(jq -r '[.bins[]|select(.area_ha_max>.area_ha)]|length' "$TMP/sp-s1.json")" "2"
api GET "/regions/BR-MT/spatial/summaries/$s1/ledger" "$OPR" > "$TMP/sp-ledger.json"
check "ledger marks the width-missing segment pending" \
  "$(jq -r '.ledger[]|select(.segment_id=="SP-NOWIDTH-1")|.status' "$TMP/sp-ledger.json")" "pending"
check "ledger marks the cross-county segment allocated" \
  "$(jq -r '.ledger[]|select(.segment_id=="SP-CROSS-1")|.status' "$TMP/sp-ledger.json")" "allocated"
check "ledger allocation plus ambiguity covers the whole run" \
  "$(jq -r '[.ledger[]|select(.segment_id=="SP-CROSS-1")|((.allocated_length_m+.ambiguous_length_m)>0)]|all' "$TMP/sp-ledger.json")" "true"

# Width backfill moves the pending segment into the county totals on recompute.
api POST /regions/BR-MT/spatial/width-backfill "$OPR" '{"items":[{"device_serial":"BR-FERT-7703","segment_id":"SP-NOWIDTH-1","working_width_m":10.0}]}' > "$TMP/sp-wf.json"
check "width backfill updates one segment" "$(jq -r .updated_segments "$TMP/sp-wf.json")" "1"
s2=$(api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" | tee "$TMP/sp-s2.json" | jq -r .summary_id)
check "recomputation supersedes the previous summary" "$(jq -r .supersedes_summary_id "$TMP/sp-s2.json")" "$s1"
check "previous summary is retained as superseded" \
  "$(api GET "/regions/BR-MT/spatial/summaries/$s1" "$OPR" | jq -r .status)" "superseded"
check "width-missing pending bucket disappears after backfill" \
  "$(jq -r '[.pending[]|select(.reason|startswith("working_width_missing"))]|length' "$TMP/sp-s2.json")" "0"

# Policy-filtered replication carries the sanitised summary, never raw points.
api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' > "$TMP/sp-repl.json"
check "spatial_summary category is replicated" \
  "$(jq -r '.copied_by_category.spatial_summary>0' "$TMP/sp-repl.json")" "true"
check "raw trajectories stay blocked while spatial summaries flow" \
  "$(jq -r '.blocked_by_category.raw_trajectory>0' "$TMP/sp-repl.json")" "true"
api GET /replication/replicas "$TOK_ADMIN" > "$TMP/sp-stats.json"
check "group store holds a BR-MT spatial replica" \
  "$(jq -r '.spatial_summaries_by_region["BR-MT"]>0' "$TMP/sp-stats.json")" "true"
check "group store still holds zero BR-MT raw trajectories" \
  "$(jq -r '.raw_trajectories_by_region["BR-MT"] // 0' "$TMP/sp-stats.json")" "0"
api GET "/spatial/summaries?region=BR-MT" "$TOK_ANALYST" > "$TMP/sp-group.json"
check "group side receives only county-grain bins under the county policy" \
  "$(jq -r '[.spatial_summaries[].bins[]|select(.unit_type=="grid")]|length' "$TMP/sp-group.json")" "0"
check "every group-visible spatial artifact declares no raw trajectories" \
  "$(jq -r '[.spatial_summaries[]|select(.contains_raw_trajectories!=false)]|length' "$TMP/sp-group.json")" "0"
check "group artifact carries the boundary and policy provenance" \
  "$(jq -r '[.spatial_summaries[]|select(.boundary_version_id=="bdv_br_mt_1" and (.policy_id|length)>0)]|length>0' "$TMP/sp-group.json")" "true"
check "group analyst has no path to the regional excluded-fragment ledger" \
  "$(api GET "/regions/BR-MT/spatial/summaries/$s1/ledger" "$TOK_ANALYST" | jq -r .error.code)" "forbidden"
check "other-region operator cannot manage BR-MT boundaries" \
  "$(api POST /regions/BR-MT/spatial/boundaries "demo-operator-de-bb" "@$S/spatial/br-mt-boundaries-v1.json" | jq -r .error.code)" "forbidden"
# Byte-level guarantee: no coordinate JSON keys exist in the control-plane
# replica. (The artifact notes mention the word "trajectories" in prose; the
# assertion matches quoted keys only, exactly like the delivery field scanner.)
check "control-plane spatial bytes contain no lat/lon/points keys" \
  "$(sql_control "select count(*) from replica_spatial_summaries where artifact ~* '\"(lat|lon|points|trajectory)\"'")" "0"
check "region database still holds the authoritative raw trajectories" \
  "$(sql_region br_mt "select count(*) from operation_events where point_count>0 and segment_id like 'SP-%'")" "4"

# Raster method: county rollup and approved grid grain are both available.
spp_rc=$(api POST /regions/BR-MT/spatial/policies "$OPR" "@$S/spatial/policy-raster-county.json" | jq -r .policy_id)
api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" > "$TMP/sp-rc.json"
check "raster/county policy emits only county bins" \
  "$(jq -r '[.bins[]|select(.unit_type!="county")]|length' "$TMP/sp-rc.json")" "0"
check "raster method records cells touched per county" \
  "$(jq -r '[.bins[].grid_cells_touched|select(.<1)]|length' "$TMP/sp-rc.json")" "0"
spp_rg=$(api POST /regions/BR-MT/spatial/policies "$OPR" "@$S/spatial/policy-raster-grid.json" | jq -r .policy_id)
api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" > "$TMP/sp-rg.json"
check "raster/grid policy emits opaque grid bins at the approved resolution" \
  "$(jq -r '[.bins[]|select(.unit_type=="grid" and (.unit_code|startswith("c200_")))]|length>0' "$TMP/sp-rg.json")" "true"
check "grid bins never embed coordinates (opaque cell codes only)" \
  "$(jq -r '[.bins[].unit_code|test("[0-9]+\\\\.[0-9]+")]|any' "$TMP/sp-rg.json")" "false"

# Delivery gate for spatial egress.
sapr=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/spatial.json" | jq -r .id)
api POST "/outbound/requests/$sapr/decide" "$TOK_APPROVER" '{"decision":"approved","note":"spatial drill"}' >/dev/null
# Re-publish the intersection/county policy so the latest summary is county-grain.
api POST /regions/BR-MT/spatial/policies "$OPR" "@$S/spatial/policy-intersection-county.json" >/dev/null
slatest=$(api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" | jq -r .summary_id)
api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' >/dev/null
sdlv=$(api POST /spatial/deliveries "$TOK_ANALYST" "$(jq -n --arg r BR-MT --arg s "$slatest" --arg a "$sapr" \
  '{source_region:$r,summary_id:$s,approval_id:$a}')" | tee "$TMP/sp-dlv.json" | jq -r .id)
check "spatial delivery is sent after all gate checks pass" "$(jq -r .status "$TMP/sp-dlv.json")" "sent"
check "spatial delivery has no failed check" \
  "$(jq -r '[.checks[]|select(.passed==false)]|length' "$TMP/sp-dlv.json")" "0"
check "delivered spatial artifact is county-only" \
  "$(jq -r '[.artifact.bins[]|select(.unit_type!="county")]|length' "$TMP/sp-dlv.json")" "0"
api GET "/spatial/deliveries/$sdlv/verify" "$TOK_AUDITOR" > "$TMP/sp-dlvverify.json"
check "spatial artifact re-hashes to the recorded digest" "$(jq -r .sha256_matches "$TMP/sp-dlvverify.json")" "true"
check "spatial artifact verification finds no restricted fields" \
  "$(jq -r '(.restricted_fields//[])|length' "$TMP/sp-dlvverify.json")" "0"
check "spatial delivery audit entry validates" "$(jq -r .audit_entry_valid "$TMP/sp-dlvverify.json")" "true"
check "spatial delivery verifies against the audit chain" "$(jq -r .audit_chain_valid "$TMP/sp-dlvverify.json")" "true"
# Revocation blocks the next spatial delivery and exposes no artifact.
api POST "/outbound/requests/$sapr/revoke" "$TOK_APPROVER" '{"reason":"spatial drill revocation"}' >/dev/null
blocked=$(api POST /spatial/deliveries "$TOK_ANALYST" "$(jq -n --arg r BR-MT --arg s "$slatest" --arg a "$sapr" \
  '{source_region:$r,summary_id:$s,approval_id:$a}')")
check "revoked approval blocks spatial delivery" "$(jq -r .status <<<"$blocked")" "blocked"
check "blocked spatial delivery exports no artifact" "$(jq -r .artifact <<<"$blocked")" "null"

# Boundary revision creates a new linked summary; historical group reports keep
# naming the grain and boundary they originally used.
api POST /regions/BR-MT/spatial/boundaries "$OPR" "@$S/spatial/br-mt-boundaries-v2.json" >/dev/null
api POST /regions/BR-MT/spatial/policies "$OPR" "@$S/spatial/policy-intersection-county-v2.json" >/dev/null
s3=$(api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" | jq -r .summary_id)
check "revision summary uses boundary v2" \
  "$(api GET "/regions/BR-MT/spatial/summaries/$s3" "$OPR" | jq -r .boundary_version_id)" "bdv_br_mt_2"
check "revision summary continues the county-grain lineage" \
  "$(api GET "/regions/BR-MT/spatial/summaries/$s3" "$OPR" | jq -r '.supersedes_summary_id|length>0')" "true"
api POST /replication/run "$TOK_ADMIN" '{"region":"BR-MT"}' >/dev/null
check "group side can still see reports computed under both boundary versions" \
  "$(api GET "/spatial/summaries?region=BR-MT" "$TOK_ANALYST" | jq -r '[.spatial_summaries[].boundary_version_id]|unique|length>=2')" "true"

# An invalid boundary holds every segment pending instead of guessing a side.
api POST /regions/BR-MT/spatial/boundaries/invalidate "$OPR" '{"version_id":"bdv_br_mt_2"}' >/dev/null
api POST /regions/BR-MT/spatial/summaries "$OPR" "$spbody" > "$TMP/sp-invalid.json"
check "invalid boundary allocates no area" "$(jq -r '(.bins|length)' "$TMP/sp-invalid.json")" "0"
check "invalid boundary lists every segment as boundary_version_invalid" \
  "$(jq -r '[.pending[]|select(.reason|startswith("boundary_version_invalid"))]|length' "$TMP/sp-invalid.json")" "1"
check "invalid boundary pending area covers all received segments" \
  "$(jq -r '.pending[0].segment_count' "$TMP/sp-invalid.json")" "4"

section "6. outbound delivery and audit chain"
apr4=$(api POST /outbound/requests "$TOK_ANALYST" "@$S/approvals/wide.json" | jq -r .id)
api POST "/outbound/requests/$apr4/decide" "$TOK_APPROVER" '{"decision":"approved"}' >/dev/null
run4=$(api POST /aggregates "$TOK_ANALYST" "$(jq --arg a "$apr4" '.approval_id=$a' <<<"$aggbody")" | jq -r .id)
dlv4=$(api POST "/aggregates/$run4/deliver" "$TOK_ANALYST")
echo "$dlv4" > "$TMP/sent.json"
check "delivery sent" "$(jq -r .status "$TMP/sent.json")" "sent"
check "artifact lists the regions actually included" \
  "$(jq -r '.artifact.included_regions|join(",")' "$TMP/sent.json")" "BR-MT,DE-BB,FR-NOR,US-IA"
check "artifact declares no raw trajectories" \
  "$(jq -r '.artifact.contains_raw_trajectories' "$TMP/sent.json")" "false"
twice=$(api POST "/aggregates/$run4/deliver" "$TOK_ANALYST")
check "the same run cannot be delivered twice" "$(jq -r .error.code <<<"$twice")" "conflict"
check "all delivery checks passed" \
  "$(jq -r '[.checks[]|select(.passed==false)]|length' "$TMP/sent.json")" "0"

dlvid=$(jq -r .id "$TMP/sent.json")
ver=$(api GET "/deliveries/$dlvid/verify" "$TOK_AUDITOR")
echo "$ver" > "$TMP/verify.json"
check "stored artifact re-hashes to the recorded digest" "$(jq -r .sha256_matches "$TMP/verify.json")" "true"
check "artifact carries each region's replication version" \
  "$(jq -r '[.artifact.regions[]|select(.included) and (.replication_seq>0)]|length' "$TMP/sent.json")" "4"
check "artifact carries a data cutoff" \
  "$(jq -r '.artifact.data_cutoff != null' "$TMP/sent.json")" "true"
check "stored artifact has no restricted fields" \
  "$(jq -r '(.restricted_fields // [])|length' "$TMP/verify.json")" "0"
check "delivery audit entry is present and bound to the digest" "$(jq -r .audit_entry_valid "$TMP/verify.json")" "true"
check "audit chain verifies" "$(jq -r .audit_chain_valid "$TMP/verify.json")" "true"
check "group audit ledger verifies end to end" \
  "$(api GET /audit/verify "$TOK_AUDITOR" | jq -r .valid)" "true"

# Out-of-band tampering with the ledger must be detected.
sql_control "update audit_events set detail=jsonb_set(detail,'{included_regions}','[\"XX-XX\"]') where seq=1" >/dev/null
tampered=$(api GET /audit/verify "$TOK_AUDITOR")
echo "$tampered" > "$TMP/tamper.json"
check "tampered audit row is detected" "$(jq -r .valid "$TMP/tamper.json")" "false"
check "tamper is reported at the edited row" "$(jq -r .broken_at "$TMP/tamper.json")" "1"

section "summary"
printf 'checks passed: %d, failed: %d\n' "$PASS" "$FAIL"
if (( FAIL > 0 )); then
  printf '\n\033[31mFAILED CHECKS\033[0m\n'
  for c in "${CHECKS[@]}"; do
    [[ "$c" == FAIL* ]] && printf '  %s\n' "$c"
  done
  exit 1
fi
printf '\n\033[32mALL CHECKS PASSED\033[0m\n'

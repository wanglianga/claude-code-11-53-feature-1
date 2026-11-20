-- Control-plane extension for in-region spatial aggregation.
--
-- The group side stores only sanitised spatial summaries: county/grid bins at
-- the approved grain, pending area with reasons, and provenance. No boundary
-- rings, per-point accuracy vectors, trajectory points or fine grid cells ever
-- reach this database.

-- residency_policies gains the spatial_summary category.
do $$
declare c record;
begin
  for c in
    select conname from pg_constraint
    where conrelid = 'residency_policies'::regclass and contype = 'c'
  loop
    execute 'alter table residency_policies drop constraint ' || quote_ident(c.conname);
  end loop;
end $$;

alter table residency_policies
  add constraint residency_policies_category_check
  check (category in ('raw_trajectory','operation_summary','device_alarm','aggregate_metric','spatial_summary'));

-- Confirmed/unconfirmed replicas of regional spatial summaries. The artifact
-- is stored as exact text bytes for the same hash-stability reason as
-- deliveries.artifact.
create table if not exists replica_spatial_summaries (
  source_region        text not null,
  summary_id           text not null,
  source_seq           bigint not null,
  policy_id            text not null,
  boundary_version_id  text not null,
  boundary_sha256      text not null,
  method               text not null,
  output_grain         text not null,
  grid_size_m          integer,
  period_from          timestamptz not null,
  period_to            timestamptz not null,
  operation_type       text not null,
  artifact             text not null,
  artifact_sha256      text not null,
  source_node          text not null,
  status               text not null check (status in ('confirmed','unconfirmed')),
  copied_at            timestamptz not null default now(),
  primary key (source_region, summary_id)
);
create index if not exists replica_spatial_period_idx
  on replica_spatial_summaries (source_region, period_from, period_to);

-- Spatial delivery records mirror the existing delivery gate: re-validate the
-- approval and residency policy immediately before sending, store the exact
-- bytes that were hashed, and bind the row to the audit ledger.
create table if not exists spatial_deliveries (
  id              text primary key,
  source_region   text not null,
  summary_id      text not null,
  approval_id     text not null,
  status          text not null check (status in ('sent','blocked')),
  channel         text not null default 'secure_store',
  checks          jsonb not null default '[]'::jsonb,
  blocked_reason  text,
  artifact        text,
  artifact_sha256 text,
  audit_seq       bigint,
  requested_by    text not null,
  created_at      timestamptz not null default now(),
  sent_at         timestamptz
);

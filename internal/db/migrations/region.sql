-- Per-region operational schema. Each region has its own PostgreSQL database;
-- raw trajectories written here are never copied out unless residency policy
-- explicitly allows the raw_trajectory category to leave the region.
-- Local writes commit locally even while the inter-region link is down.

create table if not exists region_state (
  id                boolean primary key default true check (id),
  region_code       text not null,
  active_node       text not null,
  local_epoch       bigint not null default 1,
  next_journal_seq  bigint not null default 1,
  next_outbox_seq   bigint not null default 1,
  updated_at        timestamptz not null default now()
);

create table if not exists devices (
  device_serial  text primary key,
  device_type    text not null default 'seeder',
  model          text not null default '',
  first_seen_node text not null,
  created_at     timestamptz not null default now()
);

-- Local, authoritative operation records. Deduplication key is the device
-- serial plus the region that sourced the segment; the region is implicit in
-- the database boundary and explicit in every cross-region replica.
create table if not exists operation_events (
  id            text primary key,
  journal_seq   bigint not null unique,
  device_serial text not null,
  segment_id    text not null,
  operation_type text not null check (operation_type in ('seeding','harvesting','fertilizing')),
  field_id      text not null default '',
  area_ha       double precision not null check (area_ha >= 0),
  fuel_l        double precision not null default 0 check (fuel_l >= 0),
  duration_s    integer not null default 0 check (duration_s >= 0),
  completed     boolean not null default false,
  started_at    timestamptz not null,
  ended_at      timestamptz,
  point_count   integer not null default 0,
  points        jsonb not null default '[]'::jsonb,
  alarms        jsonb not null default '[]'::jsonb,
  upload_batch  text not null default '',
  source_node   text not null,
  source_epoch  bigint not null default 1,
  received_at   timestamptz not null default now(),
  unique (device_serial, segment_id)
);
create index if not exists operation_events_started_idx on operation_events (started_at);
create index if not exists operation_events_type_idx on operation_events (operation_type);

-- Gapless local journal. The next sequence is allocated under a row lock in
-- the same transaction as the operation insert, so a region that keeps
-- recording through an outage has a contiguous, provable local history.
create table if not exists journal_entries (
  seq          bigint primary key,
  entity_type  text not null,
  entity_id    text not null,
  source_node  text not null,
  created_at   timestamptz not null default now()
);

-- Transactional outbox. Sequence is gapless and allocated under the same row
-- lock as the journal entry. One operation fans out into a summary entry, a
-- raw trajectory entry and one entry per device alarm; replication filters
-- each category against residency policy.
create table if not exists outbox_events (
  seq          bigint primary key,
  entity_type  text not null,
  entity_id    text not null,
  category     text not null check (category in ('raw_trajectory','operation_summary','device_alarm','aggregate_metric')),
  payload      jsonb not null,
  source_node  text not null,
  source_epoch bigint not null,
  created_at   timestamptz not null default now()
);
create index if not exists outbox_events_category_idx on outbox_events (category, seq);

-- In-region aggregate snapshots. Only these aggregates, never raw points, are
-- eligible for outbound reports.
create table if not exists metric_snapshots (
  snapshot_id  text primary key,
  period_from  timestamptz not null,
  period_to    timestamptz not null,
  data_cutoff  timestamptz not null,
  computed_at  timestamptz not null,
  metrics      jsonb not null,
  source_node  text not null,
  source_epoch bigint not null,
  outbox_seq   bigint not null
);
create index if not exists metric_snapshots_period_idx on metric_snapshots (period_from, period_to);

create table if not exists ingest_receipts (
  batch_id       text primary key,
  content_sha256 text not null,
  accepted       integer not null,
  duplicates     integer not null,
  first_journal  bigint,
  last_journal   bigint,
  created_at     timestamptz not null default now()
);

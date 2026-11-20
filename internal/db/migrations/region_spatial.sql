-- Spatial aggregation extension for the regional schema.
--
-- Everything in this migration stays inside the region database: boundary
-- rings, per-point accuracy, working widths, trajectory cut pieces and the
-- list of excluded fuzzy fragments. The only thing that ever leaves is the
-- sanitised spatial summary produced by the aggregation service.

-- The outbox gains one more category: spatial_summary is an in-region
-- aggregate, never a raw trajectory. Rewrite the column check to admit it.
do $$
declare c record;
begin
  for c in
    select conname from pg_constraint
    where conrelid = 'outbox_events'::regclass and contype = 'c'
  loop
    execute 'alter table outbox_events drop constraint ' || quote_ident(c.conname);
  end loop;
end $$;

alter table outbox_events
  add constraint outbox_events_category_check
  check (category in ('raw_trajectory','operation_summary','device_alarm','aggregate_metric','spatial_summary'));

-- Versioned administrative boundary catalog. A boundary correction inserts a
-- new version and supersedes the old one; historical summaries keep pointing
-- at the version they were actually computed under.
create table if not exists boundary_versions (
  version_id      text primary key,
  region_code     text not null,
  status          text not null default 'active'
                    check (status in ('active','superseded','invalid')),
  boundary_sha256 text not null,
  effective_from  timestamptz not null default now(),
  superseded_by   text,
  note            text not null default '',
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now()
);
create index if not exists boundary_versions_region_idx on boundary_versions (region_code, status);

-- County rings as [[lat, lon], ...]. The exact ring payload hashes into
-- boundary_versions.boundary_sha256 so every summary can name the precise
-- geometry it used.
create table if not exists boundary_counties (
  version_id  text not null references boundary_versions(version_id) on delete cascade,
  county_code text not null,
  county_name text not null default '',
  ring        jsonb not null,
  primary key (version_id, county_code)
);

-- The spatial policy binds one boundary version, one allocation method and
-- one approved output grain. Directory, cut/grid processing, outbound
-- aggregation and audit delivery all reference the same row.
create table if not exists spatial_policies (
  policy_id           text primary key,
  region_code         text not null,
  boundary_version_id text not null,
  output_grain        text not null check (output_grain in ('county','grid')),
  method              text not null check (method in ('intersection','raster')),
  -- Approved export resolution for grid grain, and the coarsest cell size a
  -- grid-grain export is allowed to use. Finer cells never leave the region.
  grid_size_m         integer check (grid_size_m is null or grid_size_m >= 100),
  default_accuracy_m  double precision not null default 0 check (default_accuracy_m >= 0),
  status              text not null default 'active'
                        check (status in ('active','superseded')),
  created_at          timestamptz not null default now()
);
create index if not exists spatial_policies_region_idx on spatial_policies (region_code, status);

-- Per-operation spatial metadata. A null working_width_m means the segment
-- cannot be apportioned yet: its area is held pending until the width arrives.
create table if not exists operation_spatial_meta (
  operation_id    text primary key,
  device_serial   text not null,
  segment_id      text not null,
  working_width_m double precision check (working_width_m is null or working_width_m > 0),
  accuracy_m      double precision not null default 0 check (accuracy_m >= 0),
  -- Per-sample accuracy radii in metres; empty means "use accuracy_m".
  point_accuracy  jsonb not null default '[]'::jsonb,
  updated_at      timestamptz not null default now()
);

-- Every duplicate reception is logged here, so a later summary can prove that
-- repeated fragments were deduped instead of double-counted.
create table if not exists segment_dedup_log (
  id            bigserial primary key,
  device_serial text not null,
  segment_id    text not null,
  batch_id      text not null,
  content_sha256 text not null,
  received_at   timestamptz not null default now()
);
create index if not exists segment_dedup_log_seg_idx
  on segment_dedup_log (device_serial, segment_id);

-- Outbound-eligible spatial summaries. artifact holds the exact sanitised
-- JSON bytes that were hashed and replicated; bins/pending/excluded_fragments
-- hold the full regional detail the group side never receives.
create table if not exists spatial_summaries (
  summary_id            text primary key,
  region_code           text not null,
  policy_id             text not null,
  boundary_version_id   text not null,
  boundary_sha256       text not null,
  method                text not null,
  output_grain          text not null,
  grid_size_m           integer,
  period_from           timestamptz not null,
  period_to             timestamptz not null,
  operation_type        text not null,
  status                text not null check (status in ('computed','superseded','replicated')),
  supersedes_summary_id text,
  data_cutoff           timestamptz not null,
  bins                  jsonb not null,
  pending               jsonb not null,
  excluded_fragments    jsonb not null,
  source_segment_count  integer not null,
  duplicate_count       integer not null default 0,
  artifact              text not null,
  artifact_sha256       text not null,
  outbox_seq            bigint,
  source_node           text not null,
  source_epoch          bigint not null default 1,
  created_at            timestamptz not null default now()
);
create index if not exists spatial_summaries_period_idx
  on spatial_summaries (region_code, period_from, period_to);

-- Per-segment processing ledger for one summary: which pieces were allocated
-- to which county, which length is ambiguous, and why the pending part is
-- held. Rows are replaced when the same summary is recomputed.
create table if not exists spatial_segment_ledger (
  id                 text primary key,
  summary_id         text not null references spatial_summaries(summary_id) on delete cascade,
  operation_id       text not null,
  device_serial      text not null,
  segment_id         text not null,
  status             text not null check (status in ('allocated','pending','excluded')),
  pending_reason     text,
  boundary_version_id text not null,
  policy_id          text not null,
  method             text not null,
  working_width_m    double precision,
  declared_area_ha   double precision not null default 0,
  total_length_m     double precision not null,
  allocated_length_m double precision not null,
  ambiguous_length_m double precision not null,
  allocated_area_ha  double precision not null default 0,
  pending_area_ha    double precision not null default 0,
  duration_s         integer not null default 0,
  candidates         jsonb not null default '[]'::jsonb,
  content_sha256     text not null,
  created_at         timestamptz not null default now(),
  unique (summary_id, operation_id)
);
create index if not exists spatial_segment_ledger_op_idx
  on spatial_segment_ledger (operation_id, boundary_version_id);

-- Idempotent upgrades for databases created before columns were added.
alter table boundary_versions add column if not exists updated_at timestamptz not null default now();

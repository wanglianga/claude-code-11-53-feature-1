package service

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	"agrisync/internal/model"
)

// UploadBoundary stores a new versioned administrative boundary catalog in the
// region database. The previous active version is superseded but retained:
// historical summaries keep naming the geometry they were computed under.
func (s *Service) UploadBoundary(ctx context.Context, region string, in model.BoundaryUpload, actor, role string) (model.BoundaryVersion, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.BoundaryVersion{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if len(in.Counties) == 0 {
		return model.BoundaryVersion{}, fmt.Errorf("%w: at least one county polygon is required", ErrInvalid)
	}
	type canonicalCounty struct {
		CountyCode string      `json:"county_code"`
		CountyName string      `json:"county_name"`
		Ring       [][]float64 `json:"ring"`
	}
	canonical := make([]canonicalCounty, 0, len(in.Counties))
	seen := map[string]bool{}
	for _, c := range in.Counties {
		if c.CountyCode == "" {
			return model.BoundaryVersion{}, fmt.Errorf("%w: county_code is required", ErrInvalid)
		}
		if seen[c.CountyCode] {
			return model.BoundaryVersion{}, fmt.Errorf("%w: duplicate county %q", ErrInvalid, c.CountyCode)
		}
		seen[c.CountyCode] = true
		if len(c.Ring) < 3 {
			return model.BoundaryVersion{}, fmt.Errorf("%w: county %s ring needs at least 3 points", ErrInvalid, c.CountyCode)
		}
		for _, ll := range c.Ring {
			if len(ll) != 2 || ll[0] < -90 || ll[0] > 90 || ll[1] < -180 || ll[1] > 180 {
				return model.BoundaryVersion{}, fmt.Errorf("%w: county %s has an out-of-range ring point", ErrInvalid, c.CountyCode)
			}
		}
		canonical = append(canonical, canonicalCounty{
			CountyCode: c.CountyCode, CountyName: c.CountyName, Ring: c.Ring,
		})
	}
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].CountyCode < canonical[j].CountyCode })
	boundarySHA := sha256Hex(jsonBytes(canonical))

	tx, err := pool.Begin(ctx)
	if err != nil {
		return model.BoundaryVersion{}, err
	}
	defer tx.Rollback(ctx)
	now := s.now().UTC()

	// Idempotent re-publication: identical geometry returns the existing
	// version and re-activates it, so steward tooling and policy documents can
	// keep referring to stable bdv_<region>_<n> identifiers.
	var existingID, existingStatus string
	findErr := tx.QueryRow(ctx, `
		select version_id, status from boundary_versions
		where region_code=$1 and boundary_sha256=$2`, region, boundarySHA).Scan(&existingID, &existingStatus)
	if findErr == nil {
		if _, err := tx.Exec(ctx, `
			update boundary_versions set status='superseded', updated_at=now()
			where region_code=$1 and version_id <> $2`, region, existingID); err != nil {
			return model.BoundaryVersion{}, err
		}
		if _, err := tx.Exec(ctx, `
			update boundary_versions set status='active', superseded_by=null, updated_at=now()
			where version_id=$1`, existingID); err != nil {
			return model.BoundaryVersion{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return model.BoundaryVersion{}, err
		}
		out := model.BoundaryVersion{
			VersionID: existingID, RegionCode: region, Status: "active", BoundarySHA: boundarySHA,
			EffectiveFrom: now, Note: in.Note, CountyCount: len(canonical), CreatedAt: now,
		}
		if _, err := s.AppendAudit(ctx, model.AuditEvent{
			Actor: actor, Role: role, Action: "spatial.boundary.reactivate", SubjectType: "boundary_version",
			SubjectID: existingID, RegionCode: region, Decision: "allow",
			Detail: map[string]any{"boundary_version_id": existingID, "boundary_sha256": boundarySHA},
		}); err != nil {
			return out, err
		}
		return out, nil
	} else if !errors.Is(findErr, pgx.ErrNoRows) {
		return model.BoundaryVersion{}, findErr
	}

	var seq int
	if err := tx.QueryRow(ctx,
		`select count(*) from boundary_versions where region_code=$1`, region).Scan(&seq); err != nil {
		return model.BoundaryVersion{}, err
	}
	versionID := fmt.Sprintf("bdv_%s_%d", sanitize(region), seq+1)
	if _, err := tx.Exec(ctx, `
		update boundary_versions set status='superseded', superseded_by=$2, updated_at=now()
		where region_code=$1 and status='active'`, region, versionID); err != nil {
		return model.BoundaryVersion{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into boundary_versions (version_id, region_code, status, boundary_sha256, effective_from, note, created_at)
		values ($1,$2,'active',$3,$4,$5,$6)`,
		versionID, region, boundarySHA, now, in.Note, now); err != nil {
		return model.BoundaryVersion{}, err
	}
	// Policies bound to a superseded boundary can no longer produce new
	// summaries; the steward publishes a replacement policy explicitly.
	if _, err := tx.Exec(ctx, `
		update spatial_policies set status='superseded', created_at=created_at
		where region_code=$1 and status='active' and boundary_version_id in
		  (select version_id from boundary_versions where region_code=$1 and status='superseded')`,
		region); err != nil {
		return model.BoundaryVersion{}, err
	}
	for _, c := range canonical {
		if _, err := tx.Exec(ctx, `
			insert into boundary_counties (version_id, county_code, county_name, ring)
			values ($1,$2,$3,$4)`,
			versionID, c.CountyCode, c.CountyName, jsonBytes(c.Ring)); err != nil {
			return model.BoundaryVersion{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return model.BoundaryVersion{}, err
	}
	out := model.BoundaryVersion{
		VersionID: versionID, RegionCode: region, Status: "active", BoundarySHA: boundarySHA,
		EffectiveFrom: now, Note: in.Note, CountyCount: len(canonical), CreatedAt: now,
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: "spatial.boundary.upload", SubjectType: "boundary_version",
		SubjectID: versionID, RegionCode: region, Decision: "allow",
		Detail: map[string]any{
			"boundary_version_id": versionID, "boundary_sha256": boundarySHA,
			"county_count": len(canonical), "note": in.Note,
		},
	}); err != nil {
		return out, err
	}
	return out, nil
}

// InvalidateBoundary marks a catalog version invalid (e.g. geometry defect
// discovered after release). Summaries computed under an invalid boundary hold
// every segment pending with reason boundary_version_invalid instead of
// allocating area against geometry nobody trusts.
func (s *Service) InvalidateBoundary(ctx context.Context, region, versionID, actor, role string) (model.BoundaryVersion, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.BoundaryVersion{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	tx, err := pool.Begin(ctx)
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `
		update boundary_versions set status='invalid', updated_at=now()
		where version_id=$1 and region_code=$2 and status <> 'invalid'`, versionID, region)
	if err != nil {
		return model.BoundaryVersion{}, err
	}
	if tag.RowsAffected() == 0 {
		var exists int
		if _ = tx.QueryRow(ctx, `select count(*) from boundary_versions where version_id=$1`, versionID).Scan(&exists); exists == 0 {
			return model.BoundaryVersion{}, fmt.Errorf("%w: boundary version %s", ErrNotFound, versionID)
		}
	}
	// The policy row is deliberately retained: a summary computed against an
	// invalid boundary is produced with every segment held pending under
	// boundary_version_invalid instead of allocating area against geometry
	// nobody trusts. The steward publishes a replacement to resume allocation.
	if err := tx.Commit(ctx); err != nil {
		return model.BoundaryVersion{}, err
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: "spatial.boundary.invalidate", SubjectType: "boundary_version",
		SubjectID: versionID, RegionCode: region, Decision: "deny",
		Detail: map[string]any{"boundary_version_id": versionID},
	}); err != nil {
		return model.BoundaryVersion{}, err
	}
	return s.GetBoundary(ctx, region, versionID)
}

func (s *Service) GetBoundary(ctx context.Context, region, versionID string) (model.BoundaryVersion, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.BoundaryVersion{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	var out model.BoundaryVersion
	var supersededBy *string
	err := pool.QueryRow(ctx, `
		select version_id, region_code, status, boundary_sha256, effective_from, superseded_by,
		       coalesce(note,''), created_at,
		       (select count(*) from boundary_counties where version_id=boundary_versions.version_id)
		from boundary_versions where version_id=$1 and region_code=$2`, versionID, region).
		Scan(&out.VersionID, &out.RegionCode, &out.Status, &out.BoundarySHA, &out.EffectiveFrom,
			&supersededBy, &out.Note, &out.CreatedAt, &out.CountyCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("%w: boundary version %s", ErrNotFound, versionID)
	}
	if err != nil {
		return out, err
	}
	if supersededBy != nil {
		out.SupersededBy = *supersededBy
	}
	return out, nil
}

func (s *Service) ListBoundaries(ctx context.Context, region string) ([]model.BoundaryVersion, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return nil, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	rows, err := pool.Query(ctx, `
		select version_id, region_code, status, boundary_sha256, effective_from,
		       coalesce(superseded_by,''), coalesce(note,''), created_at,
		       (select count(*) from boundary_counties bc where bc.version_id=bv.version_id)
		from boundary_versions bv where region_code=$1 order by created_at desc`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.BoundaryVersion{}
	for rows.Next() {
		var b model.BoundaryVersion
		if err := rows.Scan(&b.VersionID, &b.RegionCode, &b.Status, &b.BoundarySHA, &b.EffectiveFrom,
			&b.SupersededBy, &b.Note, &b.CreatedAt, &b.CountyCount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CreateSpatialPolicy binds a boundary version, an allocation method and an
// output grain into the single policy that directory, cut/grid processing,
// outbound aggregation and audit delivery all reference.
func (s *Service) CreateSpatialPolicy(ctx context.Context, region string, in model.SpatialPolicyInput, actor, role string) (model.SpatialPolicy, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.SpatialPolicy{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if in.Method != "intersection" && in.Method != "raster" {
		return model.SpatialPolicy{}, fmt.Errorf("%w: method must be intersection or raster", ErrInvalid)
	}
	if in.OutputGrain != "county" && in.OutputGrain != "grid" {
		return model.SpatialPolicy{}, fmt.Errorf("%w: output_grain must be county or grid", ErrInvalid)
	}
	if in.Method == "intersection" && in.OutputGrain == "grid" {
		return model.SpatialPolicy{}, fmt.Errorf("%w: intersection method only supports county grain; use raster for grid output", ErrInvalid)
	}
	if in.Method == "raster" {
		if in.GridSizeM < 100 {
			return model.SpatialPolicy{}, fmt.Errorf("%w: raster method requires grid_size_m >= 100", ErrInvalid)
		}
	}
	if in.DefaultAccuracyM < 0 {
		return model.SpatialPolicy{}, fmt.Errorf("%w: default_accuracy_m must be non-negative", ErrInvalid)
	}
	boundary, err := s.GetBoundary(ctx, region, in.BoundaryVersionID)
	if err != nil {
		return model.SpatialPolicy{}, err
	}
	if boundary.Status != "active" {
		return model.SpatialPolicy{}, fmt.Errorf("%w: boundary version %s is %s; publish an active boundary first",
			ErrInvalid, in.BoundaryVersionID, boundary.Status)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return model.SpatialPolicy{}, err
	}
	defer tx.Rollback(ctx)
	policyID := randID("spp")
	now := s.now().UTC()
	if _, err := tx.Exec(ctx, `
		update spatial_policies set status='superseded' where region_code=$1 and status='active'`, region); err != nil {
		return model.SpatialPolicy{}, err
	}
	var gridSize any
	if in.GridSizeM > 0 {
		gridSize = in.GridSizeM
	}
	if _, err := tx.Exec(ctx, `
		insert into spatial_policies (policy_id, region_code, boundary_version_id, output_grain, method,
			grid_size_m, default_accuracy_m, status, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,'active',$8)`,
		policyID, region, in.BoundaryVersionID, in.OutputGrain, in.Method, gridSize,
		in.DefaultAccuracyM, now); err != nil {
		return model.SpatialPolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.SpatialPolicy{}, err
	}
	out := model.SpatialPolicy{
		PolicyID: policyID, RegionCode: region, BoundaryVersionID: in.BoundaryVersionID,
		OutputGrain: in.OutputGrain, Method: in.Method, GridSizeM: in.GridSizeM,
		DefaultAccuracyM: in.DefaultAccuracyM, Status: "active", CreatedAt: now,
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: "spatial.policy.create", SubjectType: "spatial_policy",
		SubjectID: policyID, RegionCode: region, Decision: "allow",
		Detail: map[string]any{
			"policy_id": policyID, "boundary_version_id": in.BoundaryVersionID,
			"method": in.Method, "output_grain": in.OutputGrain, "grid_size_m": in.GridSizeM,
			"default_accuracy_m": in.DefaultAccuracyM,
		},
	}); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Service) ListSpatialPolicies(ctx context.Context, region string) ([]model.SpatialPolicy, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return nil, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	rows, err := pool.Query(ctx, `
		select policy_id, region_code, boundary_version_id, output_grain, method,
		       coalesce(grid_size_m,0), default_accuracy_m, status, created_at
		from spatial_policies where region_code=$1 order by created_at desc`, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SpatialPolicy{}
	for rows.Next() {
		var p model.SpatialPolicy
		if err := rows.Scan(&p.PolicyID, &p.RegionCode, &p.BoundaryVersionID, &p.OutputGrain, &p.Method,
			&p.GridSizeM, &p.DefaultAccuracyM, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// activeSpatialPolicy returns the region's current policy, or the explicitly
// requested one when policyID is non-empty.
func (s *Service) loadSpatialPolicy(ctx context.Context, region, policyID string) (model.SpatialPolicy, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.SpatialPolicy{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	var p model.SpatialPolicy
	q := `select policy_id, region_code, boundary_version_id, output_grain, method,
	            coalesce(grid_size_m,0), default_accuracy_m, status, created_at
	       from spatial_policies where region_code=$1`
	args := []any{region}
	if policyID != "" {
		q += ` and policy_id=$2`
		args = append(args, policyID)
	} else {
		q += ` and status='active' order by created_at desc limit 1`
	}
	err := pool.QueryRow(ctx, q, args...).Scan(&p.PolicyID, &p.RegionCode, &p.BoundaryVersionID,
		&p.OutputGrain, &p.Method, &p.GridSizeM, &p.DefaultAccuracyM, &p.Status, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fmt.Errorf("%w: no %s spatial policy", ErrNotFound,
			map[bool]string{true: "requested", false: "active"}[policyID != ""])
	}
	if err != nil {
		return p, err
	}
	return p, nil
}

// BackfillWorkingWidth supplies a missing implement width after the fact. The
// segment itself is untouched; only the regional spatial metadata changes, and
// a subsequent summary moves the segment out of the pending bucket.
func (s *Service) BackfillWorkingWidth(ctx context.Context, region string, items []model.WidthBackfill, actor, role string) (int, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return 0, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if len(items) == 0 {
		return 0, fmt.Errorf("%w: at least one backfill item is required", ErrInvalid)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	updated := 0
	for _, it := range items {
		if it.DeviceSerial == "" || it.SegmentID == "" {
			return 0, fmt.Errorf("%w: device_serial and segment_id are required", ErrInvalid)
		}
		if it.WorkingWidthM <= 0 {
			return 0, fmt.Errorf("%w: working_width_m must be positive", ErrInvalid)
		}
		opID := fmt.Sprintf("%s:%s", it.DeviceSerial, it.SegmentID)
		tag, err := tx.Exec(ctx, `
			update operation_spatial_meta set working_width_m=$2, updated_at=now()
			where operation_id=$1`, opID, it.WorkingWidthM)
		if err != nil {
			return 0, err
		}
		if tag.RowsAffected() == 0 {
			return 0, fmt.Errorf("%w: segment %s/%s is not known in this region",
				ErrNotFound, it.DeviceSerial, it.SegmentID)
		}
		updated++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: "spatial.width.backfill", SubjectType: "region",
		SubjectID: region, RegionCode: region, Decision: "allow",
		Detail: map[string]any{"updated_segments": updated},
	}); err != nil {
		return updated, err
	}
	return updated, nil
}

// ListDedupLog returns the region's duplicate-reception records, the evidence
// that repeated fragments were deduped rather than double-counted.
func (s *Service) ListDedupLog(ctx context.Context, region string, limit int) ([]model.DedupLogRow, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return nil, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := pool.Query(ctx, `
		select device_serial, segment_id, batch_id, content_sha256, received_at
		from segment_dedup_log order by id desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.DedupLogRow{}
	for rows.Next() {
		var r model.DedupLogRow
		if err := rows.Scan(&r.DeviceSerial, &r.SegmentID, &r.BatchID, &r.ContentSHA256, &r.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

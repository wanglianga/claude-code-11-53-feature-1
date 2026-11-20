package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"agrisync/internal/geo"
	"agrisync/internal/model"
)

// ComputeSpatialSummary runs the whole in-region spatial pipeline:
//
//  1. load the bound policy (boundary version + method + approved grain),
//  2. read raw trajectories and per-point accuracy from the regional store,
//  3. clip (intersection) or rasterise every segment against the approved
//     boundaries, holding fuzzy / width-missing / invalid-boundary fragments
//     as pending instead of picking a side,
//  4. persist the full regional ledger plus a sanitised outbound artifact,
//  5. emit one spatial_summary outbox event — the only thing replication may
//     carry to the group.
//
// A boundary correction or a trajectory backfill is handled by recomputing:
// the new summary supersedes the previous one while the old row and its grain
// stay readable for historical group reports.
func (s *Service) ComputeSpatialSummary(ctx context.Context, region, policyID string, from, to time.Time, operationType, actor, role string) (model.SpatialSummary, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.SpatialSummary{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if !to.After(from) {
		return model.SpatialSummary{}, fmt.Errorf("%w: period_to must be after period_from", ErrInvalid)
	}
	if operationType != "" && operationType != "seeding" && operationType != "harvesting" && operationType != "fertilizing" {
		return model.SpatialSummary{}, fmt.Errorf("%w: unknown operation_type %q", ErrInvalid, operationType)
	}
	policy, err := s.loadSpatialPolicy(ctx, region, policyID)
	if err != nil {
		return model.SpatialSummary{}, err
	}
	boundary, err := s.GetBoundary(ctx, region, policy.BoundaryVersionID)
	if err != nil {
		return model.SpatialSummary{}, err
	}
	counties, projector, err := s.loadCounties(ctx, region, policy.BoundaryVersionID)
	if err != nil {
		return model.SpatialSummary{}, err
	}

	// Every segment recorded in the requested window.
	rows, err := pool.Query(ctx, `
		select e.id, e.device_serial, e.segment_id, e.operation_type, e.area_ha, e.duration_s,
		       e.points, coalesce(m.working_width_m,0), coalesce(m.accuracy_m,0),
		       coalesce(m.point_accuracy,'[]'::jsonb)
		from operation_events e
		left join operation_spatial_meta m
		  on m.device_serial = e.device_serial and m.segment_id = e.segment_id
		where e.started_at >= $1 and e.started_at < $2
		  and ($3 = '' or e.operation_type = $3)
		order by e.journal_seq`, from.UTC(), to.UTC(), operationType)
	if err != nil {
		return model.SpatialSummary{}, err
	}
	type segment struct {
		opID, device, segID, opType string
		declaredAreaHa              float64
		durationS                   int
		rawPoints                   []byte
		widthM                      float64
		defaultAccuracyM            float64
		pointAccuracy               []float64
	}
	segments := []segment{}
	for rows.Next() {
		var sg segment
		var pointsJSON []byte
		var pointAccJSON []byte
		if err := rows.Scan(&sg.opID, &sg.device, &sg.segID, &sg.opType, &sg.declaredAreaHa,
			&sg.durationS, &pointsJSON, &sg.widthM, &sg.defaultAccuracyM, &pointAccJSON); err != nil {
			rows.Close()
			return model.SpatialSummary{}, err
		}
		sg.rawPoints = pointsJSON
		if len(pointAccJSON) > 2 {
			_ = json.Unmarshal(pointAccJSON, &sg.pointAccuracy)
		}
		segments = append(segments, sg)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return model.SpatialSummary{}, err
	}

	summaryID := randID("sps")
	now := s.now().UTC()
	boundaryInvalid := boundary.Status == "invalid"

	type binKey struct {
		unitType, unitCode, county, opType string
	}
	type binAcc struct {
		segments      map[string]bool
		areaHa        float64
		candidateHa   float64
		durationHours float64
		lengthM       float64
		cellsTouched  map[string]bool
	}
	bins := map[binKey]*binAcc{}
	type pendKey struct {
		reason string
		opType string
	}
	type pendAcc struct {
		segments      map[string]bool
		areaHa        float64
		durationHours float64
	}
	pending := map[pendKey]*pendAcc{}
	ledger := []model.SpatialSegmentLedgerRow{}
	excluded := []model.ExcludedFragment{}
	declaredTotal, allocatedTotal, pendingTotal := 0.0, 0.0, 0.0

	addBin := func(k binKey, seg string, areaHa, durHours, lengthM float64, cell string) {
		b := bins[k]
		if b == nil {
			b = &binAcc{segments: map[string]bool{}, cellsTouched: map[string]bool{}}
			bins[k] = b
		}
		b.segments[seg] = true
		b.areaHa += areaHa
		b.durationHours += durHours
		b.lengthM += lengthM
		if cell != "" {
			b.cellsTouched[cell] = true
		}
	}
	addPending := func(reason, opType, seg string, areaHa, durHours float64) {
		k := pendKey{reason, opType}
		p := pending[k]
		if p == nil {
			p = &pendAcc{segments: map[string]bool{}}
			pending[k] = p
		}
		p.segments[seg] = true
		p.areaHa += areaHa
		p.durationHours += durHours
	}

	for _, sg := range segments {
		contentSHA := sha256Hex(sg.rawPoints)
		row := model.SpatialSegmentLedgerRow{
			OperationID: sg.opID, SegmentID: sg.segID, DeviceSerial: sg.device,
			BoundaryVersionID: policy.BoundaryVersionID, PolicyID: policy.PolicyID,
			Method: policy.Method, WorkingWidthM: sg.widthM, DeclaredAreaHa: sg.declaredAreaHa,
			DurationS: sg.durationS, ContentSHA256: contentSHA, Candidates: []string{},
		}
		declaredTotal += sg.declaredAreaHa
		hold := func(reason string, areaHa float64) {
			row.Status = "pending"
			row.PendingReason = reason
			row.PendingAreaHa = round4(areaHa)
			row.AllocatedAreaHa = 0
			addPending(reason, sg.opType, sg.opID, areaHa, float64(sg.durationS)/3600.0)
			pendingTotal += areaHa
			excluded = append(excluded, model.ExcludedFragment{
				OperationID: sg.opID, SegmentID: sg.segID, DeviceSerial: sg.device,
				Status: "pending", Reason: reason, AreaHa: round4(areaHa),
				WorkingWidthM: sg.widthM,
			})
		}

		var pts []model.GeoPoint
		if len(sg.rawPoints) > 0 {
			_ = json.Unmarshal(sg.rawPoints, &pts)
		}
		switch {
		case boundaryInvalid:
			hold("boundary_version_invalid", sg.declaredAreaHa)
		case len(pts) < 2:
			hold("no_trajectory", sg.declaredAreaHa)
		case sg.widthM <= 0:
			hold("working_width_missing", sg.declaredAreaHa)
		default:
			line := make([]geo.Point, len(pts))
			ll := make([]geo.LatLon, len(pts))
			acc := make([]float64, len(pts))
			for i, p := range pts {
				ll[i] = geo.LatLon{Lat: p.Lat, Lon: p.Lon}
				line[i] = projector.Project(ll[i])
				if i < len(sg.pointAccuracy) && sg.pointAccuracy[i] > 0 {
					acc[i] = sg.pointAccuracy[i]
				} else if sg.defaultAccuracyM > 0 {
					acc[i] = sg.defaultAccuracyM
				} else {
					acc[i] = policy.DefaultAccuracyM
				}
			}
			totalLen := 0.0
			for i := 0; i+1 < len(line); i++ {
				totalLen += geo.Distance(line[i], line[i+1])
			}
			row.TotalLengthM = round2(totalLen)
			if totalLen < 1e-6 {
				hold("degenerate_trajectory", sg.declaredAreaHa)
				ledger = append(ledger, row)
				continue
			}
			durShare := func(lengthM float64) float64 {
				return float64(sg.durationS) / 3600.0 * (lengthM / totalLen)
			}
			allocLen, ambigLen, outsideLen := 0.0, 0.0, 0.0
			allocArea, pendArea := 0.0, 0.0
			candidateAreaByCounty := map[string]float64{}

			if policy.Method == "intersection" {
				pieces := geo.ClipPolyline(line, acc, counties)
				for _, pc := range pieces {
					area := geo.SquareMToHa(pc.LengthM * sg.widthM)
					dur := durShare(pc.LengthM)
					if pc.Ambiguous {
						ambigLen += pc.LengthM
						pendArea += area
						addPending("uncertainty_crosses_boundary", sg.opType, sg.opID, area, dur)
						for _, c := range pc.Candidates {
							candidateAreaByCounty[c] += area
						}
						excluded = append(excluded, model.ExcludedFragment{
							OperationID: sg.opID, SegmentID: sg.segID, DeviceSerial: sg.device,
							Status: "pending", Reason: "uncertainty_crosses_boundary",
							LengthM: round2(pc.LengthM), AreaHa: round4(area),
							Candidates: append([]string(nil), pc.Candidates...), WorkingWidthM: sg.widthM,
						})
						row.Candidates = unionStrings(row.Candidates, pc.Candidates)
						continue
					}
					if pc.County == "" {
						outsideLen += pc.LengthM
						pendArea += area
						addPending("outside_boundary_catalog", sg.opType, sg.opID, area, dur)
						excluded = append(excluded, model.ExcludedFragment{
							OperationID: sg.opID, SegmentID: sg.segID, DeviceSerial: sg.device,
							Status: "excluded", Reason: "outside_boundary_catalog",
							LengthM: round2(pc.LengthM), AreaHa: round4(area), WorkingWidthM: sg.widthM,
						})
						continue
					}
					allocLen += pc.LengthM
					allocArea += area
					addBin(binKey{"county", pc.County, pc.County, sg.opType}, sg.opID, area, dur, pc.LengthM, "")
				}
			} else {
				cells := geo.RasterizeGrid(line, acc, counties, float64(policy.GridSizeM), policy.DefaultAccuracyM, geo.Point{})
				for _, cell := range cells {
					area := geo.SquareMToHa(cell.LengthM * sg.widthM)
					dur := durShare(cell.LengthM)
					cellCode := fmt.Sprintf("c%d_r%d_c%d", policy.GridSizeM, cell.Row, cell.Col)
					if cell.Ambiguous {
						ambigLen += cell.LengthM
						pendArea += area
						addPending("uncertainty_crosses_boundary", sg.opType, sg.opID, area, dur)
						for _, c := range cell.Candidates {
							candidateAreaByCounty[c] += area
						}
						excluded = append(excluded, model.ExcludedFragment{
							OperationID: sg.opID, SegmentID: sg.segID, DeviceSerial: sg.device,
							Status: "pending", Reason: "uncertainty_crosses_boundary",
							LengthM: round2(cell.LengthM), AreaHa: round4(area),
							Candidates: append([]string(nil), cell.Candidates...), WorkingWidthM: sg.widthM,
						})
						row.Candidates = unionStrings(row.Candidates, cell.Candidates)
						continue
					}
					if cell.County == "" {
						outsideLen += cell.LengthM
						pendArea += area
						addPending("outside_boundary_catalog", sg.opType, sg.opID, area, dur)
						excluded = append(excluded, model.ExcludedFragment{
							OperationID: sg.opID, SegmentID: sg.segID, DeviceSerial: sg.device,
							Status: "excluded", Reason: "outside_boundary_catalog",
							LengthM: round2(cell.LengthM), AreaHa: round4(area), WorkingWidthM: sg.widthM,
						})
						continue
					}
					allocLen += cell.LengthM
					allocArea += area
					if policy.OutputGrain == "grid" {
						addBin(binKey{"grid", cellCode, cell.County, sg.opType}, sg.opID, area, dur, cell.LengthM, cellCode)
					} else {
						addBin(binKey{"county", cell.County, cell.County, sg.opType}, sg.opID, area, dur, cell.LengthM, cellCode)
					}
				}
			}
			// Fold the conservative candidate envelope into county bins.
			for county, cArea := range candidateAreaByCounty {
				k := binKey{"county", county, county, sg.opType}
				b := bins[k]
				if b == nil {
					b = &binAcc{segments: map[string]bool{}, cellsTouched: map[string]bool{}}
					bins[k] = b
				}
				b.candidateHa += cArea
			}
			row.Status = "allocated"
			if pendArea > 0 {
				row.Status = "pending"
				row.PendingReason = "partially_ambiguous"
			}
			row.AllocatedLengthM = round2(allocLen)
			row.AmbiguousLengthM = round2(ambigLen + outsideLen)
			row.AllocatedAreaHa = round4(allocArea)
			row.PendingAreaHa = round4(pendArea)
			allocatedTotal += allocArea
			pendingTotal += pendArea
		}
		ledger = append(ledger, row)
	}

	// Dedupe evidence: repeated receptions of the segments in this window.
	segIDs := make([]any, 0, len(segments))
	for _, sg := range segments {
		segIDs = append(segIDs, sg.segID)
	}
	duplicateCount := 0
	if len(segIDs) > 0 {
		if err := pool.QueryRow(ctx, `
			select count(*) from segment_dedup_log where segment_id = any($1)`, segIDs).Scan(&duplicateCount); err != nil {
			return model.SpatialSummary{}, err
		}
	}

	// Build the outbound artifact from the accumulators.
	binList := make([]model.SpatialBin, 0, len(bins))
	keys := make([]binKey, 0, len(bins))
	for k := range bins {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.opType != b.opType {
			return a.opType < b.opType
		}
		if a.unitType != b.unitType {
			return a.unitType < b.unitType
		}
		return a.unitCode < b.unitCode
	})
	for _, k := range keys {
		b := bins[k]
		binList = append(binList, model.SpatialBin{
			UnitType: k.unitType, UnitCode: k.unitCode, CountyCode: k.county,
			OperationType: k.opType, SegmentCount: len(b.segments),
			AreaHa: round4(b.areaHa), AreaHaMax: round4(b.areaHa + b.candidateHa),
			UncertaintyHa: round4(b.candidateHa), DurationHours: round4(b.durationHours),
			CoveredLengthM: round2(b.lengthM), GridCellsTouched: len(b.cellsTouched),
		})
	}
	pendList := make([]model.PendingByReason, 0, len(pending))
	pk := make([]pendKey, 0, len(pending))
	for k := range pending {
		pk = append(pk, k)
	}
	sort.Slice(pk, func(i, j int) bool {
		if pk[i].reason != pk[j].reason {
			return pk[i].reason < pk[j].reason
		}
		return pk[i].opType < pk[j].opType
	})
	for _, k := range pk {
		p := pending[k]
		pendList = append(pendList, model.PendingByReason{
			Reason: k.reason + "|" + k.opType, AreaHa: round4(p.areaHa),
			DurationHours: round4(p.durationHours), SegmentCount: len(p.segments),
		})
	}

	opLabel := operationType
	if opLabel == "" {
		opLabel = "all"
	}
	artifact := model.SpatialArtifact{
		Schema: "agrisync.spatial_summary/v1", SummaryID: summaryID, SourceRegion: region,
		PolicyID: policy.PolicyID, BoundaryVersionID: policy.BoundaryVersionID,
		BoundarySHA256: boundary.BoundarySHA, Method: policy.Method, OutputGrain: policy.OutputGrain,
		GridSizeM: policy.GridSizeM, PeriodFrom: from.UTC(), PeriodTo: to.UTC(),
		OperationType: opLabel, GeneratedAt: now, Bins: binList, Pending: pendList,
		PendingTotalHa: round4(pendingTotal), AllocatedTotalHa: round4(allocatedTotal),
		DeclaredTotalHa: round4(declaredTotal), SourceSegmentCount: len(segments),
		DuplicateCount: duplicateCount, ContainsRawPoints: false,
		Notes: []string{
			"Computed entirely inside the region from raw trajectories that never leave it.",
			"Bins are at the approved grain only; no ordered points or finer cells are included.",
			"Pending area is unallocated by rule (uncertainty, missing width, invalid boundary or outside catalog), never assigned by convenience.",
		},
	}
	if hits := scanForRestrictedFields(artifact, ""); len(hits) > 0 {
		return model.SpatialSummary{}, fmt.Errorf("internal: outbound artifact contains restricted paths %v", hits)
	}
	artifactJSON := jsonBytes(artifact)
	artifactSHA := sha256Hex(artifactJSON)

	// The previous summary at the same reporting scope (region, period,
	// operation type and output grain) is superseded, not deleted. A policy
	// change such as a boundary correction therefore produces a fresh, linked
	// summary while the old row and its grain stay readable for historical
	// group reports. A different grain (county vs grid) keeps its own lineage.
	var supersedes string
	_ = pool.QueryRow(ctx, `
		select summary_id from spatial_summaries
		where region_code=$1 and period_from=$2 and period_to=$3
		  and operation_type=$4 and output_grain=$5 and status <> 'superseded'
		order by created_at desc limit 1`,
		region, from.UTC(), to.UTC(), opLabel, policy.OutputGrain).Scan(&supersedes)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return model.SpatialSummary{}, err
	}
	defer tx.Rollback(ctx)
	var node string
	var epoch, nextOutbox int64
	if err := tx.QueryRow(ctx, `
		select active_node, local_epoch, next_outbox_seq from region_state where id for update`).
		Scan(&node, &epoch, &nextOutbox); err != nil {
		return model.SpatialSummary{}, err
	}
	if supersedes != "" {
		if _, err := tx.Exec(ctx, `
			update spatial_summaries set status='superseded' where summary_id=$1`, supersedes); err != nil {
			return model.SpatialSummary{}, err
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into spatial_summaries (summary_id, region_code, policy_id, boundary_version_id, boundary_sha256,
			method, output_grain, grid_size_m, period_from, period_to, operation_type, status,
			supersedes_summary_id, data_cutoff, bins, pending, excluded_fragments, source_segment_count,
			duplicate_count, artifact, artifact_sha256, outbox_seq, source_node, source_epoch, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'computed',$12,now(),$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		summaryID, region, policy.PolicyID, policy.BoundaryVersionID, boundary.BoundarySHA, policy.Method,
		policy.OutputGrain, nullableInt(policy.GridSizeM), from.UTC(), to.UTC(), opLabel, nullStr(supersedes),
		jsonBytes(binList), jsonBytes(pendList), jsonBytes(excluded), len(segments), duplicateCount,
		string(artifactJSON), artifactSHA, nextOutbox, node, epoch, now); err != nil {
		return model.SpatialSummary{}, err
	}
	for _, lr := range ledger {
		if _, err := tx.Exec(ctx, `
			insert into spatial_segment_ledger (id, summary_id, operation_id, device_serial, segment_id,
				status, pending_reason, boundary_version_id, policy_id, method, working_width_m,
				declared_area_ha, total_length_m, allocated_length_m, ambiguous_length_m,
				allocated_area_ha, pending_area_ha, duration_s, candidates, content_sha256, created_at)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
			summaryID+":"+lr.OperationID, summaryID, lr.OperationID, lr.DeviceSerial, lr.SegmentID,
			lr.Status, nullStr(lr.PendingReason), lr.BoundaryVersionID, lr.PolicyID, lr.Method,
			nullableFloat(lr.WorkingWidthM), lr.DeclaredAreaHa, lr.TotalLengthM, lr.AllocatedLengthM,
			lr.AmbiguousLengthM, lr.AllocatedAreaHa, lr.PendingAreaHa, lr.DurationS,
			jsonBytes(lr.Candidates), lr.ContentSHA256, now); err != nil {
			return model.SpatialSummary{}, err
		}
	}
	payload := map[string]any{
		"summary_id": summaryID, "policy_id": policy.PolicyID,
		"boundary_version_id": policy.BoundaryVersionID, "boundary_sha256": boundary.BoundarySHA,
		"method": policy.Method, "output_grain": policy.OutputGrain, "grid_size_m": policy.GridSizeM,
		"period_from": from.UTC(), "period_to": to.UTC(), "operation_type": opLabel,
		"artifact": string(artifactJSON), "artifact_sha256": artifactSHA,
	}
	if err := insertOutbox(ctx, tx, nextOutbox, "spatial_summary", summaryID, "spatial_summary",
		payload, node, epoch); err != nil {
		return model.SpatialSummary{}, err
	}
	if _, err := tx.Exec(ctx, `update region_state set next_outbox_seq=$1, updated_at=now() where id`,
		nextOutbox+1); err != nil {
		return model.SpatialSummary{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.SpatialSummary{}, err
	}

	out := model.SpatialSummary{
		SummaryID: summaryID, RegionCode: region, PolicyID: policy.PolicyID,
		BoundaryVersionID: policy.BoundaryVersionID, BoundarySHA256: boundary.BoundarySHA,
		Method: policy.Method, OutputGrain: policy.OutputGrain, GridSizeM: policy.GridSizeM,
		PeriodFrom: from.UTC(), PeriodTo: to.UTC(), OperationType: opLabel, Status: "computed",
		SupersedesSummary: supersedes, DataCutoff: now, Bins: binList, Pending: pendList,
		ExcludedFragments: excluded, SourceSegmentCount: len(segments), DuplicateCount: duplicateCount,
		ArtifactSHA256: artifactSHA, OutboxSeq: nextOutbox, SourceNode: node, CreatedAt: now,
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: "spatial.summary.compute", SubjectType: "spatial_summary",
		SubjectID: summaryID, RegionCode: region, Decision: "allow",
		Detail: map[string]any{
			"summary_id": summaryID, "policy_id": policy.PolicyID,
			"boundary_version_id": policy.BoundaryVersionID, "method": policy.Method,
			"output_grain": policy.OutputGrain, "segments": len(segments),
			"duplicates": duplicateCount, "allocated_ha": round4(allocatedTotal),
			"pending_ha": round4(pendingTotal), "supersedes": supersedes,
			"artifact_sha256": artifactSHA,
		},
	}); err != nil {
		return out, err
	}
	return out, nil
}

// loadCounties reads one boundary catalog and builds projected polygons using
// a projector anchored at the catalog's own geographic center, so rings and
// trajectories share one metric frame.
func (s *Service) loadCounties(ctx context.Context, region, versionID string) ([]geo.County, *geo.Projector, error) {
	pool := s.db.Region(region)
	rows, err := pool.Query(ctx, `
		select county_code, ring from boundary_counties where version_id=$1 order by county_code`, versionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	type raw struct {
		code string
		ring [][]float64
	}
	raws := []raw{}
	allLL := []geo.LatLon{}
	for rows.Next() {
		var r raw
		var ringJSON []byte
		if err := rows.Scan(&r.code, &ringJSON); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal(ringJSON, &r.ring); err != nil {
			return nil, nil, err
		}
		raws = append(raws, r)
		for _, ll := range r.ring {
			allLL = append(allLL, geo.LatLon{Lat: ll[0], Lon: ll[1]})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(raws) == 0 {
		return nil, nil, fmt.Errorf("%w: boundary version %s has no county rings", ErrInvalid, versionID)
	}
	projector := geo.NewProjector(geo.BoundsCenter(allLL))
	counties := make([]geo.County, 0, len(raws))
	for _, r := range raws {
		ring := make(geo.Ring, len(r.ring))
		for i, ll := range r.ring {
			ring[i] = projector.Project(geo.LatLon{Lat: ll[0], Lon: ll[1]})
		}
		counties = append(counties, geo.County{Code: r.code, Ring: ring})
	}
	return counties, projector, nil
}

func unionStrings(a, b []string) []string {
	for _, v := range b {
		if !containsString(a, v) {
			a = append(a, v)
		}
	}
	sort.Strings(a)
	return a
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}

func round4(v float64) float64 {
	return float64(int64(v*10000+0.5)) / 10000
}

// --- regional reads -----------------------------------------------------

func (s *Service) GetSpatialSummary(ctx context.Context, region, summaryID string) (model.SpatialSummary, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.SpatialSummary{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	var out model.SpatialSummary
	var bins, pend, excluded []byte
	var grid *int
	var supersedes *string
	var outboxSeq *int64
	err := pool.QueryRow(ctx, `
		select summary_id, region_code, policy_id, boundary_version_id, boundary_sha256, method,
		       output_grain, grid_size_m, period_from, period_to, operation_type, status,
		       supersedes_summary_id, data_cutoff, bins, pending, excluded_fragments,
		       source_segment_count, duplicate_count, artifact_sha256, outbox_seq, source_node, created_at
		from spatial_summaries where summary_id=$1 and region_code=$2`, summaryID, region).
		Scan(&out.SummaryID, &out.RegionCode, &out.PolicyID, &out.BoundaryVersionID, &out.BoundarySHA256,
			&out.Method, &out.OutputGrain, &grid, &out.PeriodFrom, &out.PeriodTo, &out.OperationType,
			&out.Status, &supersedes, &out.DataCutoff, &bins, &pend, &excluded, &out.SourceSegmentCount,
			&out.DuplicateCount, &out.ArtifactSHA256, &outboxSeq, &out.SourceNode, &out.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, fmt.Errorf("%w: spatial summary %s", ErrNotFound, summaryID)
	}
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal(bins, &out.Bins)
	_ = json.Unmarshal(pend, &out.Pending)
	_ = json.Unmarshal(excluded, &out.ExcludedFragments)
	if grid != nil {
		out.GridSizeM = *grid
	}
	if supersedes != nil {
		out.SupersedesSummary = *supersedes
	}
	if outboxSeq != nil {
		out.OutboxSeq = *outboxSeq
	}
	return out, nil
}

func (s *Service) ListSpatialSummaries(ctx context.Context, region string, limit int) ([]model.SpatialSummary, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return nil, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := pool.Query(ctx, `
		select summary_id from spatial_summaries where region_code=$1 order by created_at desc limit $1`,
		region, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []model.SpatialSummary{}
	for _, id := range ids {
		sum, err := s.GetSpatialSummary(ctx, region, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sum)
	}
	return out, nil
}

func (s *Service) GetSpatialLedger(ctx context.Context, region, summaryID string) ([]model.SpatialSegmentLedgerRow, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return nil, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	rows, err := pool.Query(ctx, `
		select operation_id, segment_id, device_serial, status, coalesce(pending_reason,''),
		       boundary_version_id, policy_id, method, coalesce(working_width_m,0), declared_area_ha,
		       total_length_m, allocated_length_m, ambiguous_length_m, allocated_area_ha,
		       pending_area_ha, duration_s, candidates, content_sha256
		from spatial_segment_ledger where summary_id=$1 order by operation_id`, summaryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SpatialSegmentLedgerRow{}
	for rows.Next() {
		var r model.SpatialSegmentLedgerRow
		var cand []byte
		if err := rows.Scan(&r.OperationID, &r.SegmentID, &r.DeviceSerial, &r.Status, &r.PendingReason,
			&r.BoundaryVersionID, &r.PolicyID, &r.Method, &r.WorkingWidthM, &r.DeclaredAreaHa,
			&r.TotalLengthM, &r.AllocatedLengthM, &r.AmbiguousLengthM, &r.AllocatedAreaHa,
			&r.PendingAreaHa, &r.DurationS, &cand, &r.ContentSHA256); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cand, &r.Candidates)
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- group-side reads ---------------------------------------------------

// ListGroupSpatialSummaries returns the confirmed, sanitised spatial
// replicas visible to the group. It only parses the outbound artifact; the
// regional ledger with excluded fragments is not reachable from this side.
func (s *Service) ListGroupSpatialSummaries(ctx context.Context, region string) ([]model.SpatialArtifact, error) {
	q := `select source_region, summary_id, artifact from replica_spatial_summaries where status='confirmed'`
	args := []any{}
	if region != "" {
		q += ` and source_region=$1`
		args = append(args, region)
	}
	q += ` order by copied_at desc limit 200`
	rows, err := s.db.Control().Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SpatialArtifact{}
	for rows.Next() {
		var src, id, artifactText string
		if err := rows.Scan(&src, &id, &artifactText); err != nil {
			return nil, err
		}
		var art model.SpatialArtifact
		if err := json.Unmarshal([]byte(artifactText), &art); err != nil {
			return nil, err
		}
		out = append(out, art)
	}
	return out, rows.Err()
}

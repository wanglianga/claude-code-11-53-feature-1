package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"agrisync/internal/model"
)

const spatialMetricKey = "spatial_area_ha"

// DeliverSpatialSummary is the group-side gate for one regional spatial
// summary. As with the scalar delivery, every authorization fact is re-read
// against the exact artifact bytes immediately before sending:
//   - the approval is still approved, current, valid and covers the region,
//   - the residency policy still allows the spatial_summary category,
//   - no raw trajectory of a restricted region sits in the group store,
//   - the artifact contains no ordered points, coordinates or fine cells.
func (s *Service) DeliverSpatialSummary(ctx context.Context, in model.SpatialDeliveryInput, actor, role string) (model.SpatialDelivery, error) {
	if in.SourceRegion == "" || in.SummaryID == "" || in.ApprovalID == "" {
		return model.SpatialDelivery{}, fmt.Errorf("%w: source_region, summary_id and approval_id are required", ErrInvalid)
	}
	approval, err := s.GetApproval(ctx, in.ApprovalID)
	if err != nil {
		return model.SpatialDelivery{}, err
	}
	var artifactText, artifactSHA, sourceNode, summaryCheck string
	var policyID, boundaryVersionID, boundarySHA, method, grain string
	var gridSize *int
	var status string
	err = s.db.Control().QueryRow(ctx, `
		select summary_id, policy_id, boundary_version_id, boundary_sha256, method, output_grain,
		       grid_size_m, artifact, artifact_sha256, source_node, status
		from replica_spatial_summaries
		where source_region=$1 and summary_id=$2 and status='confirmed'`,
		in.SourceRegion, in.SummaryID).
		Scan(&summaryCheck, &policyID, &boundaryVersionID, &boundarySHA, &method, &grain,
			&gridSize, &artifactText, &artifactSHA, &sourceNode, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.SpatialDelivery{}, fmt.Errorf("%w: confirmed spatial summary %s/%s not replicated yet",
			ErrNotFound, in.SourceRegion, in.SummaryID)
	}
	if err != nil {
		return model.SpatialDelivery{}, err
	}
	var artifact model.SpatialArtifact
	if err := json.Unmarshal([]byte(artifactText), &artifact); err != nil {
		return model.SpatialDelivery{}, err
	}

	now := s.now().UTC()
	d := model.SpatialDelivery{
		ID: randID("sdl"), SourceRegion: in.SourceRegion, SummaryID: in.SummaryID,
		ApprovalID: approval.ID, Channel: s.cfg.DeliveryChannel, RequestedBy: actor,
		CreatedAt: now, Status: "blocked", Checks: []model.Check{},
	}
	addCheck := func(name string, passed bool, detail string) {
		d.Checks = append(d.Checks, model.Check{Name: name, Passed: passed, Detail: detail})
	}

	addCheck("approval_status", approval.Status == "approved", "approval status is "+approval.Status)
	inValidity := true
	if approval.ValidFrom != nil && now.Before(*approval.ValidFrom) {
		inValidity = false
	}
	if approval.ValidUntil != nil && now.After(*approval.ValidUntil) {
		inValidity = false
	}
	addCheck("approval_validity_window", inValidity, "checked against current approval validity window")
	regionCovered := containsString(approval.ScopeRegions, in.SourceRegion)
	addCheck("region_scope_current", regionCovered,
		fmt.Sprintf("region %s vs approval scope %s", in.SourceRegion, strings.Join(approval.ScopeRegions, ",")))
	metricCovered := containsString(approval.ScopeMetrics, spatialMetricKey)
	addCheck("metric_scope_current", metricCovered,
		"approval scope must include "+spatialMetricKey)

	// Residency: the spatial_summary category must still be egress-allowed,
	// and a restricted region's raw points must never have reached the group.
	var spatialAllowed, rawAllowed bool
	if err := s.db.Control().QueryRow(ctx, `
		select coalesce(bool_or(case when category='spatial_summary' then outbound_allowed end),false),
		       coalesce(bool_or(case when category='raw_trajectory' then outbound_allowed end),false)
		from residency_policies where region_code=$1`, in.SourceRegion).
		Scan(&spatialAllowed, &rawAllowed); err != nil {
		return d, err
	}
	addCheck("spatial_summary_egress_policy", spatialAllowed,
		"residency policy for spatial_summary in "+in.SourceRegion)
	var rawRows int
	if err := s.db.Control().QueryRow(ctx, `
		select count(*) from replica_trajectories where source_region=$1`, in.SourceRegion).Scan(&rawRows); err != nil {
		return d, err
	}
	addCheck("restricted_raw_never_in_group_store", rawAllowed || rawRows == 0,
		fmt.Sprintf("raw trajectory replicas for %s: %d", in.SourceRegion, rawRows))

	forbidden := scanForRestrictedFields(artifact, "")
	addCheck("artifact_has_no_coordinates", len(forbidden) == 0,
		"restricted field paths found in artifact: "+strings.Join(forbidden, ","))
	addCheck("artifact_grain_approved", artifact.OutputGrain == grain &&
		(grain != "grid" || (gridSize != nil && artifact.GridSizeM == *gridSize && *gridSize >= 100)),
		fmt.Sprintf("artifact grain=%s size=%dm against policy grain=%s", artifact.OutputGrain, artifact.GridSizeM, grain))
	addCheck("artifact_contains_only_aggregates", !artifact.ContainsRawPoints,
		"artifact declares contains_raw_trajectories=false")
	if recomputed := sha256Hex([]byte(artifactText)); recomputed != artifactSHA {
		addCheck("artifact_hash_intact", false, "recomputed "+recomputed+" vs recorded "+artifactSHA)
	} else {
		addCheck("artifact_hash_intact", true, artifactSHA)
	}

	allPassed := true
	for _, c := range d.Checks {
		if !c.Passed {
			allPassed = false
			if d.BlockedReason == "" {
				d.BlockedReason = c.Name + ": " + c.Detail
			}
		}
	}

	tx, err := s.db.Control().Begin(ctx)
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx)
	var storedArtifact any
	if allPassed {
		d.Status = "sent"
		sentAt := now
		d.SentAt = &sentAt
		d.ArtifactSHA = artifactSHA
		d.Artifact = artifact
		storedArtifact = artifactText
	}
	if _, err := tx.Exec(ctx, `
		insert into spatial_deliveries (id, source_region, summary_id, approval_id, status, channel,
			checks, blocked_reason, artifact, artifact_sha256, requested_by, created_at, sent_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		d.ID, d.SourceRegion, d.SummaryID, d.ApprovalID, d.Status, d.Channel, jsonBytes(d.Checks),
		nullStr(d.BlockedReason), storedArtifact, nullStr(d.ArtifactSHA), d.RequestedBy, d.CreatedAt, d.SentAt); err != nil {
		return d, err
	}
	if err := tx.Commit(ctx); err != nil {
		return d, err
	}
	decision, action := "allow", "spatial.delivery.sent"
	if !allPassed {
		decision, action = "deny", "spatial.delivery.blocked"
	}
	auditSeq, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: action, SubjectType: "spatial_delivery", SubjectID: d.ID,
		RegionCode: d.SourceRegion, Decision: decision,
		Detail: map[string]any{
			"source_region": d.SourceRegion, "summary_id": d.SummaryID,
			"approval_id": approval.ID, "boundary_version_id": boundaryVersionID,
			"boundary_sha256": boundarySHA, "method": method, "output_grain": grain,
			"artifact_sha256": d.ArtifactSHA, "blocked_reason": d.BlockedReason, "checks": d.Checks,
		},
	})
	if err != nil {
		return d, err
	}
	d.AuditSeq = auditSeq
	_, _ = s.db.Control().Exec(ctx, `update spatial_deliveries set audit_seq=$2 where id=$1`, d.ID, auditSeq)
	return d, nil
}

// VerifySpatialDelivery re-hashes the stored bytes, rescans restricted fields
// and checks the audit entry plus the whole ledger chain.
func (s *Service) VerifySpatialDelivery(ctx context.Context, deliveryID string) (model.SpatialArtifactVerification, error) {
	var d model.SpatialDelivery
	var checks, artifact []byte
	err := s.db.Control().QueryRow(ctx, `
		select id, source_region, summary_id, approval_id, status, channel, checks,
		       coalesce(blocked_reason,''), artifact, coalesce(artifact_sha256,''),
		       coalesce(audit_seq,0), requested_by, created_at, sent_at
		from spatial_deliveries where id=$1`, deliveryID).
		Scan(&d.ID, &d.SourceRegion, &d.SummaryID, &d.ApprovalID, &d.Status, &d.Channel, &checks,
			&d.BlockedReason, &artifact, &d.ArtifactSHA, &d.AuditSeq, &d.RequestedBy, &d.CreatedAt, &d.SentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.SpatialArtifactVerification{}, fmt.Errorf("%w: spatial delivery %s", ErrNotFound, deliveryID)
	}
	if err != nil {
		return model.SpatialArtifactVerification{}, err
	}
	_ = json.Unmarshal(checks, &d.Checks)
	out := model.SpatialArtifactVerification{
		DeliveryID: d.ID, Status: d.Status, RecordedSHA: d.ArtifactSHA,
		AuditSeq: d.AuditSeq, VerifiedAt: s.now().UTC(), ArtifactSize: len(artifact),
	}
	if len(artifact) == 0 || string(artifact) == "null" {
		out.SHA256Matches = d.Status != "sent"
	} else {
		out.RecomputedSHA = sha256Hex(artifact)
		out.SHA256Matches = out.RecomputedSHA == d.ArtifactSHA
		var generic any
		if err := json.Unmarshal(artifact, &generic); err != nil {
			return out, err
		}
		out.RestrictedHits = scanForRestrictedFields(generic, "")
		if len(out.RestrictedHits) > 0 {
			out.SHA256Matches = false
		}
	}
	events, err := s.AuditFor(ctx, d.ID)
	if err != nil {
		return out, err
	}
	for _, e := range events {
		if e.SubjectID != d.ID {
			continue
		}
		if e.Action != "spatial.delivery.sent" && e.Action != "spatial.delivery.blocked" {
			continue
		}
		if v, ok := e.Detail["artifact_sha256"].(string); ok && v == d.ArtifactSHA {
			out.AuditValid = true
		}
	}
	chain, err := s.VerifyAudit(ctx)
	if err != nil {
		return out, err
	}
	out.AuditVerified = chain.Valid
	out.ChainHead = chain.Head
	return out, nil
}

// CountSpatialDeliveries backs the demo summary endpoint.
func (s *Service) CountSpatialDeliveries(ctx context.Context) (int, error) {
	var n int
	if err := s.db.Control().QueryRow(ctx, `select count(*) from spatial_deliveries`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ListSpatialDeliveries is the group-side history of spatial egress decisions.
func (s *Service) ListSpatialDeliveries(ctx context.Context, limit int) ([]model.SpatialDelivery, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Control().Query(ctx, `
		select id, source_region, summary_id, approval_id, status, channel, checks,
		       coalesce(blocked_reason,''), artifact, coalesce(artifact_sha256,''),
		       coalesce(audit_seq,0), requested_by, created_at, sent_at
		from spatial_deliveries order by created_at desc limit $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SpatialDelivery{}
	for rows.Next() {
		var d model.SpatialDelivery
		var checks, artifact []byte
		if err := rows.Scan(&d.ID, &d.SourceRegion, &d.SummaryID, &d.ApprovalID, &d.Status, &d.Channel,
			&checks, &d.BlockedReason, &artifact, &d.ArtifactSHA, &d.AuditSeq, &d.RequestedBy,
			&d.CreatedAt, &d.SentAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(checks, &d.Checks)
		if len(artifact) > 0 && string(artifact) != "null" {
			_ = json.Unmarshal(artifact, &d.Artifact)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

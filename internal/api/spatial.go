package api

import (
	"net/http"
	"time"

	"agrisync/internal/model"
	"agrisync/internal/service"
)

// Spatial HTTP surface.
//
// Regional routes stay under /regions/{region}/spatial/... and inherit the
// region access gate: only the region's own operator, an auditor or the
// platform admin can reach them. The group analyst has no path here, so the
// excluded-fragment ledger and boundary rings can never be read group-side.
//
// Group routes under /spatial/... expose only the confirmed sanitised
// replicas and the delivery gate.

func (s *Server) handleSpatialBoundaries(w http.ResponseWriter, r *http.Request) {
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	if !s.regionKnown(region) {
		s.fail(w, service.ErrNotFound)
		return
	}
	p := principalFrom(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.svc.ListBoundaries(r.Context(), region)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"region": region, "boundary_versions": list})
	case http.MethodPost:
		if !p.inAny("region_operator") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var in model.BoundaryUpload
		if err := decodeJSON(r, &in); err != nil {
			s.fail(w, err)
			return
		}
		v, err := s.svc.UploadBoundary(r.Context(), region, in, p.Actor, p.Role)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 201, v)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

func (s *Server) handleSpatialBoundaryInvalidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only", nil)
		return
	}
	region := pathValue(r, "region")
	p := principalFrom(r.Context())
	if !p.inAny("region_operator") {
		s.fail(w, service.ErrForbidden)
		return
	}
	if err := s.requireRegion(p, region); err != nil {
		s.fail(w, err)
		return
	}
	var req struct {
		VersionID string `json:"version_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	v, err := s.svc.InvalidateBoundary(r.Context(), region, req.VersionID, p.Actor, p.Role)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) handleSpatialPolicies(w http.ResponseWriter, r *http.Request) {
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	if !s.regionKnown(region) {
		s.fail(w, service.ErrNotFound)
		return
	}
	p := principalFrom(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.svc.ListSpatialPolicies(r.Context(), region)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"region": region, "policies": list})
	case http.MethodPost:
		if !p.inAny("region_operator") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var in model.SpatialPolicyInput
		if err := decodeJSON(r, &in); err != nil {
			s.fail(w, err)
			return
		}
		v, err := s.svc.CreateSpatialPolicy(r.Context(), region, in, p.Actor, p.Role)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 201, v)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

type spatialSummaryRequest struct {
	PolicyID      string    `json:"policy_id"`
	PeriodFrom    time.Time `json:"period_from"`
	PeriodTo      time.Time `json:"period_to"`
	OperationType string    `json:"operation_type"`
}

func (s *Server) handleRegionSpatialSummaries(w http.ResponseWriter, r *http.Request) {
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	if !s.regionKnown(region) {
		s.fail(w, service.ErrNotFound)
		return
	}
	p := principalFrom(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.svc.ListSpatialSummaries(r.Context(), region, queryInt(r, "limit", 50))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"region": region, "summaries": list})
	case http.MethodPost:
		if !p.inAny("region_operator") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var req spatialSummaryRequest
		if err := decodeJSON(r, &req); err != nil {
			s.fail(w, err)
			return
		}
		sum, err := s.svc.ComputeSpatialSummary(r.Context(), region, req.PolicyID,
			req.PeriodFrom, req.PeriodTo, req.OperationType, p.Actor, p.Role)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 201, sum)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

func (s *Server) handleRegionSpatialSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	sum, err := s.svc.GetSpatialSummary(r.Context(), region, pathValue(r, "sid"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, sum)
}

func (s *Server) handleRegionSpatialLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.svc.GetSpatialLedger(r.Context(), region, pathValue(r, "sid"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"region": region, "summary_id": pathValue(r, "sid"), "ledger": rows})
}

func (s *Server) handleSpatialWidthBackfill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only", nil)
		return
	}
	region := pathValue(r, "region")
	p := principalFrom(r.Context())
	if !p.inAny("region_operator") {
		s.fail(w, service.ErrForbidden)
		return
	}
	if err := s.requireRegion(p, region); err != nil {
		s.fail(w, err)
		return
	}
	var req struct {
		Items []model.WidthBackfill `json:"items"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	n, err := s.svc.BackfillWorkingWidth(r.Context(), region, req.Items, p.Actor, p.Role)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"region": region, "updated_segments": n})
}

func (s *Server) handleSpatialDedupLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.svc.ListDedupLog(r.Context(), region, queryInt(r, "limit", 100))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"region": region, "dedup_log": rows})
}

// --- group side ---------------------------------------------------------

func (s *Server) handleGroupSpatialSummaries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	list, err := s.svc.ListGroupSpatialSummaries(r.Context(), r.URL.Query().Get("region"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"spatial_summaries": list})
}

func (s *Server) handleGroupSpatialDeliveries(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.svc.ListSpatialDeliveries(r.Context(), queryInt(r, "limit", 50))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"deliveries": list})
	case http.MethodPost:
		if !p.inAny("group_analyst") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var in model.SpatialDeliveryInput
		if err := decodeJSON(r, &in); err != nil {
			s.fail(w, err)
			return
		}
		d, err := s.svc.DeliverSpatialSummary(r.Context(), in, p.Actor, p.Role)
		if err != nil {
			s.fail(w, err)
			return
		}
		code := http.StatusOK
		if d.Status != "sent" {
			code = http.StatusConflict
		}
		writeJSON(w, code, d)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

func (s *Server) handleGroupSpatialDeliveryVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	v, err := s.svc.VerifySpatialDelivery(r.Context(), pathValue(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	code := http.StatusOK
	if v.Status == "sent" && (!v.SHA256Matches || len(v.RestrictedHits) > 0 || !v.AuditValid || !v.AuditVerified) {
		code = http.StatusConflict
	}
	writeJSON(w, code, v)
}

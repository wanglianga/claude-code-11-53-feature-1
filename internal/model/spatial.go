package model

import "time"

// Spatial types. The split between regional and group-visible structs mirrors
// the database boundary: the regional structs can name boundary rings and
// excluded fragments; the group artifact only carries approved-grain bins.

// CountyRingInput is one administrative polygon in a boundary catalog upload.
type CountyRingInput struct {
	CountyCode string      `json:"county_code"`
	CountyName string      `json:"county_name"`
	Ring       [][]float64 `json:"ring"` // [[lat, lon], ...], closed implicitly
}

type BoundaryUpload struct {
	Note     string            `json:"note"`
	Counties []CountyRingInput `json:"counties"`
}

type BoundaryVersion struct {
	VersionID     string    `json:"version_id"`
	RegionCode    string    `json:"region_code"`
	Status        string    `json:"status"`
	BoundarySHA   string    `json:"boundary_sha256"`
	EffectiveFrom time.Time `json:"effective_from"`
	SupersededBy  string    `json:"superseded_by,omitempty"`
	Note          string    `json:"note"`
	CountyCount   int       `json:"county_count"`
	CreatedAt     time.Time `json:"created_at"`
}

type WidthBackfill struct {
	DeviceSerial  string  `json:"device_serial"`
	SegmentID     string  `json:"segment_id"`
	WorkingWidthM float64 `json:"working_width_m"`
}

type SpatialPolicyInput struct {
	BoundaryVersionID string  `json:"boundary_version_id"`
	OutputGrain       string  `json:"output_grain"` // county | grid
	Method            string  `json:"method"`       // intersection | raster
	GridSizeM         int     `json:"grid_size_m"`
	DefaultAccuracyM  float64 `json:"default_accuracy_m"`
}

type SpatialPolicy struct {
	PolicyID          string    `json:"policy_id"`
	RegionCode        string    `json:"region_code"`
	BoundaryVersionID string    `json:"boundary_version_id"`
	OutputGrain       string    `json:"output_grain"`
	Method            string    `json:"method"`
	GridSizeM         int       `json:"grid_size_m,omitempty"`
	DefaultAccuracyM  float64   `json:"default_accuracy_m"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

// SpatialBin is one approved-grain aggregate line. It never contains
// coordinates: grid bins use opaque cell references whose geographic anchor
// stays in the region.
type SpatialBin struct {
	UnitType         string  `json:"unit_type"` // county | grid
	UnitCode         string  `json:"unit_code"`
	CountyCode       string  `json:"county_code,omitempty"`
	OperationType    string  `json:"operation_type"`
	SegmentCount     int     `json:"segment_count"`
	AreaHa           float64 `json:"area_ha"`
	AreaHaMax        float64 `json:"area_ha_max"` // incl. candidate ambiguous area
	UncertaintyHa    float64 `json:"uncertainty_ha"`
	DurationHours    float64 `json:"duration_hours"`
	CoveredLengthM   float64 `json:"covered_length_m"`
	GridCellsTouched int     `json:"grid_cells_touched,omitempty"`
}

type PendingByReason struct {
	Reason        string  `json:"reason"`
	AreaHa        float64 `json:"area_ha"`
	DurationHours float64 `json:"duration_hours"`
	SegmentCount  int     `json:"segment_count"`
}

// SpatialArtifact is the only spatial payload allowed to leave a region.
type SpatialArtifact struct {
	Schema             string            `json:"schema"`
	SummaryID          string            `json:"summary_id"`
	SourceRegion       string            `json:"source_region"`
	PolicyID           string            `json:"policy_id"`
	BoundaryVersionID  string            `json:"boundary_version_id"`
	BoundarySHA256     string            `json:"boundary_sha256"`
	Method             string            `json:"method"`
	OutputGrain        string            `json:"output_grain"`
	GridSizeM          int               `json:"grid_size_m,omitempty"`
	PeriodFrom         time.Time         `json:"period_from"`
	PeriodTo           time.Time         `json:"period_to"`
	OperationType      string            `json:"operation_type"`
	GeneratedAt        time.Time         `json:"generated_at"`
	Bins               []SpatialBin      `json:"bins"`
	Pending            []PendingByReason `json:"pending"`
	PendingTotalHa     float64           `json:"pending_total_ha"`
	AllocatedTotalHa   float64           `json:"allocated_total_ha"`
	DeclaredTotalHa    float64           `json:"declared_total_ha"`
	SourceSegmentCount int               `json:"source_segment_count"`
	DuplicateCount     int               `json:"duplicate_count"`
	SupersedesSummary  string            `json:"supersedes_summary_id,omitempty"`
	ContainsRawPoints  bool              `json:"contains_raw_trajectories"`
	Notes              []string          `json:"notes"`
}

// ExcludedFragment is regional-only detail: why a fragment was held back and
// which counties its uncertainty disk could reach. No coordinates stored.
type ExcludedFragment struct {
	OperationID   string   `json:"operation_id"`
	SegmentID     string   `json:"segment_id"`
	DeviceSerial  string   `json:"device_serial"`
	Status        string   `json:"status"`
	Reason        string   `json:"reason"`
	LengthM       float64  `json:"ambiguous_length_m"`
	AreaHa        float64  `json:"pending_area_ha"`
	Candidates    []string `json:"candidate_counties"`
	WorkingWidthM float64  `json:"working_width_m,omitempty"`
}

type SpatialSummary struct {
	SummaryID          string             `json:"summary_id"`
	RegionCode         string             `json:"region_code"`
	PolicyID           string             `json:"policy_id"`
	BoundaryVersionID  string             `json:"boundary_version_id"`
	BoundarySHA256     string             `json:"boundary_sha256"`
	Method             string             `json:"method"`
	OutputGrain        string             `json:"output_grain"`
	GridSizeM          int                `json:"grid_size_m,omitempty"`
	PeriodFrom         time.Time          `json:"period_from"`
	PeriodTo           time.Time          `json:"period_to"`
	OperationType      string             `json:"operation_type"`
	Status             string             `json:"status"`
	SupersedesSummary  string             `json:"supersedes_summary_id,omitempty"`
	DataCutoff         time.Time          `json:"data_cutoff"`
	Bins               []SpatialBin       `json:"bins"`
	Pending            []PendingByReason  `json:"pending"`
	ExcludedFragments  []ExcludedFragment `json:"excluded_fragments"`
	SourceSegmentCount int                `json:"source_segment_count"`
	DuplicateCount     int                `json:"duplicate_count"`
	ArtifactSHA256     string             `json:"artifact_sha256"`
	OutboxSeq          int64              `json:"outbox_seq,omitempty"`
	SourceNode         string             `json:"source_node"`
	CreatedAt          time.Time          `json:"created_at"`
}

type SpatialSegmentLedgerRow struct {
	OperationID       string   `json:"operation_id"`
	SegmentID         string   `json:"segment_id"`
	DeviceSerial      string   `json:"device_serial"`
	Status            string   `json:"status"`
	PendingReason     string   `json:"pending_reason,omitempty"`
	BoundaryVersionID string   `json:"boundary_version_id"`
	PolicyID          string   `json:"policy_id"`
	Method            string   `json:"method"`
	WorkingWidthM     float64  `json:"working_width_m,omitempty"`
	DeclaredAreaHa    float64  `json:"declared_area_ha"`
	TotalLengthM      float64  `json:"total_length_m"`
	AllocatedLengthM  float64  `json:"allocated_length_m"`
	AmbiguousLengthM  float64  `json:"ambiguous_length_m"`
	AllocatedAreaHa   float64  `json:"allocated_area_ha"`
	PendingAreaHa     float64  `json:"pending_area_ha"`
	DurationS         int      `json:"duration_s"`
	Candidates        []string `json:"candidate_counties"`
	ContentSHA256     string   `json:"content_sha256"`
}

type DedupLogRow struct {
	DeviceSerial  string    `json:"device_serial"`
	SegmentID     string    `json:"segment_id"`
	BatchID       string    `json:"batch_id"`
	ContentSHA256 string    `json:"content_sha256"`
	ReceivedAt    time.Time `json:"received_at"`
}

type SpatialDelivery struct {
	ID            string     `json:"id"`
	SourceRegion  string     `json:"source_region"`
	SummaryID     string     `json:"summary_id"`
	ApprovalID    string     `json:"approval_id"`
	Status        string     `json:"status"`
	Channel       string     `json:"channel"`
	Checks        []Check    `json:"checks"`
	BlockedReason string     `json:"blocked_reason,omitempty"`
	Artifact      any        `json:"artifact,omitempty"`
	ArtifactSHA   string     `json:"artifact_sha256,omitempty"`
	AuditSeq      int64      `json:"audit_seq,omitempty"`
	RequestedBy   string     `json:"requested_by"`
	CreatedAt     time.Time  `json:"created_at"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

type SpatialDeliveryInput struct {
	SourceRegion string `json:"source_region"`
	SummaryID    string `json:"summary_id"`
	ApprovalID   string `json:"approval_id"`
}

type SpatialArtifactVerification struct {
	DeliveryID     string    `json:"delivery_id"`
	Status         string    `json:"status"`
	RecomputedSHA  string    `json:"recomputed_sha256,omitempty"`
	RecordedSHA    string    `json:"recorded_sha256,omitempty"`
	SHA256Matches  bool      `json:"sha256_matches"`
	RestrictedHits []string  `json:"restricted_fields,omitempty"`
	AuditSeq       int64     `json:"audit_seq,omitempty"`
	AuditValid     bool      `json:"audit_entry_valid"`
	AuditVerified  bool      `json:"audit_chain_valid"`
	ChainHead      string    `json:"audit_chain_head,omitempty"`
	VerifiedAt     time.Time `json:"verified_at"`
	ArtifactSize   int       `json:"artifact_bytes"`
}

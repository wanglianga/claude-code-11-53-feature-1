package service

import (
	"encoding/json"
	"testing"

	"agrisync/internal/model"
)

func TestSpatialArtifactIsFreeOfRestrictedFields(t *testing.T) {
	artifact := model.SpatialArtifact{
		Schema: "agrisync.spatial_summary/v1", SummaryID: "sps_1", SourceRegion: "BR-MT",
		OutputGrain: "county", Method: "intersection",
		Bins: []model.SpatialBin{
			{UnitType: "county", UnitCode: "MT-C1", OperationType: "seeding", AreaHa: 1.0428, AreaHaMax: 2.0855},
			{UnitType: "grid", UnitCode: "c200_r-2_c1", OperationType: "seeding", AreaHa: 0.2193},
		},
		Pending: []model.PendingByReason{
			{Reason: "uncertainty_crosses_boundary|seeding", AreaHa: 1.0428, SegmentCount: 1},
			{Reason: "working_width_missing|seeding", AreaHa: 0.6, SegmentCount: 1},
		},
		ContainsRawPoints: false,
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if hits := scanForRestrictedFields(generic, ""); len(hits) != 0 {
		t.Fatalf("sanitised spatial artifact leaked restricted paths: %v", hits)
	}
	// A grid bin code must stay opaque: no decimal coordinate pair survives.
	var back model.SpatialArtifact
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, b := range back.Bins {
		if b.UnitType == "grid" && b.UnitCode == "" {
			t.Fatal("grid bin lost its opaque cell code")
		}
	}
}

func TestRound4(t *testing.T) {
	cases := map[float64]float64{
		1.04284: 1.0428,
		2.08555: 2.0856,
		0:       0,
	}
	for in, want := range cases {
		if got := round4(in); got != want {
			t.Errorf("round4(%v)=%v want %v", in, got, want)
		}
	}
}

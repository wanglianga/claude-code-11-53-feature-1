package geo

import (
	"math"
	"testing"
)

// Two neighbouring county rectangles sharing the edge x=1000:
//
//	A: [0,1000] x [0,1000]      B: [1000,2000] x [0,1000]
func twoCounties() []County {
	ring := func(x0, x1 float64) Ring {
		return Ring{
			{X: x0, Y: 0}, {X: x1, Y: 0}, {X: x1, Y: 1000}, {X: x0, Y: 1000},
		}
	}
	return []County{{Code: "A", Ring: ring(0, 1000)}, {Code: "B", Ring: ring(1000, 2000)}}
}

func TestClipPolylineStraightCrossCountyLine(t *testing.T) {
	counties := twoCounties()
	// Straight run from x=200 to x=1800 at y=500: 800 m in A, 800 m in B.
	line := []Point{{X: 200, Y: 500}, {X: 1800, Y: 500}}
	pieces := ClipPolyline(line, []float64{0, 0}, counties)
	if len(pieces) != 2 {
		t.Fatalf("expected 2 pieces, got %d: %+v", len(pieces), pieces)
	}
	byCounty := map[string]float64{}
	for _, p := range pieces {
		if p.Ambiguous {
			t.Fatalf("high-accuracy run must not be ambiguous: %+v", p)
		}
		byCounty[p.County] += p.LengthM
	}
	if math.Abs(byCounty["A"]-800) > 1e-6 || math.Abs(byCounty["B"]-800) > 1e-6 {
		t.Fatalf("length split wrong: %+v", byCounty)
	}
	// Area = length * working width (12 m), split by the same shares.
	width := 12.0
	total := 0.0
	for _, p := range pieces {
		total += SquareMToHa(p.LengthM * width)
	}
	if math.Abs(total-SquareMToHa(1600*width)) > 1e-9 {
		t.Fatalf("apportioned area does not sum to total: %v", total)
	}
}

func TestClipPolylineUncertaintyAcrossBoundaryIsAmbiguous(t *testing.T) {
	counties := twoCounties()
	// The endpoint sits on the shared border with a 60 m accuracy radius:
	// the uncertainty disk covers both counties, so the fragment touching it
	// must be held unallocated instead of assigned to either side.
	line := []Point{{X: 700, Y: 500}, {X: 1000, Y: 500}, {X: 1300, Y: 500}}
	acc := []float64{5, 60, 5}
	pieces := ClipPolyline(line, acc, counties)
	sawAmbiguous := false
	allocated := 0.0
	for _, p := range pieces {
		if p.Ambiguous {
			sawAmbiguous = true
			if p.Reason != "uncertainty_crosses_boundary" {
				t.Fatalf("unexpected ambiguity reason %q", p.Reason)
			}
			if len(p.Candidates) != 2 {
				t.Fatalf("ambiguous fragment must list both candidate counties: %+v", p.Candidates)
			}
			continue
		}
		allocated += p.LengthM
	}
	if !sawAmbiguous {
		t.Fatalf("border-straddling low-accuracy fragment was not flagged: %+v", pieces)
	}
	// Only the confidently-inside portions (700->940-ish in A and the B side
	// beyond the uncertain vertex) may be allocated; the ambiguous length is
	// excluded from county totals.
	if allocated >= 600 {
		t.Fatalf("ambiguous length was allocated to a county: %v", allocated)
	}
}

func TestClipPolylineOutsideCatalogIsExcluded(t *testing.T) {
	counties := twoCounties()
	// A run entirely below the catalog rectangles.
	line := []Point{{X: 100, Y: -400}, {X: 900, Y: -400}}
	pieces := ClipPolyline(line, []float64{0, 0}, counties)
	for _, p := range pieces {
		if p.County != "" {
			t.Fatalf("outside-catalog run assigned to county %s", p.County)
		}
	}
}

func TestRasterizeQuantizesToApprovedResolution(t *testing.T) {
	counties := twoCounties()
	line := []Point{{X: 200, Y: 500}, {X: 1800, Y: 500}}
	cells := Rasterize(line, nil, counties, 100, 0)
	if len(cells) == 0 {
		t.Fatal("raster produced no cells")
	}
	lengthByCounty := map[string]float64{}
	for _, c := range cells {
		if c.Ambiguous {
			t.Fatalf("high-accuracy raster cells must be decisive: %+v", c)
		}
		lengthByCounty[c.County] += c.LengthM
	}
	// Quantization tolerance: each side's covered length is within one cell of
	// the exact 800 m, and the total still covers the full run.
	if math.Abs(lengthByCounty["A"]-lengthByCounty["B"]) > 200 {
		t.Fatalf("raster split wildly asymmetric: %+v", lengthByCounty)
	}
	total := lengthByCounty["A"] + lengthByCounty["B"]
	if math.Abs(total-1600) > 1e-6 {
		t.Fatalf("raster lost coverage: total=%v want 1600", total)
	}
}

func TestRasterizeBoundaryCellsBecomeAmbiguous(t *testing.T) {
	counties := twoCounties()
	// Run along the shared border x=1000 with a coarse 80 m uncertainty:
	// border cells must be ambiguous rather than silently assigned.
	line := []Point{{X: 1000, Y: 100}, {X: 1000, Y: 900}}
	cells := Rasterize(line, nil, counties, 100, 80)
	ambiguous := 0
	for _, c := range cells {
		if c.Ambiguous {
			ambiguous++
		}
	}
	if ambiguous == 0 {
		t.Fatal("border cells under coarse accuracy were not marked ambiguous")
	}
}

func TestRasterInheritsPerVertexAmbiguityIntoCoarseCells(t *testing.T) {
	counties := twoCounties()
	// Coarse 500 m cells whose centers are far from the border, but the middle
	// vertex sits exactly on x=1000 with a 60 m uncertainty disk. Vertex
	// ambiguity must propagate into every cell the adjacent edges cover.
	line := []Point{{X: 400, Y: 500}, {X: 1000, Y: 500}, {X: 1600, Y: 500}}
	acc := []float64{2, 60, 2}
	cells := Rasterize(line, acc, counties, 500, 2)
	sawAmbiguous := false
	for _, c := range cells {
		if c.Ambiguous {
			sawAmbiguous = true
			if len(c.Candidates) != 2 {
				t.Fatalf("ambiguous coarse cell must name both counties: %+v", c)
			}
		}
	}
	if !sawAmbiguous {
		t.Fatal("per-vertex uncertainty vanished under coarse rasterization")
	}
}

func TestProjectorRoundTrip(t *testing.T) {
	center := LatLon{Lat: -12.64, Lon: -55.42}
	pr := NewProjector(center)
	origin := pr.Project(center)
	if math.Abs(origin.X) > 1e-9 || math.Abs(origin.Y) > 1e-9 {
		t.Fatalf("projector origin offset: %+v", origin)
	}
	// One degree of latitude is ~111.32 km.
	north := pr.Project(LatLon{Lat: center.Lat + 1, Lon: center.Lon})
	if math.Abs(north.Y-111320) > 1 {
		t.Fatalf("latitude scale wrong: %v", north.Y)
	}
}

func TestCountyAt(t *testing.T) {
	counties := twoCounties()
	if c := CountyAt(Point{X: 500, Y: 500}, counties); c != "A" {
		t.Fatalf("point in A resolved as %q", c)
	}
	if c := CountyAt(Point{X: 1500, Y: 500}, counties); c != "B" {
		t.Fatalf("point in B resolved as %q", c)
	}
	if c := CountyAt(Point{X: 500, Y: 1500}, counties); c != "" {
		t.Fatalf("outside point resolved as %q", c)
	}
}

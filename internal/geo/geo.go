// Package geo implements the in-region spatial engine. It is deliberately
// dependency-free (no PostGIS, no third-party geometry library): every
// calculation runs inside the region process on raw trajectories that never
// leave the region.
//
// Two allocation methods are provided:
//
//   - Intersection: each trajectory polyline is clipped exactly against the
//     approved administrative polygons; worked area (length x working width)
//     is apportioned by the resulting in-county length shares.
//   - Raster: the same work is accumulated on a regular grid at the approved
//     resolution; cells are rolled up to the authorised output grain.
//
// Positional uncertainty is a first-class input. Whenever the uncertainty
// radius around a sampled position reaches across a boundary (or the position
// is closer to any boundary than its accuracy radius), the affected piece is
// flagged ambiguous and its area is held as unallocated rather than assigned
// to whichever county looks most convenient.
package geo

import (
	"math"
	"sort"
)

// Point is a projected position in local metric coordinates.
type Point struct {
	X float64
	Y float64
}

// LatLon is one geographic coordinate. These values only exist inside the
// region database and region process memory.
type LatLon struct {
	Lat float64
	Lon float64
}

// Ring is a closed simple polygon ring (first and last point may differ; the
// engine closes it implicitly).
type Ring []Point

// County is one administrative unit from the region's boundary catalog.
type County struct {
	Code string
	Ring Ring
}

// Projector converts lat/lon to a local tangent-plane metric frame. The
// equirectangular projection is accurate at county scale (sub-centimetre
// distortion over a few tens of kilometres) and keeps the engine fully
// deterministic.
type Projector struct {
	lat0    float64
	lon0    float64
	mPerLat float64
	mPerLon float64
}

func NewProjector(center LatLon) *Projector {
	return &Projector{
		lat0:    center.Lat,
		lon0:    center.Lon,
		mPerLat: 111320.0,
		mPerLon: 111320.0 * math.Cos(center.Lat*math.Pi/180.0),
	}
}

func (p *Projector) Project(ll LatLon) Point {
	return Point{X: (ll.Lon - p.lon0) * p.mPerLon, Y: (ll.Lat - p.lat0) * p.mPerLat}
}

// BoundsCenter returns the geographic center of a set of coordinates, used to
// anchor a projector for one trajectory.
func BoundsCenter(points []LatLon) LatLon {
	if len(points) == 0 {
		return LatLon{}
	}
	minLat, maxLat := points[0].Lat, points[0].Lat
	minLon, maxLon := points[0].Lon, points[0].Lon
	for _, p := range points[1:] {
		minLat = math.Min(minLat, p.Lat)
		maxLat = math.Max(maxLat, p.Lat)
		minLon = math.Min(minLon, p.Lon)
		maxLon = math.Max(maxLon, p.Lon)
	}
	return LatLon{Lat: (minLat + maxLat) / 2, Lon: (minLon + maxLon) / 2}
}

// --- primitives ---------------------------------------------------------

// Distance returns the metric distance between two projected points.
func Distance(a, b Point) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	return math.Hypot(dx, dy)
}

func distance(a, b Point) float64 {
	return Distance(a, b)
}

// pointInRing implements the ray-cast test in metric coordinates.
func pointInRing(pt Point, ring Ring) bool {
	inside := false
	n := len(ring)
	if n < 3 {
		return false
	}
	j := n - 1
	for i := 0; i < n; i++ {
		xi, yi := ring[i].X, ring[i].Y
		xj, yj := ring[j].X, ring[j].Y
		if (yi > pt.Y) != (yj > pt.Y) {
			xIntersect := (xj-xi)*(pt.Y-yi)/(yj-yi) + xi
			if pt.X < xIntersect {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

// CountyAt returns the county containing pt, or "" when the point falls
// outside every catalog polygon.
func CountyAt(pt Point, counties []County) string {
	for _, c := range counties {
		if pointInRing(pt, c.Ring) {
			return c.Code
		}
	}
	return ""
}

// segmentIntersection returns the intersection point of segments a-b and c-d
// together with the parameter t along a-b, or ok=false.
func segmentIntersection(a, b, c, d Point) (pt Point, t float64, ok bool) {
	rx, ry := b.X-a.X, b.Y-a.Y
	sx, sy := d.X-c.X, d.Y-c.Y
	denom := rx*sy - ry*sx
	if math.Abs(denom) < 1e-12 {
		return Point{}, 0, false
	}
	qx, qy := c.X-a.X, c.Y-a.Y
	t = (qx*sy - qy*sx) / denom
	u := (qx*ry - qy*rx) / denom
	if t < 0 || t > 1 || u < 0 || u > 1 {
		return Point{}, 0, false
	}
	return Point{X: a.X + t*rx, Y: a.Y + t*ry}, t, true
}

// distanceToSegment is the minimum euclidean distance from p to segment a-b.
func distanceToSegment(p, a, b Point) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	l2 := dx*dx + dy*dy
	if l2 < 1e-12 {
		return distance(p, a)
	}
	t := ((p.X-a.X)*dx + (p.Y-a.Y)*dy) / l2
	t = math.Max(0, math.Min(1, t))
	proj := Point{X: a.X + t*dx, Y: a.Y + t*dy}
	return distance(p, proj)
}

// distanceToRing returns the distance from p to the nearest ring edge.
func distanceToRing(p Point, ring Ring) float64 {
	best := math.Inf(1)
	n := len(ring)
	for i := 0; i < n; i++ {
		a, b := ring[i], ring[(i+1)%n]
		if d := distanceToSegment(p, a, b); d < best {
			best = d
		}
	}
	return best
}

// Piece is one allocated or ambiguous fragment of a clipped trajectory.
type Piece struct {
	County        string
	LengthM       float64
	Ambiguous     bool
	Reason        string
	Candidates    []string // counties the uncertainty buffer may reach
	StartAccuracy float64
	EndAccuracy   float64
}

// VertexAmbiguous is the exported form of vertexAmbiguous, used by callers that
// need to propagate per-vertex uncertainty into coarser structures (raster
// cells inherit the ambiguity of every edge vertex that covers them).
func VertexAmbiguous(vertex Point, accuracyM float64, counties []County) (bool, []string) {
	return vertexAmbiguous(vertex, accuracyM, counties)
}

// vertexAmbiguous decides whether the uncertainty disk around a vertex
// crosses a county boundary. A vertex is ambiguous when:
//   - its accuracy radius reaches a boundary edge, or
//   - compass samples on the accuracy disk fall into more than one county.
func vertexAmbiguous(vertex Point, accuracyM float64, counties []County) (bool, []string) {
	if accuracyM <= 0 {
		return false, nil
	}
	hit := map[string]bool{}
	if c := CountyAt(vertex, counties); c != "" {
		hit[c] = true
	}
	for _, c := range counties {
		if distanceToRing(vertex, c.Ring) <= accuracyM {
			hit[c.Code] = true
		}
	}
	// Eight compass samples on the uncertainty disk catch a boundary that
	// passes through the disk without an edge endpoint inside the radius.
	for k := 0; k < 8; k++ {
		ang := float64(k) * math.Pi / 4
		sample := Point{X: vertex.X + accuracyM*math.Cos(ang), Y: vertex.Y + accuracyM*math.Sin(ang)}
		if c := CountyAt(sample, counties); c != "" {
			hit[c] = true
		}
	}
	if len(hit) <= 1 {
		return false, nil
	}
	out := make([]string, 0, len(hit))
	for c := range hit {
		out = append(out, c)
	}
	sort.Strings(out)
	return true, out
}

// ClipPolyline splits a projected trajectory against the county polygons.
// Vertex accuracies are per-sample radii in metres; the returned pieces carry
// the ambiguity decision for the fragment they belong to.
func ClipPolyline(line []Point, accuracies []float64, counties []County) []Piece {
	if len(line) < 2 {
		return nil
	}
	type cut struct {
		t      float64
		county string
	}
	pieces := []Piece{}
	for i := 0; i+1 < len(line); i++ {
		a, b := line[i], line[i+1]
		edgeLen := distance(a, b)
		if edgeLen < 1e-9 {
			continue
		}
		cuts := []cut{{t: 0}, {t: 1}}
		for _, c := range counties {
			n := len(c.Ring)
			for j := 0; j < n; j++ {
				r0, r1 := c.Ring[j], c.Ring[(j+1)%n]
				if _, t, ok := segmentIntersection(a, b, r0, r1); ok && t > 0 && t < 1 {
					cuts = append(cuts, cut{t: t, county: c.Code})
				}
			}
		}
		sort.Slice(cuts, func(k, l int) bool { return cuts[k].t < cuts[l].t })
		// De-duplicate coincident cuts (shared border edges).
		uniq := cuts[:0]
		for _, c := range cuts {
			if len(uniq) == 0 || math.Abs(uniq[len(uniq)-1].t-c.t) > 1e-9 {
				uniq = append(uniq, c)
			}
		}
		cuts = uniq
		for k := 0; k+1 < len(cuts); k++ {
			t0, t1 := cuts[k].t, cuts[k+1].t
			mid := Point{X: a.X + (b.X-a.X)*(t0+t1)/2, Y: a.Y + (b.Y-a.Y)*(t0+t1)/2}
			county := CountyAt(mid, counties)
			length := edgeLen * (t1 - t0)
			acc0, acc1 := 0.0, 0.0
			if i < len(accuracies) {
				acc0 = accuracies[i]
			}
			if i+1 < len(accuracies) {
				acc1 = accuracies[i+1]
			}
			amb, candidates := vertexAmbiguous(a, acc0, counties)
			amb1, cand1 := vertexAmbiguous(b, acc1, counties)
			if amb1 {
				amb = true
				for _, c := range cand1 {
					if !contains(candidates, c) {
						candidates = append(candidates, c)
					}
				}
				sort.Strings(candidates)
			}
			reason := ""
			if amb {
				reason = "uncertainty_crosses_boundary"
			} else if county == "" {
				reason = "outside_boundary_catalog"
			}
			pieces = append(pieces, Piece{
				County: county, LengthM: length, Ambiguous: amb, Reason: reason,
				Candidates: candidates, StartAccuracy: acc0, EndAccuracy: acc1,
			})
		}
	}
	// Merge adjacent compatible pieces so the ledger reads per county rather
	// than per edge.
	merged := []Piece{}
	for _, p := range pieces {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.County == p.County && last.Ambiguous == p.Ambiguous && last.Reason == p.Reason {
				last.LengthM += p.LengthM
				last.EndAccuracy = p.EndAccuracy
				if !sameStringSet(last.Candidates, p.Candidates) {
					for _, c := range p.Candidates {
						if !contains(last.Candidates, c) {
							last.Candidates = append(last.Candidates, c)
						}
					}
					sort.Strings(last.Candidates)
				}
				continue
			}
		}
		cp := p
		cp.Candidates = append([]string(nil), p.Candidates...)
		merged = append(merged, cp)
	}
	return merged
}

// --- raster method ------------------------------------------------------

// Cell is one grid cell at the approved resolution. Row/col indices are local
// to the segment grid; the geographic anchor stays in the region and is never
// exported.
type Cell struct {
	Row        int
	Col        int
	County     string
	LengthM    float64
	Ambiguous  bool
	Candidates []string
}

// Rasterize accumulates trajectory coverage onto a square grid of cellSizeM
// metres anchored at the local trajectory origin. Cell-county assignment uses
// the cell center; a cell whose center's uncertainty disk (the largest
// per-point accuracy of the edges covering it) reaches another county is
// ambiguous and never allocated.
//
// Traversal is Amanatides-Woo: every metre of an edge is attributed to exactly
// one cell, so a run lying on a grid line is never counted on both sides.
func Rasterize(line []Point, accuracies []float64, counties []County, cellSizeM, fallbackAccuracyM float64) []Cell {
	if len(line) == 0 {
		return nil
	}
	minX, minY := line[0].X, line[0].Y
	for _, p := range line[1:] {
		minX = math.Min(minX, p.X)
		minY = math.Min(minY, p.Y)
	}
	return RasterizeGrid(line, accuracies, counties, cellSizeM, fallbackAccuracyM, Point{X: minX, Y: minY})
}

// RasterizeGrid is the global-anchor form: row/col indices are computed against
// a fixed frame origin so cells from different segments share one reference
// grid. The indices are quantised at cellSizeM and never expose coordinates
// finer than the approved resolution.
func RasterizeGrid(line []Point, accuracies []float64, counties []County, cellSizeM, fallbackAccuracyM float64, origin Point) []Cell {
	if len(line) < 2 || cellSizeM <= 0 {
		return nil
	}
	accuracyAt := func(i int) float64 {
		if i < len(accuracies) && accuracies[i] > 0 {
			return accuracies[i]
		}
		return fallbackAccuracyM
	}
	cellOf := func(p Point) (int, int) {
		col := int(math.Floor((p.X - origin.X) / cellSizeM))
		row := int(math.Floor((p.Y - origin.Y) / cellSizeM))
		return row, col
	}
	type key struct{ row, col int }
	length := map[key]float64{}
	cellAccuracy := map[key]float64{}
	cellAmbiguous := map[key]bool{}
	cellCandidates := map[key][]string{}
	order := []key{}
	// Per-vertex ambiguity is computed once; cells inherit it from every edge
	// vertex that covers them, so a 60 m uncertainty cannot disappear just
	// because a 200 m cell's center happens to sit 90 m from the border.
	vertexAmb := make([]bool, len(line))
	vertexCand := make([][]string, len(line))
	for i, p := range line {
		vertexAmb[i], vertexCand[i] = vertexAmbiguous(p, accuracyAt(i), counties)
	}
	add := func(k key, l, acc float64, amb bool, cand []string) {
		if _, ok := length[k]; !ok {
			order = append(order, k)
		}
		length[k] += l
		if acc > cellAccuracy[k] {
			cellAccuracy[k] = acc
		}
		if amb {
			cellAmbiguous[k] = true
			for _, c := range cand {
				if !contains(cellCandidates[k], c) {
					cellCandidates[k] = append(cellCandidates[k], c)
				}
			}
			sort.Strings(cellCandidates[k])
		}
	}
	inf := math.Inf(1)
	for i := 0; i+1 < len(line); i++ {
		a, b := line[i], line[i+1]
		edgeLen := distance(a, b)
		if edgeLen < 1e-9 {
			continue
		}
		// The uncertainty envelope of an edge piece is the larger of its two
		// endpoint radii; ambiguity of either endpoint propagates to every
		// cell the edge covers.
		edgeAccuracy := math.Max(accuracyAt(i), accuracyAt(i+1))
		edgeAmb := vertexAmb[i] || vertexAmb[i+1]
		edgeCand := append(append([]string(nil), vertexCand[i]...), vertexCand[i+1]...)
		sort.Strings(edgeCand)
		dx, dy := b.X-a.X, b.Y-a.Y
		row, col := cellOf(a)
		stepX, stepY := 0, 0
		tMaxX, tMaxY, tDeltaX, tDeltaY := inf, inf, inf, inf
		if dx > 0 {
			stepX = 1
			tMaxX = ((origin.X + float64(col+1)*cellSizeM) - a.X) / dx
			tDeltaX = cellSizeM / dx
		} else if dx < 0 {
			stepX = -1
			tMaxX = (a.X - (origin.X + float64(col)*cellSizeM)) / -dx
			tDeltaX = cellSizeM / -dx
		}
		if dy > 0 {
			stepY = 1
			tMaxY = ((origin.Y + float64(row+1)*cellSizeM) - a.Y) / dy
			tDeltaY = cellSizeM / dy
		} else if dy < 0 {
			stepY = -1
			tMaxY = (a.Y - (origin.Y + float64(row)*cellSizeM)) / -dy
			tDeltaY = cellSizeM / -dy
		}
		t := 0.0
		for t < 1 {
			tNext := math.Min(math.Min(tMaxX, tMaxY), 1.0)
			add(key{row, col}, edgeLen*(tNext-t), edgeAccuracy, edgeAmb, edgeCand)
			if tNext >= 1.0 {
				break
			}
			if tMaxX < tMaxY {
				tMaxX += tDeltaX
				col += stepX
			} else {
				tMaxY += tDeltaY
				row += stepY
			}
			t = tNext
		}
	}
	out := make([]Cell, 0, len(order))
	for _, k := range order {
		center := Point{
			X: origin.X + (float64(k.col)+0.5)*cellSizeM,
			Y: origin.Y + (float64(k.row)+0.5)*cellSizeM,
		}
		cell := Cell{Row: k.row, Col: k.col, LengthM: length[k]}
		cell.County = CountyAt(center, counties)
		if cellAmbiguous[k] {
			cell.Ambiguous = true
			cell.Candidates = cellCandidates[k]
		} else {
			cell.Ambiguous, cell.Candidates = vertexAmbiguous(center, cellAccuracy[k], counties)
		}
		if !cell.Ambiguous && cell.County == "" {
			cell.Ambiguous = true
		}
		out = append(out, cell)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Row != out[j].Row {
			return out[i].Row < out[j].Row
		}
		return out[i].Col < out[j].Col
	})
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// SquareMToHa converts square metres to hectares.
func SquareMToHa(m2 float64) float64 { return m2 / 10000.0 }

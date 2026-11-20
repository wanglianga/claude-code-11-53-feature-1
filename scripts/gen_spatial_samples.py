#!/usr/bin/env python3
"""Generate deterministic samples for the in-region spatial aggregation drill.

The drill lives in BR-MT. Two county rectangles share one border:

    MT-C1 (west)  lon [-55.430, -55.420]   lat [-12.650, -12.630]
    MT-C2 (east)  lon [-55.420, -55.410]   lat [-12.650, -12.630]

Boundary v2 moves the shared border 0.001 degree east, to lon -55.419.

Scenarios covered:
  1. SP-CROSS-1   straight run crossing the county line, high accuracy
  2. SP-LOWACC-1  run whose border vertex carries a 60 m uncertainty radius
  3. SP-NOWIDTH-1 clean in-county run uploaded without a working width
  4. batch B repeats SP-CROSS-1 (dedupe) and adds SP-CLEAN-2
"""
import json
import os
from datetime import datetime, timedelta, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "samples")
UTC = timezone.utc
BASE = datetime(2026, 11, 6, 8, 0, tzinfo=UTC)


def iso(dt):
    return dt.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def write(rel, payload):
    path = os.path.join(OUT, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        json.dump(payload, fh, indent=2)
        fh.write("\n")
    print("wrote", os.path.relpath(path, ROOT))


def ring(lon0, lon1):
    # Rectangular county ring, lat range -12.650 .. -12.630.
    return [
        [-12.630, lon0], [-12.630, lon1], [-12.650, lon1], [-12.650, lon0],
    ]


def point(lat, lon, dt, accuracy=None):
    p = {"lat": lat, "lon": lon, "at": iso(dt)}
    return p


def op(device, segment, started, area, duration, points, width=None,
       accuracy=2.0, point_accuracy=None, field="MT-F-SP"):
    out = {
        "device_serial": device,
        "segment_id": segment,
        "operation_type": "seeding",
        "field_id": field,
        "area_ha": area,
        "fuel_l": round(area * 8.5, 2),
        "duration_s": duration,
        "completed": True,
        "started_at": iso(started),
        "ended_at": iso(started + timedelta(seconds=duration)),
        "points": points,
        "alarms": [],
        "accuracy_m": accuracy,
    }
    if width is not None:
        out["working_width_m"] = width
    if point_accuracy is not None:
        out["point_accuracy_m"] = point_accuracy
    return out


def main():
    # --- boundary catalogs -------------------------------------------------
    write("spatial/br-mt-boundaries-v1.json", {
        "note": "BR-MT county border v1: shared edge at lon -55.420",
        "counties": [
            {"county_code": "MT-C1", "county_name": "Campo Oeste", "ring": ring(-55.430, -55.420)},
            {"county_code": "MT-C2", "county_name": "Campo Leste", "ring": ring(-55.420, -55.410)},
        ],
    })
    write("spatial/br-mt-boundaries-v2.json", {
        "note": "BR-MT county border v2 (correction): shared edge moved to lon -55.419",
        "counties": [
            {"county_code": "MT-C1", "county_name": "Campo Oeste", "ring": ring(-55.430, -55.419)},
            {"county_code": "MT-C2", "county_name": "Campo Leste", "ring": ring(-55.419, -55.410)},
        ],
    })

    # --- batch A: cross-county, low-accuracy, width-missing ----------------
    t0 = BASE
    cross_points = [
        point(-12.640, -55.428 + k * 0.0032, t0 + timedelta(minutes=6 * k))
        for k in range(6)
    ]
    cross = op("BR-SEED-7701", "SP-CROSS-1", t0, 2.09, 1800, cross_points, width=12.0, accuracy=2.0)

    t1 = BASE + timedelta(hours=4)
    lowacc_points = [
        point(-12.635, -55.424, t1),
        point(-12.635, -55.420, t1 + timedelta(minutes=8)),
        point(-12.635, -55.416, t1 + timedelta(minutes=16)),
    ]
    lowacc = op("BR-HARV-7702", "SP-LOWACC-1", t1, 1.04, 1200, lowacc_points,
                width=12.0, accuracy=2.0, point_accuracy=[2.0, 60.0, 2.0])

    t2 = BASE + timedelta(hours=8)
    nowidth_points = [
        point(-12.645, -55.428 + k * 0.001, t2 + timedelta(minutes=5 * k))
        for k in range(5)
    ]
    # No working_width_m: the segment area must stay pending until backfilled.
    nowidth = op("BR-FERT-7703", "SP-NOWIDTH-1", t2, 0.6, 900, nowidth_points, width=None, accuracy=2.0)

    write("spatial/br-mt-batch-a.json", {
        "batch_id": "spatial-batch-a",
        "operations": [cross, lowacc, nowidth],
    })

    # --- batch B: exact duplicate of SP-CROSS-1 plus one new clean segment --
    t3 = BASE + timedelta(hours=12)
    clean2_points = [
        point(-12.642, -55.418 + k * 0.001, t3 + timedelta(minutes=5 * k))
        for k in range(5)
    ]
    clean2 = op("BR-SEED-7701", "SP-CLEAN-2", t3, 0.55, 900, clean2_points, width=12.0, accuracy=2.0)
    write("spatial/br-mt-batch-b.json", {
        "batch_id": "spatial-batch-b",
        # SP-CROSS-1 is byte-identical to batch A and must be deduped.
        "operations": [cross, clean2],
    })

    # --- spatial policies (boundary ids are deterministic after demo reset) -
    write("spatial/policy-intersection-county.json", {
        "boundary_version_id": "bdv_br_mt_1",
        "output_grain": "county",
        "method": "intersection",
        "default_accuracy_m": 2.0,
    })
    write("spatial/policy-raster-county.json", {
        "boundary_version_id": "bdv_br_mt_1",
        "output_grain": "county",
        "method": "raster",
        "grid_size_m": 200,
        "default_accuracy_m": 2.0,
    })
    write("spatial/policy-raster-grid.json", {
        "boundary_version_id": "bdv_br_mt_1",
        "output_grain": "grid",
        "method": "raster",
        "grid_size_m": 200,
        "default_accuracy_m": 2.0,
    })
    write("spatial/policy-intersection-county-v2.json", {
        "boundary_version_id": "bdv_br_mt_2",
        "output_grain": "county",
        "method": "intersection",
        "default_accuracy_m": 2.0,
    })

    # --- approval scoped to the spatial egress metric ----------------------
    write("approvals/spatial.json", {
        "purpose": "County-level completed area comparison under data residency rules",
        "recipient": "group-agronomy@example.org",
        "scope_regions": ["BR-MT"],
        "scope_metrics": ["spatial_area_ha"],
        "valid_until": "2026-12-31T23:59:59Z",
    })


if __name__ == "__main__":
    main()

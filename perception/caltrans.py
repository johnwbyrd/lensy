"""Caltrans CCTV inventory adapter.

Caltrans publishes a per-district camera inventory as JSON and, for some
cameras, a public HLS stream. The inventory's "inService" flag is advisory:
when this was written, 418 District 7 cameras advertised a stream and 146 of
them actually served a playlist. So this module does what the Exercise 2
answer says to do with a data portal of unknown reliability: it probes.

    python3 caltrans.py list --district 7 --route I-110        # inventory, with live check
    python3 caltrans.py still CCTV-196 out.jpg                  # one frame, for drawing a region of interest
    python3 caltrans.py config CCTV-196 CCTV-319                # config skeleton for cameras.json

Nothing here is specific to District 7; the district is a parameter.
"""
from __future__ import annotations

import argparse
import concurrent.futures as cf
import json
import os
import sys
import time
import urllib.error
import urllib.request

INVENTORY_URL = "https://cwwp2.dot.ca.gov/data/d{d}/cctv/cctvStatusD{dd}.json"
DEFAULT_CACHE = os.path.join(os.path.dirname(__file__), "..", "data", "caltrans_d{dd}.json")


def fetch_inventory(district: int, cache: str | None = None, max_age_s: float = 3600) -> list[dict]:
    """Return the district's cameras as flat dicts. Cached on disk because the
    portal is slow and we do not want a demo to depend on it answering."""
    cache = cache or DEFAULT_CACHE.format(dd=f"{district:02d}")
    raw = None
    if os.path.exists(cache) and time.time() - os.path.getmtime(cache) < max_age_s:
        raw = json.load(open(cache))
    else:
        url = INVENTORY_URL.format(d=district, dd=f"{district:02d}")
        try:
            with urllib.request.urlopen(url, timeout=30) as r:
                raw = json.loads(r.read())
            os.makedirs(os.path.dirname(cache), exist_ok=True)
            json.dump(raw, open(cache, "w"))
        except (urllib.error.URLError, TimeoutError, OSError) as e:
            if os.path.exists(cache):
                raw = json.load(open(cache))
            else:
                raise SystemExit(f"cannot fetch inventory and no cache: {e}")
    out = []
    for item in raw.get("data", []):
        c = item.get("cctv", {})
        loc = c.get("location", {})
        stream = c.get("imageData", {}).get("streamingVideoURL") or ""
        sid = stream.split("/")[-2].replace(".stream", "") if stream else ""
        out.append({
            "stream_id": sid, "index": c.get("index"), "in_service": c.get("inService") == "true",
            "route": loc.get("route"), "direction": loc.get("direction"), "name": loc.get("locationName"),
            "county": loc.get("county"), "nearby": loc.get("nearbyPlace"),
            "lat": loc.get("latitude"), "lon": loc.get("longitude"),
            "stream": stream, "still": c.get("imageData", {}).get("static", {}).get("currentImageURL"),
            "recorded": c.get("recordedDate"),
        })
    return out


def probe_streams(cams: list[dict], workers: int = 12, timeout: float = 8.0) -> list[dict]:
    """Ask each advertised playlist whether it answers. Adds 'live' and 'http' to each camera."""
    def one(c: dict) -> dict:
        c = dict(c)
        c["live"], c["http"] = False, 0
        if not c["stream"]:
            return c
        try:
            with urllib.request.urlopen(urllib.request.Request(c["stream"]), timeout=timeout) as r:
                c["http"] = r.status
                c["live"] = b"#EXTM3U" in r.read(2000)
        except urllib.error.HTTPError as e:
            c["http"] = e.code
        except Exception:
            pass
        return c
    with cf.ThreadPoolExecutor(workers) as ex:
        return list(ex.map(one, cams))


def grab_still(stream_url: str, out_path: str, tries: int = 40) -> bool:
    import cv2
    cap = cv2.VideoCapture(stream_url)
    frame = None
    for _ in range(tries):
        ok, f = cap.read()
        if ok:
            frame = f
            break
    cap.release()
    if frame is None:
        return False
    cv2.imwrite(out_path, frame)
    return True


# Caltrans burns a timestamp into the top right and a logo plus caption into
# the bottom left. Both are static pixels that would otherwise look like a
# stationary object, so they are masked by default.
CALTRANS_OVERLAY_MASK = [[0.0, 0.80, 0.72, 1.0], [0.52, 0.0, 1.0, 0.09]]


def config_entry(c: dict, agency: str = "us-st-06.dot", district: int = 7) -> dict:
    return {
        "id": f"D{district}-{c['stream_id']}",
        "camera": f"lrn:gov:{agency}:video:camera/D{district}-{c['stream_id']}",
        "agency": agency,
        "label": f"{c['route']} {c['direction']} · {c['name']}",
        "source": c["stream"],
        "lat": c["lat"], "lon": c["lon"],
        "roi": [[0.0, 0.70], [1.0, 0.70], [1.0, 0.84], [0.0, 0.84]],
        "mask": CALTRANS_OVERLAY_MASK,
        "confirm_seconds": 60,
        "analyze_fps": 3,
        "note": "EDIT roi: polygon over the shoulder as fractions of the frame; use `caltrans.py still` to look.",
    }


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    l = sub.add_parser("list", help="list cameras; --probe checks which streams really answer")
    l.add_argument("--district", type=int, default=7)
    l.add_argument("--route", help="filter, e.g. I-110")
    l.add_argument("--probe", action="store_true")
    l.add_argument("--json", action="store_true")
    s = sub.add_parser("still", help="save one frame from a camera's stream")
    s.add_argument("stream_id")
    s.add_argument("out")
    s.add_argument("--district", type=int, default=7)
    g = sub.add_parser("config", help="print cameras.json entries for the given stream ids")
    g.add_argument("stream_ids", nargs="+")
    g.add_argument("--district", type=int, default=7)
    g.add_argument("--agency", default="us-st-06.dot")
    a = ap.parse_args(argv)

    cams = fetch_inventory(a.district)
    if a.cmd == "list":
        cams = [c for c in cams if c["in_service"] and c["stream"]]
        if a.route:
            cams = [c for c in cams if c["route"] == a.route]
        if a.probe:
            t = time.time()
            cams = probe_streams(cams)
            live = sum(c["live"] for c in cams)
            print(f"# probed {len(cams)} advertised streams in {time.time()-t:.0f}s: {live} live", file=sys.stderr)
        if a.json:
            json.dump(cams, sys.stdout, indent=1)
            return 0
        for c in cams:
            flag = ("LIVE " if c.get("live") else f"{c.get('http', '-'):>4} ") if a.probe else ""
            print(f"{flag}{c['stream_id']:<10} {c['route']:<7} {c['direction']:<6} {c['name']}")
        return 0
    by_id = {c["stream_id"]: c for c in cams if c["stream_id"]}
    if a.cmd == "still":
        c = by_id.get(a.stream_id) or sys.exit(f"unknown stream id {a.stream_id}")
        ok = grab_still(c["stream"], a.out)
        print("saved" if ok else "no frame", a.out)
        return 0 if ok else 1
    if a.cmd == "config":
        entries = [config_entry(by_id[s], a.agency, a.district) for s in a.stream_ids if s in by_id]
        print(json.dumps({"cameras": entries}, indent=2))
        return 0
    return 0


if __name__ == "__main__":
    sys.exit(main())

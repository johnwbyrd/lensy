"""Perception worker: one camera in, candidate detections and heartbeats out.

    python3 main.py --source ../data/cam01.mp4 --camera lrn:gov:us-st-06.dot:video:camera/CAM01 \
        --agency us-st-06.dot --core http://localhost:8080

A trace ID is minted when a frame enters the system and travels with the
detection to the core and on to the console, so one alert can be followed
through every hop.
"""
from __future__ import annotations

import argparse
import collections
import json
import logging
import os
import sys
import threading
import time
from datetime import timedelta

from adapters import FeedAdapter, utcnow
from detector import StationaryObjectDetector
from emitter import EventCoreClient, new_trace_id

log = logging.getLogger("perception")


def load_camera_config(path: str, camera_id: str) -> dict:
    cfg = json.load(open(path))
    for c in cfg.get("cameras", []):
        if c.get("id") == camera_id:
            return c
    raise SystemExit(f"camera id {camera_id!r} not in {path}")


def parse_roi(text: str):
    """Either 'x,y,w,h' or a JSON polygon / list of polygons."""
    text = text.strip()
    if text.startswith("["):
        return json.loads(text)
    return tuple(float(v) for v in text.split(","))


def run(args: argparse.Namespace) -> int:
    client = EventCoreClient(args.core)
    is_stream = args.source.startswith(("rtsp://", "http://", "https://"))
    adapter = FeedAdapter(args.source, analyze_fps=args.analyze_fps, realtime=not args.fast, loop=not args.once)
    det = StationaryObjectDetector(roi=parse_roi(args.roi), mask=args.mask, confirm_seconds=args.confirm_seconds,
                                   min_area=args.min_area)
    emitted = 0
    # Heartbeats run on their own thread so they say "this worker is alive and
    # this is how old its newest frame is", whether or not frames are arriving.
    # HLS delivers frames in bursts one segment long (10 s on Caltrans), and a
    # heartbeat tied to frame arrival would flap stale/fresh every segment.
    state = {"last_frame": None, "fps_window": collections.deque(), "stop": False}

    def heartbeat_loop() -> None:
        while not state["stop"]:
            time.sleep(args.heartbeat_seconds)
            if args.probe:
                continue
            fr = state["last_frame"]
            now = time.monotonic()
            win = state["fps_window"]
            while win and now - win[0] > 30:
                win.popleft()
            fps = len(win) / min(30.0, max(1.0, now - t0))
            frame_ts = fr.captured_at if fr else utcnow()
            frame_age = (utcnow() - frame_ts).total_seconds() if fr else -1.0
            ok = client.heartbeat(args.camera, args.agency, frame_ts, fps, det.health.frozen,
                                  det.health.scene_change, args.source, args.label, frame_age)
            if det.health.frozen:
                log.warning("feed appears FROZEN (frame delta %.2f)", det.health.frame_delta)
            if not ok:
                log.warning("heartbeat failed: event core unreachable at %s", args.core)
    log.info("camera=%s agency=%s label=%r source=%s model=%s analyze_fps=%s confirm=%ss probe=%s",
             args.camera, args.agency, args.label, args.source, det.__class__.__name__, args.analyze_fps,
             args.confirm_seconds, args.probe)
    debug_written = False
    # Live feeds are analyzed on the wall clock. Replays (--fast) run on the
    # clip's own clock, so confirmation windows mean the same thing either way.
    t0 = time.monotonic()
    threading.Thread(target=heartbeat_loop, daemon=True).start()
    try:
        for frame in adapter.frames():
            state["last_frame"] = frame
            state["fps_window"].append(time.monotonic())
            trace_id = new_trace_id()
            # Replays and network streams run on the media clock; local files
            # played in real time run on the wall clock (they are the same thing).
            t = frame.media_time if (args.fast or is_stream) else time.monotonic() - t0
            cands = det.process(t, frame.image)
            if args.debug_dir and not debug_written and frame.seq >= 3:
                os.makedirs(args.debug_dir, exist_ok=True)
                import cv2
                cv2.imwrite(os.path.join(args.debug_dir, args.camera.split("/")[-1] + "-roi.jpg"), det.debug_view(frame.image))
                debug_written = True
            for cand in cands:
                still = det.annotate(frame.image, cand, args.camera)
                onset = frame.captured_at - timedelta(seconds=max(0.0, cand.confirmed_t - cand.onset_t))
                res = client.detection(trace_id=trace_id, camera=args.camera, agency=args.agency, cand=cand,
                                       image=still, captured_at=frame.captured_at, onset_estimate=onset,
                                       confirmed_at=frame.captured_at, emitted_at=utcnow(), probe=args.probe)
                emitted += 1
                log.info("candidate hazard=%s conf=%.2f bbox=%s stationary=%.1fs -> %s %s trace=%s",
                         cand.hazard, cand.confidence, cand.bbox, cand.stationary_seconds,
                         res.get("http"), res.get("status") or res.get("error"), trace_id)
                if args.probe and args.once:
                    return 0 if res.get("http") in (200, 201) else 2
    except KeyboardInterrupt:
        pass
    finally:
        state["stop"] = True
    log.info("done: %d candidates emitted", emitted)
    return 0 if (not args.probe or emitted) else 2


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--config", help="cameras.json; with --camera-id, loads source, roi, mask, label and windows from it")
    ap.add_argument("--camera-id", help="entry id inside --config")
    ap.add_argument("--source", help="video file, rtsp:// or http:// stream")
    ap.add_argument("--camera", help="camera resource name, e.g. lrn:gov:us-st-06.dot:video:camera/CAM01")
    ap.add_argument("--agency", help="owning agency id, e.g. us-st-06.dot")
    ap.add_argument("--label", default="", help="human name shown in the console, e.g. 'I-110 S at Avenue 26'")
    ap.add_argument("--core", default="http://localhost:8080")
    ap.add_argument("--analyze-fps", type=float, default=None)
    ap.add_argument("--confirm-seconds", type=float, default=None, help="how long an object must be still before alerting")
    ap.add_argument("--roi", default=None, help="shoulder region: x,y,w,h fractions, or a JSON polygon / list of polygons")
    ap.add_argument("--mask", default=None, help="JSON list of [x0,y0,x1,y1] fraction rectangles to ignore (burned-in overlays)")
    ap.add_argument("--min-area", type=int, default=None, help="minimum blob area in analyzed-frame pixels")
    ap.add_argument("--debug-dir", default=None, help="write one frame with the analyzed region tinted, to check config")
    ap.add_argument("--heartbeat-seconds", type=float, default=1.0)
    ap.add_argument("--fast", action="store_true", help="do not pace file playback in real time")
    ap.add_argument("--once", action="store_true", help="play the file once instead of looping")
    ap.add_argument("--probe", action="store_true", help="flag detections as synthetic probes (never shown to operators)")
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args(argv)
    cfg: dict = {}
    if args.config:
        if not args.camera_id:
            ap.error("--config needs --camera-id")
        cfg = load_camera_config(args.config, args.camera_id)
    # Command line beats config beats defaults.
    args.source = args.source or cfg.get("source")
    args.camera = args.camera or cfg.get("camera")
    args.agency = args.agency or cfg.get("agency")
    args.label = args.label or cfg.get("label", "")
    args.analyze_fps = args.analyze_fps if args.analyze_fps is not None else float(cfg.get("analyze_fps", 5.0))
    args.confirm_seconds = args.confirm_seconds if args.confirm_seconds is not None else float(cfg.get("confirm_seconds", 3.0))
    args.roi = args.roi if args.roi is not None else json.dumps(cfg["roi"]) if "roi" in cfg else "0,0.70,1,0.14"
    args.mask = json.loads(args.mask) if args.mask is not None else cfg.get("mask", [])
    args.min_area = args.min_area if args.min_area is not None else int(cfg.get("min_area", 350))
    for req in ("source", "camera", "agency"):
        if not getattr(args, req):
            ap.error(f"--{req} is required (or provide it via --config/--camera-id)")
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(name)s %(levelname)s %(message)s", stream=sys.stderr)
    return run(args)


if __name__ == "__main__":
    sys.exit(main())

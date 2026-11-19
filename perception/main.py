"""Perception worker: one camera in, candidate detections and heartbeats out.

    python3 main.py --source ../data/cam01.mp4 --camera lrn:gov:us-st-06.dot:video:camera/CAM01 \
        --agency us-st-06.dot --core http://localhost:8080

A trace ID is minted when a frame enters the system and travels with the
detection to the core and on to the console, so one alert can be followed
through every hop.
"""
from __future__ import annotations

import argparse
import logging
import sys
import time
from datetime import timedelta

from adapters import FeedAdapter, utcnow
from detector import StationaryObjectDetector
from emitter import EventCoreClient, new_trace_id

log = logging.getLogger("perception")


def run(args: argparse.Namespace) -> int:
    client = EventCoreClient(args.core)
    adapter = FeedAdapter(args.source, analyze_fps=args.analyze_fps, realtime=not args.fast, loop=not args.once)
    roi = tuple(float(v) for v in args.roi.split(","))
    det = StationaryObjectDetector(roi=roi, confirm_seconds=args.confirm_seconds)
    last_hb = 0.0
    frames_since_hb = 0
    emitted = 0
    log.info("camera=%s agency=%s source=%s model=%s analyze_fps=%s probe=%s",
             args.camera, args.agency, args.source, det.__class__.__name__, args.analyze_fps, args.probe)
    # Live feeds are analyzed on the wall clock. Replays (--fast) run on the
    # clip's own clock, so confirmation windows mean the same thing either way.
    t0 = time.monotonic()
    try:
        for frame in adapter.frames():
            trace_id = new_trace_id()
            t = frame.media_time if args.fast else time.monotonic() - t0
            cands = det.process(t, frame.image)
            frames_since_hb += 1
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
            now = time.monotonic()
            if not args.probe and now - last_hb >= args.heartbeat_seconds:
                fps = frames_since_hb / (now - last_hb) if last_hb else args.analyze_fps
                ok = client.heartbeat(args.camera, args.agency, frame.captured_at, fps, det.health.frozen,
                                      det.health.scene_change, args.source)
                if det.health.frozen:
                    log.warning("feed appears FROZEN (frame delta %.2f)", det.health.frame_delta)
                if not ok:
                    log.warning("heartbeat failed: event core unreachable at %s", args.core)
                last_hb, frames_since_hb = now, 0
    except KeyboardInterrupt:
        pass
    log.info("done: %d candidates emitted", emitted)
    return 0 if (not args.probe or emitted) else 2


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--source", required=True, help="video file, rtsp:// or http:// stream")
    ap.add_argument("--camera", required=True, help="camera resource name, e.g. lrn:gov:us-st-06.dot:video:camera/CAM01")
    ap.add_argument("--agency", required=True, help="owning agency id, e.g. us-st-06.dot")
    ap.add_argument("--core", default="http://localhost:8080")
    ap.add_argument("--analyze-fps", type=float, default=5.0)
    ap.add_argument("--confirm-seconds", type=float, default=3.0, help="how long an object must be still before alerting")
    ap.add_argument("--roi", default="0,0.70,1,0.14", help="shoulder region x,y,w,h as fractions of the frame")
    ap.add_argument("--heartbeat-seconds", type=float, default=1.0)
    ap.add_argument("--fast", action="store_true", help="do not pace file playback in real time")
    ap.add_argument("--once", action="store_true", help="play the file once instead of looping")
    ap.add_argument("--probe", action="store_true", help="flag detections as synthetic probes (never shown to operators)")
    ap.add_argument("-v", "--verbose", action="store_true")
    args = ap.parse_args(argv)
    logging.basicConfig(level=logging.DEBUG if args.verbose else logging.INFO,
                        format="%(asctime)s %(name)s %(levelname)s %(message)s", stream=sys.stderr)
    return run(args)


if __name__ == "__main__":
    sys.exit(main())

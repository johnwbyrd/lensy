"""HTTP client for the event core. Standard library only.

Detections are delivered at least once with retry; the core deduplicates on
dedupe_key, so a retry can never create a second alert. Heartbeats are
best-effort: a missed heartbeat is itself the signal.
"""
from __future__ import annotations

import base64
import hashlib
import json
import time
import urllib.error
import urllib.request
import uuid
from datetime import datetime

import cv2
import numpy as np


def iso(ts: datetime) -> str:
    return ts.isoformat().replace("+00:00", "Z")


class EventCoreClient:
    def __init__(self, base_url: str, timeout: float = 3.0):
        self.base = base_url.rstrip("/")
        self.timeout = timeout

    def _post(self, path: str, body: dict) -> tuple[int, dict]:
        data = json.dumps(body).encode()
        req = urllib.request.Request(self.base + path, data=data, headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as r:
                return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            return e.code, json.loads(e.read() or b"{}")

    def heartbeat(self, camera: str, agency: str, frame_ts: datetime, fps: float, frozen: bool, scene_change: bool, source: str) -> bool:
        try:
            code, _ = self._post("/v1/feeds/heartbeat", {
                "camera": camera, "agency": agency, "frame_ts": iso(frame_ts), "fps": round(fps, 2),
                "frozen": frozen, "scene_change": scene_change, "source": source,
            })
            return code < 300
        except (urllib.error.URLError, TimeoutError, OSError):
            return False

    def detection(self, *, trace_id: str, camera: str, agency: str, cand, image: np.ndarray,
                  captured_at: datetime, onset_estimate: datetime, confirmed_at: datetime, emitted_at: datetime,
                  probe: bool = False, retries: int = 5) -> dict:
        ok, jpg = cv2.imencode(".jpg", image, [int(cv2.IMWRITE_JPEG_QUALITY), 82])
        if not ok:
            raise RuntimeError("jpeg encode failed")
        raw = jpg.tobytes()
        body = {
            "trace_id": trace_id, "camera": camera, "agency": agency, "hazard": cand.hazard,
            "confidence": cand.confidence, "model_version": cand.model_version,
            "dedupe_key": f"{camera}:{cand.model_version}:{cand.track_id}:{iso(onset_estimate)}",
            "probe": probe, "captured_at": iso(captured_at), "onset_estimate": iso(onset_estimate),
            "confirmed_at": iso(confirmed_at), "emitted_at": iso(emitted_at),
            "bbox": list(cand.bbox), "stationary_seconds": cand.stationary_seconds,
            "evidence": {"image_b64": base64.b64encode(raw).decode(), "sha256": hashlib.sha256(raw).hexdigest()},
        }
        delay = 0.2
        last: dict = {}
        for _ in range(retries):
            try:
                code, last = self._post("/v1/detections", body)
                if code < 500:
                    last["http"] = code
                    return last
            except (urllib.error.URLError, TimeoutError, OSError) as e:
                last = {"error": str(e)}
            time.sleep(delay)
            delay = min(3.0, delay * 2)
        last["http"] = 0
        return last


def new_trace_id() -> str:
    return uuid.uuid4().hex

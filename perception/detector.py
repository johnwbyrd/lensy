"""Stationary-object detector: an explainable stand-in for the Specific AI model.

Dual-background method. A slow background model learns the empty road. A fast
model follows whatever is there now. Pixels that differ from the slow model but
match the fast one are "something new that has stopped moving". Blobs of such
pixels inside the shoulder region of interest that persist for the
confirmation window become a disabled-vehicle candidate.

Every candidate carries the model version, the onset estimate (first frame the
blob was seen), the confirmation time and a bounding box, so an alert can be
traced back to its frames.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional

import cv2
import numpy as np

MODEL_VERSION = "stationary-dualbg-v0.1"


@dataclass
class Track:
    track_id: int
    bbox: tuple[int, int, int, int]
    first_seen: float
    last_seen: float
    frames: int = 1
    emitted: bool = False


@dataclass
class Candidate:
    """A confirmed hazard. Times are seconds on the caller's timeline: in live
    operation that is wall clock, in replay it is the clip's own clock, so a
    clip replayed at 50x still needs the full confirmation window of footage."""
    hazard: str
    confidence: float
    bbox: tuple[int, int, int, int]
    onset_t: float
    confirmed_t: float
    stationary_seconds: float
    track_id: int
    model_version: str = MODEL_VERSION


@dataclass
class FeedHealth:
    frozen: bool = False
    scene_change: bool = False
    frame_delta: float = 0.0


@dataclass
class StationaryObjectDetector:
    roi: tuple[float, float, float, float] = (0.0, 0.70, 1.0, 0.14)   # x, y, w, h as fractions
    confirm_seconds: float = 3.0
    min_area: int = 350
    long_alpha: float = 0.003
    short_alpha: float = 0.25
    diff_threshold: int = 28
    still_threshold: int = 12
    match_px: float = 25.0
    track_gap_seconds: float = 1.0
    freeze_seconds: float = 2.0
    freeze_delta: float = 0.6
    scene_change_delta: float = 45.0
    _long: Optional[np.ndarray] = field(default=None, repr=False)
    _short: Optional[np.ndarray] = field(default=None, repr=False)
    _prev: Optional[np.ndarray] = field(default=None, repr=False)
    _n: int = 0
    _tracks: dict = field(default_factory=dict)
    _next_id: int = 1
    _still_since: Optional[float] = None
    health: FeedHealth = field(default_factory=FeedHealth)

    def process(self, ts: float, image: np.ndarray) -> list[Candidate]:
        """Analyze one frame captured at time ts (seconds)."""
        gray = cv2.GaussianBlur(cv2.cvtColor(image, cv2.COLOR_BGR2GRAY), (5, 5), 0).astype(np.float32)
        self._n += 1
        if self._long is None:
            self._long = gray.copy()
            self._short = gray.copy()
            self._prev = gray.copy()
            return []

        # Feed health: frozen encoder or moved camera. Both are blindness.
        delta = float(np.mean(np.abs(gray - self._prev)))
        self.health.frame_delta = delta
        self.health.scene_change = delta > self.scene_change_delta
        if delta < self.freeze_delta:
            self._still_since = ts if self._still_since is None else self._still_since
            self.health.frozen = (ts - self._still_since) >= self.freeze_seconds
        else:
            self._still_since = None
            self.health.frozen = False
        self._prev = gray

        h, w = gray.shape
        rx, ry, rw, rh = self.roi
        x0, y0, x1, y1 = int(rx * w), int(ry * h), int((rx + rw) * w), int((ry + rh) * h)

        far_from_long = np.abs(gray - self._long) > self.diff_threshold
        close_to_short = np.abs(gray - self._short) < self.still_threshold
        mask = (far_from_long & close_to_short).astype(np.uint8) * 255
        roi_mask = np.zeros_like(mask)
        roi_mask[y0:y1, x0:x1] = 255
        mask = cv2.bitwise_and(mask, roi_mask)
        kernel = cv2.getStructuringElement(cv2.MORPH_RECT, (7, 7))
        mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, kernel)
        mask = cv2.morphologyEx(mask, cv2.MORPH_CLOSE, kernel)

        # Update backgrounds. The slow model is bootstrapped quickly, then
        # frozen under confirmed stationary blobs so a parked car is not
        # quietly absorbed into "normal".
        alpha_long = max(self.long_alpha, 1.0 / self._n) if self._n < 60 else self.long_alpha
        hold = mask > 0
        upd = self._long * (1 - alpha_long) + gray * alpha_long
        self._long = np.where(hold, self._long, upd)
        self._short = self._short * (1 - self.short_alpha) + gray * self.short_alpha

        contours, _ = cv2.findContours(mask, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
        blobs = []
        for c in contours:
            if cv2.contourArea(c) < self.min_area:
                continue
            bx, by, bw, bh = cv2.boundingRect(c)
            blobs.append((bx, by, bw, bh))

        # Nearest-centroid tracking.
        unmatched = set(self._tracks.keys())
        for b in blobs:
            cx, cy = b[0] + b[2] / 2, b[1] + b[3] / 2
            best, best_d = None, self.match_px
            for tid in unmatched:
                t = self._tracks[tid]
                tx, ty = t.bbox[0] + t.bbox[2] / 2, t.bbox[1] + t.bbox[3] / 2
                d = ((cx - tx) ** 2 + (cy - ty) ** 2) ** 0.5
                if d < best_d:
                    best, best_d = tid, d
            if best is None:
                self._tracks[self._next_id] = Track(self._next_id, b, ts, ts)
                self._next_id += 1
            else:
                t = self._tracks[best]
                t.bbox, t.last_seen, t.frames = b, ts, t.frames + 1
                unmatched.discard(best)
        for tid in list(unmatched):
            if (ts - self._tracks[tid].last_seen) > self.track_gap_seconds:
                del self._tracks[tid]

        out = []
        for t in self._tracks.values():
            dur = t.last_seen - t.first_seen
            if not t.emitted and dur >= self.confirm_seconds and t.frames >= 3:
                t.emitted = True
                area = t.bbox[2] * t.bbox[3]
                conf = min(0.98, 0.55 + 0.25 * min(1.0, area / 2500.0) + 0.18 * min(1.0, dur / (2 * self.confirm_seconds)))
                out.append(Candidate("disabled_vehicle", round(conf, 3), t.bbox, t.first_seen, ts, round(dur, 2), t.track_id))
        return out

    @staticmethod
    def annotate(image: np.ndarray, cand: Candidate, camera: str) -> np.ndarray:
        img = image.copy()
        x, y, w, h = cand.bbox
        cv2.rectangle(img, (x, y), (x + w, y + h), (0, 200, 255), 2)
        label = f"{cand.hazard} {cand.confidence:.2f} stationary {cand.stationary_seconds:.0f}s"
        (tw, th), _ = cv2.getTextSize(label, cv2.FONT_HERSHEY_SIMPLEX, 0.45, 1)
        ly = max(th + 8, y - 6)
        cv2.rectangle(img, (x - 2, ly - th - 4), (x + tw + 4, ly + 4), (20, 20, 20), -1)
        cv2.putText(img, label, (x, ly), cv2.FONT_HERSHEY_SIMPLEX, 0.45, (0, 200, 255), 1)
        cv2.putText(img, f"{camera} {cand.model_version}", (8, img.shape[0] - 8), cv2.FONT_HERSHEY_SIMPLEX, 0.4, (255, 255, 255), 1)
        return img

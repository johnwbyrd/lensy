"""Feed adapters: turn a source into timestamped frames plus a health signal.

One contract above, source differences below. A file, an RTSP URL and an HTTP
MJPEG stream all come through the same Frame iterator. Timestamps are taken on
our clock at ingest because camera clocks cannot be trusted.
"""
from __future__ import annotations

import time
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Iterator

import cv2
import numpy as np


@dataclass
class Frame:
    seq: int
    captured_at: datetime        # ingest wall clock (UTC)
    media_time: float            # seconds on the source's own timeline
    image: np.ndarray
    source_fps: float


def utcnow() -> datetime:
    return datetime.now(timezone.utc)


class FeedAdapter:
    """OpenCV-backed adapter for files and network streams.

    analyze_fps: how many frames per second to hand to perception.
    realtime: pace file playback at the file's frame rate (streams are already realtime).
    loop: restart a file when it ends (demo convenience).
    """

    def __init__(self, source: str, analyze_fps: float = 5.0, realtime: bool = True, loop: bool = True):
        self.source = source
        self.analyze_fps = analyze_fps
        self.realtime = realtime
        self.loop = loop
        self.is_file = not (source.startswith("rtsp://") or source.startswith("http://") or source.startswith("https://"))
        self.cap: cv2.VideoCapture | None = None
        self.reconnects = 0

    def _open(self, attempts: int = 1) -> None:
        """Open the source. Network streams get several attempts with backoff:
        a public HLS origin that is slow to answer is a normal condition, and
        the right response is to keep trying while the core, seeing no
        heartbeat, reports the camera as not watched."""
        delay = 1.0
        for i in range(attempts):
            self.cap = cv2.VideoCapture(self.source)
            if self.cap.isOpened():
                return
            if i + 1 < attempts:
                time.sleep(delay)
                delay = min(30.0, delay * 2)
        raise RuntimeError(f"cannot open source {self.source} after {attempts} attempt(s)")

    def frames(self) -> Iterator[Frame]:
        self._open(attempts=1 if self.is_file else 8)
        assert self.cap is not None
        src_fps = self.cap.get(cv2.CAP_PROP_FPS) or 15.0
        stride = max(1, int(round(src_fps / self.analyze_fps)))
        seq = 0
        raw = 0
        start = time.monotonic()
        while True:
            ok, img = self.cap.read()
            if not ok:
                if self.is_file and self.loop:
                    self.cap.set(cv2.CAP_PROP_POS_FRAMES, 0)
                    start = time.monotonic()
                    raw = 0
                    continue
                if self.is_file:
                    return
                # Network stream dropped: back off and reconnect. The caller
                # keeps heartbeating so the core sees the gap as a stale feed.
                self.reconnects += 1
                time.sleep(min(10, 0.5 * 2 ** min(self.reconnects, 5)))
                try:
                    self._open()
                except RuntimeError:
                    pass
                continue
            raw += 1
            if self.is_file and self.realtime:
                due = start + raw / src_fps
                delay = due - time.monotonic()
                if delay > 0:
                    time.sleep(delay)
            if raw % stride:
                continue
            seq += 1
            # Streams report their own media clock; files are paced by frame count.
            pos_ms = self.cap.get(cv2.CAP_PROP_POS_MSEC) if not self.is_file else 0.0
            media_time = pos_ms / 1000.0 if pos_ms and pos_ms > 0 else raw / src_fps
            yield Frame(seq=seq, captured_at=utcnow(), media_time=media_time, image=img, source_fps=src_fps)

"""Synthetic traffic video for demos and tests.

Real agency footage cannot be redistributed, so the prototype ships with a
generator: a four-lane roadway with a shoulder, cars at varied speeds, and one
car that pulls onto the shoulder and stops. Optional freeze simulates a camera
whose encoder has hung, which should be reported as blindness, not quiet.
"""
from __future__ import annotations

import argparse
import random

import cv2
import numpy as np

W, H = 640, 360
ROAD_TOP, ROAD_BOTTOM = 60, 300          # roadway band
SHOULDER_TOP = 255                        # shoulder: SHOULDER_TOP..ROAD_BOTTOM
LANES = [90, 135, 185, 230]               # lane centre y
SHOULDER_Y = (SHOULDER_TOP + ROAD_BOTTOM) // 2


def draw_scene(t: float, cars: list[dict], rng: random.Random) -> np.ndarray:
    img = np.zeros((H, W, 3), np.uint8)
    img[:] = (70, 110, 60)                                   # grass
    cv2.rectangle(img, (0, ROAD_TOP), (W, ROAD_BOTTOM), (85, 85, 85), -1)      # asphalt
    cv2.rectangle(img, (0, SHOULDER_TOP), (W, ROAD_BOTTOM), (120, 120, 120), -1)  # shoulder
    cv2.line(img, (0, SHOULDER_TOP), (W, SHOULDER_TOP), (240, 240, 240), 2)
    cv2.line(img, (0, 160), (W, 160), (200, 190, 60), 2)    # median
    for y in (112, 207):
        for x in range(0, W, 40):
            cv2.line(img, (x, y), (x + 20, y), (230, 230, 230), 2)
    for c in cars:
        x, y = int(c["x"]), int(c["y"])
        w, h = c["w"], c["h"]
        cv2.rectangle(img, (x, y - h // 2), (x + w, y + h // 2), c["color"], -1)
        cv2.rectangle(img, (x + w // 4, y - h // 2 + 3), (x + 3 * w // 4, y + h // 2 - 3), (40, 40, 40), -1)
    # Sensor noise and slow illumination drift so background models have to work.
    noise = np.random.default_rng(int(t * 1000) % (2**32)).normal(0, 3, img.shape)
    img = np.clip(img.astype(np.float32) + noise + 6 * np.sin(t / 7.0), 0, 255).astype(np.uint8)
    cv2.putText(img, f"SYNTH CAM  t={t:6.2f}s", (8, 20), cv2.FONT_HERSHEY_SIMPLEX, 0.5, (255, 255, 255), 1)
    return img


def generate(path: str, seconds: float = 40.0, fps: int = 15, stop_at: float | None = 10.0,
             freeze_at: float | None = None, seed: int = 7) -> None:
    rng = random.Random(seed)
    palette = [(200, 60, 60), (60, 60, 200), (220, 220, 220), (30, 30, 30), (60, 180, 200), (180, 120, 40)]
    cars: list[dict] = []
    for _ in range(9):
        lane = rng.choice(range(4))
        direction = -1 if lane < 2 else 1
        cars.append({
            "x": rng.uniform(0, W), "y": LANES[lane], "w": rng.randint(34, 50), "h": 20,
            "v": direction * rng.uniform(70, 150), "color": rng.choice(palette), "lane": lane,
        })
    disabled = None
    if stop_at is not None:
        # Work backwards from where it should stop (mid-frame) so the car is on
        # screen when it pulls over, whatever stop_at is: 3 s of deceleration
        # covers ~165 px, and before that it cruises at 110 px/s.
        x_stop = W * 0.55
        x0 = x_stop - 165.0 - 110.0 * max(0.0, stop_at - 3.0)
        disabled = {"x": x0, "y": float(LANES[3]), "w": 46, "h": 20, "v": 110.0,
                    "color": (40, 200, 240), "lane": 3}
        cars.append(disabled)

    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    out = cv2.VideoWriter(path, fourcc, fps, (W, H))
    if not out.isOpened():
        path = path.rsplit(".", 1)[0] + ".avi"
        out = cv2.VideoWriter(path, cv2.VideoWriter_fourcc(*"MJPG"), fps, (W, H))
    n = int(seconds * fps)
    dt = 1.0 / fps
    frozen = None
    for i in range(n):
        t = i * dt
        if freeze_at is not None and t >= freeze_at:
            if frozen is None:
                frozen = draw_scene(t, cars, rng)
            out.write(frozen)
            continue
        for c in cars:
            if c is disabled:
                # Approach, drift to the shoulder, decelerate, stop; then stay.
                if t < stop_at - 3:
                    c["x"] += c["v"] * dt
                elif t < stop_at:
                    frac = (t - (stop_at - 3)) / 3.0
                    c["y"] = LANES[3] + (SHOULDER_Y - LANES[3]) * frac
                    c["v"] = 110.0 * (1 - frac)
                    c["x"] += c["v"] * dt
                else:
                    c["v"] = 0.0
                continue
            c["x"] += c["v"] * dt
            if c["x"] > W + 60:
                c["x"] = -60
            if c["x"] < -60:
                c["x"] = W + 60
        out.write(draw_scene(t, cars, rng))
    out.release()
    print(f"wrote {path}: {seconds}s @ {fps}fps, stop_at={stop_at}, freeze_at={freeze_at}")


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("path")
    ap.add_argument("--seconds", type=float, default=40)
    ap.add_argument("--fps", type=int, default=15)
    ap.add_argument("--stop-at", type=float, default=10.0, help="when the car stops on the shoulder; negative for never")
    ap.add_argument("--freeze-at", type=float, default=None, help="freeze the feed from this time onward")
    a = ap.parse_args()
    generate(a.path, a.seconds, a.fps, None if a.stop_at < 0 else a.stop_at, a.freeze_at)

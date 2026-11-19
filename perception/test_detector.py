"""Replay-style tests: labeled synthetic clips through the detector.

This is the seed of the replay harness described in the design: every change
to perception must still find the known hazard and must not alarm on the
clip with no hazard.
"""
import os
import tempfile
import unittest

import cv2

from detector import StationaryObjectDetector
from synth import generate, SHOULDER_TOP, ROAD_BOTTOM


def run_clip(path, confirm=2.0):
    cap = cv2.VideoCapture(path)
    fps = cap.get(cv2.CAP_PROP_FPS) or 15
    det = StationaryObjectDetector(confirm_seconds=confirm)
    t = 0.0
    cands, frozen_at = [], None
    i = 0
    while True:
        ok, img = cap.read()
        if not ok:
            break
        if i % 3 == 0:  # analyze at 5 fps
            cands += det.process(t, img)
            if det.health.frozen and frozen_at is None:
                frozen_at = t
        t += 1 / fps
        i += 1
    return cands, frozen_at, det


class DetectorTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp()

    def test_finds_stopped_car_on_shoulder_once(self):
        p = os.path.join(self.dir, "stop.mp4")
        generate(p, seconds=14, fps=15, stop_at=5.0)
        cands, frozen_at, _ = run_clip(p)
        self.assertEqual(len(cands), 1, f"expected exactly one candidate, got {cands}")
        c = cands[0]
        self.assertEqual(c.hazard, "disabled_vehicle")
        y_mid = c.bbox[1] + c.bbox[3] / 2
        self.assertTrue(SHOULDER_TOP - 10 <= y_mid <= ROAD_BOTTOM + 10, f"bbox not on shoulder: {c.bbox}")
        self.assertGreaterEqual(c.stationary_seconds, 2.0)
        self.assertLess(c.confirmed_t - c.onset_t, 6.0)
        self.assertAlmostEqual(c.onset_t, 5.0, delta=1.5, msg="onset should be close to when the car stopped")
        self.assertIsNone(frozen_at)

    def test_long_clip_stop_at_ten_seconds(self):
        # The demo clip: the car must still be on screen when it stops.
        p = os.path.join(self.dir, "demo.mp4")
        generate(p, seconds=20, fps=15, stop_at=10.0)
        cands, _, _ = run_clip(p, confirm=3.0)
        self.assertEqual(len(cands), 1, f"expected one candidate, got {cands}")
        self.assertAlmostEqual(cands[0].onset_t, 10.0, delta=1.5)

    def test_moving_traffic_does_not_alarm(self):
        p = os.path.join(self.dir, "clear.mp4")
        generate(p, seconds=14, fps=15, stop_at=None)
        cands, _, _ = run_clip(p)
        self.assertEqual(cands, [])

    def test_frozen_feed_is_reported(self):
        p = os.path.join(self.dir, "frozen.mp4")
        generate(p, seconds=10, fps=15, stop_at=None, freeze_at=4.0)
        _, frozen_at, det = run_clip(p)
        self.assertIsNotNone(frozen_at, "freeze not detected")
        self.assertTrue(det.health.frozen)


if __name__ == "__main__":
    unittest.main()


class ConfigTests(unittest.TestCase):
    def test_polygon_roi_and_mask_rasterize(self):
        import numpy as np
        det = StationaryObjectDetector(roi=[[[0.0, 0.5], [1.0, 0.5], [1.0, 1.0], [0.0, 1.0]]], mask=[[0.0, 0.8, 0.5, 1.0]])
        m = det._region_mask(100, 200)
        self.assertEqual(m[25, 100], 0)      # above the polygon
        self.assertEqual(m[60, 100], 255)    # inside
        self.assertEqual(m[90, 20], 0)       # inside polygon but masked overlay
        self.assertEqual(m[90, 150], 255)    # inside polygon, outside mask
        self.assertGreater(int(np.count_nonzero(m)), 0)

    def test_cameras_json_is_well_formed(self):
        import json
        cfg = json.load(open(os.path.join(os.path.dirname(__file__), "cameras.json")))
        ids = set()
        for c in cfg["cameras"]:
            for key in ("id", "camera", "agency", "label", "source", "roi", "confirm_seconds"):
                self.assertIn(key, c, f"{c.get('id')} missing {key}")
            self.assertNotIn(c["id"], ids); ids.add(c["id"])
            self.assertTrue(c["camera"].endswith("/" + c["id"]))
            for poly in c["roi"]:
                self.assertGreaterEqual(len(poly), 3)
                for x, y in poly:
                    self.assertTrue(0 <= x <= 1 and 0 <= y <= 1)
            StationaryObjectDetector(roi=c["roi"], mask=c.get("mask", []))._region_mask(72, 128)

    def test_downscaled_bbox_maps_back_to_full_frame(self):
        p = os.path.join(tempfile.mkdtemp(), "hd.mp4")
        generate(p, seconds=12, fps=15, stop_at=4.0)
        cap = cv2.VideoCapture(p)
        det = StationaryObjectDetector(confirm_seconds=2.0, proc_width=320, min_area=60)   # analyze at half size
        t, cands, i = 0.0, [], 0
        while True:
            ok, img = cap.read()
            if not ok:
                break
            if i % 3 == 0:
                cands += det.process(t, img)
            t += 1 / 15; i += 1
        self.assertEqual(len(cands), 1)
        x, y, w, h = cands[0].bbox
        self.assertTrue(300 <= x <= 420 and 250 <= y <= 290, f"bbox not in full-frame coordinates: {cands[0].bbox}")

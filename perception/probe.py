"""Black-box probe: replay a known hazard clip through the production path.

Every interval, the probe runs a recorded hazard clip through the same
detector, emitter and event core an operator alert would use, flagged
probe=true so it never reaches a dispatcher, training data or mission counts.
If the clip stops producing a committed probe event, the alert path is broken
and someone should be paged. A quiet highway and a blind system look the same
from the outside; this is how we tell them apart.
"""
from __future__ import annotations

import argparse
import logging
import sys
import time

from main import main as run_worker

log = logging.getLogger("probe")


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--clip", required=True, help="recorded hazard clip")
    ap.add_argument("--core", default="http://localhost:8080")
    ap.add_argument("--interval", type=float, default=60.0, help="seconds between probes")
    ap.add_argument("--agency", default="us-st-06.dot")
    ap.add_argument("--camera", default="lrn:gov:us-st-06.dot:video:camera/PROBE")
    ap.add_argument("--confirm-seconds", type=float, default=3.0)
    a = ap.parse_args(argv)
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s", stream=sys.stderr)
    failures = 0
    while True:
        t0 = time.monotonic()
        rc = run_worker(["--source", a.clip, "--camera", a.camera, "--agency", a.agency, "--core", a.core,
                         "--confirm-seconds", str(a.confirm_seconds), "--fast", "--once", "--probe"])
        if rc == 0:
            failures = 0
            log.info("probe OK in %.1fs", time.monotonic() - t0)
        else:
            failures += 1
            log.error("PROBE FAILED (%d consecutive): alert path did not commit a probe detection. PAGE ON-CALL.", failures)
        time.sleep(max(0.0, a.interval - (time.monotonic() - t0)))


if __name__ == "__main__":
    sys.exit(main())

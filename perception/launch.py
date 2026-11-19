"""Start one perception worker per camera in a config file.

    python3 launch.py --config cameras.json --core http://localhost:8080

Each camera is its own process, so a decoder that hangs on one stream cannot
stall another. Workers that exit are restarted with backoff; the core sees
the gap as a stale feed either way.
"""
from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--config", default=os.path.join(HERE, "cameras.json"))
    ap.add_argument("--core", default="http://localhost:8080")
    ap.add_argument("--only", nargs="*", help="camera ids to start (default: all enabled)")
    ap.add_argument("--log-dir", default=None)
    ap.add_argument("--debug-dir", default=None, help="write an ROI check image per camera")
    ap.add_argument("--pid-file", default=None, help="append worker pids here so a supervisor can clean up")
    a = ap.parse_args(argv)
    cfg = json.load(open(a.config))
    cams = [c for c in cfg["cameras"] if c.get("enabled", True) and (not a.only or c["id"] in a.only)]
    if not cams:
        print("no cameras selected", file=sys.stderr)
        return 1
    procs: dict[str, tuple[subprocess.Popen, float, int]] = {}
    stop = False

    def start(c: dict) -> None:
        cmd = [sys.executable, os.path.join(HERE, "main.py"), "--config", a.config, "--camera-id", c["id"], "--core", a.core]
        if a.debug_dir:
            cmd += ["--debug-dir", a.debug_dir]
        out = open(os.path.join(a.log_dir, f"{c['id']}.log"), "a") if a.log_dir else None
        p = subprocess.Popen(cmd, stdout=out, stderr=subprocess.STDOUT if out else None)
        procs[c["id"]] = (p, time.monotonic(), procs.get(c["id"], (None, 0, 0))[2])
        if a.pid_file:
            with open(a.pid_file, "a") as f:
                f.write(f"{p.pid}\n")
        print(f"started {c['id']} pid {p.pid}: {c.get('label', '')}", flush=True)

    def on_signal(*_):
        nonlocal stop
        stop = True
    signal.signal(signal.SIGINT, on_signal)
    signal.signal(signal.SIGTERM, on_signal)
    if a.log_dir:
        os.makedirs(a.log_dir, exist_ok=True)
    for c in cams:
        start(c)
    by_id = {c["id"]: c for c in cams}
    while not stop:
        time.sleep(1)
        for cid, (p, started, restarts) in list(procs.items()):
            if p.poll() is not None:
                delay = min(60, 2 ** min(restarts, 6))
                print(f"{cid} exited with {p.returncode}; restarting in {delay}s", flush=True)
                time.sleep(delay)
                procs[cid] = (p, started, restarts + 1)
                start(by_id[cid])
    for cid, (p, _, _) in procs.items():
        p.terminate()
    for cid, (p, _, _) in procs.items():
        try:
            p.wait(5)
        except subprocess.TimeoutExpired:
            p.kill()
    return 0


if __name__ == "__main__":
    sys.exit(main())

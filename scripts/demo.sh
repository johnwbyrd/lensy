#!/usr/bin/env bash
# One-command demo of the alert path on synthetic video.
#
#   scripts/demo.sh            synthetic: the core, two generated cameras and the probe
#   scripts/demo.sh live       live: the core, the Caltrans cameras in perception/cameras.json,
#                              plus the synthetic CAM01 (guaranteed hazard) and the probe
#   scripts/demo.sh stop       stop everything started by this script
#
# Then open http://localhost:8080 and enter an operator id.
# Synthetic CAM01 shows a car pulling onto the shoulder at ~10 s; the clip loops.
# Synthetic CAM02 freezes at 25 s so you can watch the feed health panel turn amber.
# Live cameras use a 60 s confirmation window; expect a stopped vehicle within
# an hour on a busy corridor, and expect some of them to be false alarms.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA="$ROOT/data"
PIDS="$DATA/demo.pids"
PORT="${PORT:-8080}"

stop() {
  if [[ -f "$PIDS" ]]; then
    while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$PIDS"
    # launch.py records its worker pids; make sure none outlive it.
    if [[ -f "$DATA/workers.pids" ]]; then
      while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$DATA/workers.pids"
      rm -f "$DATA/workers.pids"
    fi
    rm -f "$PIDS"
    echo "stopped"
  fi
}
MODE="${1:-synthetic}"
if [[ "$MODE" == "stop" ]]; then stop; exit 0; fi
stop
mkdir -p "$DATA"

echo "== build console"
( cd "$ROOT/console" && if [[ -x node_modules/.bin/tsc ]]; then node_modules/.bin/tsc -p .; else tsc -p .; fi && cp index.html styles.css dist/ )
echo "== build event core"
( cd "$ROOT/eventcore" && go build -o "$ROOT/bin/eventcore" . )
echo "== synthetic video"
[[ -f "$DATA/cam01.mp4" ]]  || python3 "$ROOT/perception/synth.py" "$DATA/cam01.mp4" --seconds 40 --stop-at 10
[[ -f "$DATA/cam02.mp4" ]]  || python3 "$ROOT/perception/synth.py" "$DATA/cam02.mp4" --seconds 40 --stop-at -1 --freeze-at 25
[[ -f "$DATA/probe.mp4" ]]  || python3 "$ROOT/perception/synth.py" "$DATA/probe.mp4" --seconds 12 --stop-at 4

echo "== start event core on :$PORT"
"$ROOT/bin/eventcore" -addr ":$PORT" -data "$DATA" -static "$ROOT/console/dist" -escalate-after 45s > "$DATA/eventcore.log" 2>&1 &
echo $! >> "$PIDS"
for _ in $(seq 1 30); do curl -fsS "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.2; done

echo "== start perception workers ($MODE)"
cd "$ROOT/perception"
python3 main.py --source "$DATA/cam01.mp4" --camera lrn:gov:us-st-06.dot:video:camera/CAM01 --agency us-st-06.dot --label "synthetic clip, hazard at 10 s" --core "http://localhost:$PORT" > "$DATA/cam01.log" 2>&1 &
echo $! >> "$PIDS"
if [[ "$MODE" == "live" ]]; then
  python3 launch.py --config "$ROOT/perception/cameras.json" --core "http://localhost:$PORT" --log-dir "$DATA" --debug-dir "$DATA/roi" --pid-file "$DATA/workers.pids" > "$DATA/launch.log" 2>&1 &
  echo $! >> "$PIDS"
else
  python3 main.py --source "$DATA/cam02.mp4" --camera lrn:gov:us-pl-0644000.police:video:camera/CAM02 --agency us-pl-0644000.police --label "synthetic clip, freezes at 25 s" --core "http://localhost:$PORT" --once > "$DATA/cam02.log" 2>&1 &
  echo $! >> "$PIDS"
fi
python3 probe.py --clip "$DATA/probe.mp4" --core "http://localhost:$PORT" --interval 60 > "$DATA/probe.log" 2>&1 &
echo $! >> "$PIDS"

cat <<MSG

LENS Detect prototype is running.
  console   http://localhost:$PORT
  metrics   http://localhost:$PORT/v1/metrics   (Prometheus text at /metrics)
  event log http://localhost:$PORT/v1/events    (append-only, ndjson)
  logs      $DATA/*.log

Stop with: scripts/demo.sh stop
MSG

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Wire types. These mirror schema/detection.v1.json and are the only contract
// between perception, the event core and the console.

type DetectionIn struct {
	TraceID       string    `json:"trace_id"`
	Camera        string    `json:"camera"`
	Agency        string    `json:"agency"`
	Hazard        string    `json:"hazard"`
	Confidence    float64   `json:"confidence"`
	ModelVersion  string    `json:"model_version"`
	DedupeKey     string    `json:"dedupe_key"`
	Probe         bool      `json:"probe"`
	CapturedAt    time.Time `json:"captured_at"`
	OnsetEstimate time.Time `json:"onset_estimate"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
	EmittedAt     time.Time `json:"emitted_at"`
	BBox          [4]int    `json:"bbox"`
	Stationary    float64   `json:"stationary_seconds"`
	Evidence      struct {
		ImageB64 string `json:"image_b64"`
		SHA256   string `json:"sha256"`
	} `json:"evidence"`
}

type HeartbeatIn struct {
	Camera      string    `json:"camera"`
	Agency      string    `json:"agency"`
	FrameTS     time.Time `json:"frame_ts"`
	FPS         float64   `json:"fps"`
	Frozen      bool      `json:"frozen"`
	SceneChange bool      `json:"scene_change"`
	Source      string    `json:"source"`
}

type ActionIn struct {
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

type RenderedIn struct {
	RenderedAt time.Time `json:"rendered_at"`
}

type KillSwitchIn struct {
	Active   bool   `json:"active"`
	Operator string `json:"operator"`
	Reason   string `json:"reason"`
}

func (c *Core) Routes(staticDir string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/detections", c.handleDetection)
	mux.HandleFunc("POST /v1/feeds/heartbeat", c.handleHeartbeat)
	mux.HandleFunc("POST /v1/alerts/{id}/rendered", c.handleRendered)
	mux.HandleFunc("POST /v1/alerts/{id}/{action}", c.handleAction)
	mux.HandleFunc("POST /v1/agencies/{agency}/killswitch", c.handleKillSwitch)
	mux.HandleFunc("GET /v1/alerts", c.handleAlerts)
	mux.HandleFunc("GET /v1/alerts/{id}", c.handleAlert)
	mux.HandleFunc("GET /v1/feeds", c.handleFeeds)
	mux.HandleFunc("GET /v1/metrics", c.handleMetrics)
	mux.HandleFunc("GET /metrics", c.handlePrometheus)
	mux.HandleFunc("GET /v1/events", c.handleEvents)
	mux.HandleFunc("GET /v1/stream", c.handleStream)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.Handle("GET /evidence/", http.StripPrefix("/evidence/", http.FileServer(http.Dir(filepath.Join(c.dataDir, "evidence")))))
	if staticDir != "" {
		mux.Handle("GET /", spaHandler(staticDir))
	}
	return cors(mux)
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func spaHandler(dir string) http.Handler {
	fs := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Join(dir, filepath.Clean(r.URL.Path))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			fs.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	return dec.Decode(v)
}

// handleDetection is the ingest point for perception. It is idempotent on
// dedupe_key, verifies evidence integrity, commits the candidate, then decides
// whether the alert is presented, suppressed by a kill switch, or routed to the
// probe lane. Perception gets its 201 only after the record is on disk.
func (c *Core) handleDetection(w http.ResponseWriter, r *http.Request) {
	var in DetectionIn
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if in.Camera == "" || in.Agency == "" || in.Hazard == "" || in.ModelVersion == "" {
		writeErr(w, 400, "camera, agency, hazard and model_version are required")
		return
	}
	if in.CapturedAt.IsZero() {
		writeErr(w, 400, "captured_at is required")
		return
	}
	if in.TraceID == "" {
		in.TraceID = newID()
	}
	if in.DedupeKey != "" {
		if id, ok := c.st.LookupDedupe(in.DedupeKey); ok {
			a := c.st.GetAlert(id)
			writeJSON(w, 200, map[string]any{"alert_id": id, "status": a.Status, "trace_id": a.TraceID, "duplicate": true})
			return
		}
	}
	id := newID()
	evidenceURL, sha, err := c.storeEvidence(id, in.Evidence.ImageB64, in.Evidence.SHA256)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	now := time.Now().UTC()
	cand := &Event{
		ID: id, TraceID: in.TraceID, Type: EvDetectionCandidate, TS: now,
		Agency: in.Agency, Resource: in.Camera, Actor: "perception/" + in.ModelVersion,
		Payload: mustJSON(detectionPayload{
			Hazard: in.Hazard, Confidence: in.Confidence, ModelVersion: in.ModelVersion,
			DedupeKey: in.DedupeKey, Probe: in.Probe, CapturedAt: in.CapturedAt,
			OnsetEstimate: in.OnsetEstimate, ConfirmedAt: in.ConfirmedAt, EmittedAt: in.EmittedAt,
			EvidenceURL: evidenceURL, EvidenceSHA: sha, BBox: in.BBox, Stationary: in.Stationary,
		}),
	}
	if err := c.Commit(cand); err != nil {
		writeErr(w, 500, "commit: "+err.Error())
		return
	}
	next := &Event{TraceID: in.TraceID, Agency: in.Agency, Resource: in.Camera, Actor: "eventcore",
		Payload: mustJSON(map[string]any{"alert_id": id})}
	status := "presented"
	switch {
	case in.Probe:
		next.Type = EvProbeReceived
		status = "probe"
	case c.st.KillSwitchActive(in.Agency):
		next.Type = EvAlertSuppressed
		status = "suppressed"
		next.Payload = mustJSON(map[string]any{"alert_id": id, "reason": "kill switch active for agency"})
	default:
		next.Type = EvAlertPresented
	}
	if err := c.Commit(next); err != nil {
		writeErr(w, 500, "commit: "+err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"alert_id": id, "status": status, "trace_id": in.TraceID, "committed_at": cand.TS})
}

// storeEvidence writes the still to disk and checks the hash perception
// claims, so the chain from frame to alert is verifiable later.
func (c *Core) storeEvidence(id, b64, claimed string) (string, string, error) {
	if b64 == "" {
		return "", "", nil
	}
	img, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", "", fmt.Errorf("evidence image_b64 is not valid base64")
	}
	sum := sha256.Sum256(img)
	sha := hex.EncodeToString(sum[:])
	if claimed != "" && !strings.EqualFold(claimed, sha) {
		return "", "", fmt.Errorf("evidence sha256 mismatch: claimed %s computed %s", claimed, sha)
	}
	dir := filepath.Join(c.dataDir, "evidence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	name := id + ".jpg"
	if err := os.WriteFile(filepath.Join(dir, name), img, 0o644); err != nil {
		return "", "", err
	}
	return "/evidence/" + name, sha, nil
}

func (c *Core) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var in HeartbeatIn
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if in.Camera == "" || in.Agency == "" {
		writeErr(w, 400, "camera and agency are required")
		return
	}
	ev := &Event{Type: EvFeedHeartbeat, Agency: in.Agency, Resource: in.Camera, Actor: "perception",
		Payload: mustJSON(heartbeatPayload{FrameTS: in.FrameTS, FPS: in.FPS, Frozen: in.Frozen, SceneChange: in.SceneChange, Source: in.Source})}
	if err := c.Commit(ev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, map[string]string{"status": "ok"})
}

// transitions is the alert state machine. Anything not listed is refused.
var transitions = map[string]map[string]string{
	"acknowledge": {"presented": EvAlertAcknowledged, "escalated": EvAlertAcknowledged},
	"validate":    {"presented": EvAlertValidated, "acknowledged": EvAlertValidated},
	"reject":      {"presented": EvAlertRejected, "acknowledged": EvAlertRejected},
	"dispatch":    {"validated": EvAlertDispatched},
	"close":       {"validated": EvAlertClosed, "dispatched": EvAlertClosed},
}

var errBadTransition = errors.New("transition not allowed")

func (c *Core) handleAction(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	a := c.st.GetAlert(id)
	if a == nil {
		writeErr(w, 404, "no such alert")
		return
	}
	if a.Probe {
		writeErr(w, 403, "probe alerts cannot be acted on")
		return
	}
	var in ActionIn
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if in.Operator == "" {
		writeErr(w, 400, "operator is required: every decision is attributed")
		return
	}
	allowed, ok := transitions[action]
	if !ok {
		writeErr(w, 404, "unknown action "+action)
		return
	}
	evType, ok := allowed[a.Status]
	if !ok {
		writeErr(w, 409, fmt.Sprintf("%v: cannot %s an alert in status %q", errBadTransition, action, a.Status))
		return
	}
	ev := &Event{Type: evType, TraceID: a.TraceID, Agency: a.Agency, Resource: a.Camera, Actor: in.Operator,
		Payload: mustJSON(map[string]any{"alert_id": id, "note": in.Note})}
	if err := c.Commit(ev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c.st.GetAlert(id))
}

// handleRendered closes the latency loop: the browser reports when the alert
// was actually painted for a human, which is the only moment that counts.
func (c *Core) handleRendered(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a := c.st.GetAlert(id)
	if a == nil {
		writeErr(w, 404, "no such alert")
		return
	}
	var in RenderedIn
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if in.RenderedAt.IsZero() {
		in.RenderedAt = time.Now().UTC()
	}
	if _, done := a.Timeline["rendered_at"]; done {
		writeJSON(w, 200, a)
		return
	}
	ev := &Event{Type: EvAlertRendered, TraceID: a.TraceID, Agency: a.Agency, Resource: a.Camera, Actor: "console",
		Payload: mustJSON(map[string]any{"alert_id": id, "rendered_at": in.RenderedAt})}
	if err := c.Commit(ev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c.st.GetAlert(id))
}

func (c *Core) handleKillSwitch(w http.ResponseWriter, r *http.Request) {
	agency := r.PathValue("agency")
	var in KillSwitchIn
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, "bad json: "+err.Error())
		return
	}
	if in.Operator == "" {
		writeErr(w, 400, "operator is required")
		return
	}
	ev := &Event{Type: EvAgencyKillSwitch, Agency: agency, Actor: in.Operator,
		Payload: mustJSON(killSwitchPayload{Active: in.Active, Reason: in.Reason})}
	if err := c.Commit(ev); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, c.st.KillSwitches())
}

func (c *Core) handleAlerts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, c.st.OperatorAlerts(r.URL.Query().Get("all") == "1"))
}

func (c *Core) handleAlert(w http.ResponseWriter, r *http.Request) {
	a := c.st.GetAlert(r.PathValue("id"))
	if a == nil {
		writeErr(w, 404, "no such alert")
		return
	}
	writeJSON(w, 200, a)
}

func (c *Core) handleFeeds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, c.st.FeedList(time.Now().UTC()))
}

func (c *Core) handleMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, c.st.Snapshot(time.Now().UTC()))
}

func (c *Core) handlePrometheus(w http.ResponseWriter, r *http.Request) {
	m := c.st.Snapshot(time.Now().UTC())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "lens_effective_coverage %.4f\n", m.Coverage)
	fmt.Fprintf(w, "lens_probe_last_ok_age_seconds %.1f\n", m.Probes.LastAge)
	fmt.Fprintf(w, "lens_probe_total %d\n", m.Probes.Total)
	for st, n := range m.AlertsByStatus {
		fmt.Fprintf(w, "lens_alerts{status=%q} %d\n", st, n)
	}
	for st, n := range m.FeedsByStatus {
		fmt.Fprintf(w, "lens_feeds{status=%q} %d\n", st, n)
	}
	for name, p := range m.Latency {
		fmt.Fprintf(w, "lens_latency_ms{stage=%q,quantile=\"0.5\"} %.1f\n", name, p.P50)
		fmt.Fprintf(w, "lens_latency_ms{stage=%q,quantile=\"0.95\"} %.1f\n", name, p.P95)
		fmt.Fprintf(w, "lens_latency_ms{stage=%q,quantile=\"0.99\"} %.1f\n", name, p.P99)
	}
	for _, k := range m.KillSwitches {
		v := 0
		if k.Active {
			v = 1
		}
		fmt.Fprintf(w, "lens_kill_switch_active{agency=%q} %d\n", k.Agency, v)
	}
}

// handleEvents exposes the raw log for audit and replay. It is a read of the
// same records the system itself trusts, which is what the Developer Center
// should start as.
func (c *Core) handleEvents(w http.ResponseWriter, r *http.Request) {
	var since uint64
	fmt.Sscan(r.URL.Query().Get("since"), &since)
	trace := r.URL.Query().Get("trace_id")
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	_, err := c.log.Replay(func(ev Event) {
		if ev.Seq <= since {
			return
		}
		if trace != "" && ev.TraceID != trace {
			return
		}
		_ = enc.Encode(ev)
	})
	if err != nil {
		writeErr(w, 500, err.Error())
	}
}

// handleStream is server-sent events: a snapshot, then every committed event.
// A heartbeat every few seconds lets the console detect a stale connection and
// tell the dispatcher to fall back to normal procedure.
func (c *Core) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	ch := c.subscribe()
	defer c.unsubscribe(ch)
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", c.snapshot())
	fl.Flush()
	hb := time.NewTicker(3 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: event\ndata: %s\n\n", b)
			fl.Flush()
		case now := <-hb.C:
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"kind\":\"heartbeat\",\"now\":%q,\"last_seq\":%d}\n\n", now.UTC().Format(time.RFC3339Nano), c.st.Snapshot(now).LastSeq)
			fl.Flush()
		}
	}
}

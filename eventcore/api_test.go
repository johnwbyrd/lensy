package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func newTestCore(t *testing.T, dir string) (*Core, *httptest.Server) {
	t.Helper()
	lg, err := OpenLog(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	st := NewState()
	if _, err := lg.Replay(st.Apply); err != nil {
		t.Fatal(err)
	}
	core := NewCore(lg, st, dir)
	srv := httptest.NewServer(core.Routes(""))
	t.Cleanup(func() { srv.Close(); lg.Close() })
	return core, srv
}

func post(t *testing.T, url string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func detection(dedupe string, probe bool) DetectionIn {
	img := []byte("not-really-a-jpeg-" + dedupe)
	sum := sha256.Sum256(img)
	d := DetectionIn{
		TraceID: "trace-" + dedupe, Camera: "lrn:gov:us-st-06.dot:video:camera/CAM01", Agency: "us-st-06.dot",
		Hazard: "disabled_vehicle", Confidence: 0.91, ModelVersion: "stationary-v0.1", DedupeKey: dedupe, Probe: probe,
		CapturedAt: time.Now().UTC().Add(-900 * time.Millisecond), OnsetEstimate: time.Now().UTC().Add(-4 * time.Second),
		ConfirmedAt: time.Now().UTC().Add(-100 * time.Millisecond), EmittedAt: time.Now().UTC(),
	}
	d.Evidence.ImageB64 = base64.StdEncoding.EncodeToString(img)
	d.Evidence.SHA256 = hex.EncodeToString(sum[:])
	return d
}

func TestAlertLifecycleAndIdempotency(t *testing.T) {
	_, srv := newTestCore(t, t.TempDir())
	code, out := post(t, srv.URL+"/v1/detections", detection("cam01:track1", false))
	if code != 201 || out["status"] != "presented" {
		t.Fatalf("want 201 presented, got %d %v", code, out)
	}
	id := out["alert_id"].(string)

	// Same dedupe key again: at-least-once delivery must not create a second alert.
	code, dup := post(t, srv.URL+"/v1/detections", detection("cam01:track1", false))
	if code != 200 || dup["duplicate"] != true || dup["alert_id"] != id {
		t.Fatalf("want duplicate of %s, got %d %v", id, code, dup)
	}

	// Dispatch before validation is refused.
	if code, _ := post(t, srv.URL+"/v1/alerts/"+id+"/dispatch", ActionIn{Operator: "disp-7"}); code != 409 {
		t.Fatalf("want 409 for dispatch of presented alert, got %d", code)
	}
	// Decisions must be attributed.
	if code, _ := post(t, srv.URL+"/v1/alerts/"+id+"/acknowledge", ActionIn{}); code != 400 {
		t.Fatalf("want 400 for unattributed action, got %d", code)
	}
	for _, step := range []struct{ action, status string }{
		{"acknowledge", "acknowledged"}, {"validate", "validated"}, {"dispatch", "dispatched"}, {"close", "closed"},
	} {
		code, out := post(t, srv.URL+"/v1/alerts/"+id+"/"+step.action, ActionIn{Operator: "disp-7", Note: step.action})
		if code != 200 || out["status"] != step.status {
			t.Fatalf("%s: want 200 %s, got %d %v", step.action, step.status, code, out)
		}
	}
	// Terminal state.
	if code, _ := post(t, srv.URL+"/v1/alerts/"+id+"/validate", ActionIn{Operator: "disp-7"}); code != 409 {
		t.Fatalf("want 409 after close, got %d", code)
	}
}

func TestEvidenceIntegrity(t *testing.T) {
	_, srv := newTestCore(t, t.TempDir())
	d := detection("cam01:bad", false)
	d.Evidence.SHA256 = "00"
	if code, _ := post(t, srv.URL+"/v1/detections", d); code != 400 {
		t.Fatalf("want 400 on hash mismatch, got %d", code)
	}
}

func TestProbeNeverReachesOperators(t *testing.T) {
	core, srv := newTestCore(t, t.TempDir())
	code, out := post(t, srv.URL+"/v1/detections", detection("probe:1", true))
	if code != 201 || out["status"] != "probe" {
		t.Fatalf("want probe, got %d %v", code, out)
	}
	if n := len(core.st.OperatorAlerts(true)); n != 0 {
		t.Fatalf("probe leaked into operator queue: %d alerts", n)
	}
	if code, _ := post(t, srv.URL+"/v1/alerts/"+out["alert_id"].(string)+"/acknowledge", ActionIn{Operator: "x"}); code != 403 {
		t.Fatalf("want 403 acting on probe, got %d", code)
	}
	if m := core.st.Snapshot(time.Now()); m.Probes.Total != 1 {
		t.Fatalf("probe not counted: %+v", m.Probes)
	}
}

func TestKillSwitchSuppresses(t *testing.T) {
	_, srv := newTestCore(t, t.TempDir())
	post(t, srv.URL+"/v1/agencies/us-st-06.dot/killswitch", KillSwitchIn{Active: true, Operator: "sup-1", Reason: "alert flood"})
	code, out := post(t, srv.URL+"/v1/detections", detection("cam01:ks", false))
	if code != 201 || out["status"] != "suppressed" {
		t.Fatalf("want suppressed, got %d %v", code, out)
	}
	post(t, srv.URL+"/v1/agencies/us-st-06.dot/killswitch", KillSwitchIn{Active: false, Operator: "sup-1"})
	code, out = post(t, srv.URL+"/v1/detections", detection("cam01:ks2", false))
	if code != 201 || out["status"] != "presented" {
		t.Fatalf("want presented after release, got %d %v", code, out)
	}
}

func TestReplayRebuildsState(t *testing.T) {
	dir := t.TempDir()
	core, srv := newTestCore(t, dir)
	_, out := post(t, srv.URL+"/v1/detections", detection("cam01:replay", false))
	id := out["alert_id"].(string)
	post(t, srv.URL+"/v1/alerts/"+id+"/validate", ActionIn{Operator: "disp-2"})
	post(t, srv.URL+"/v1/feeds/heartbeat", HeartbeatIn{Camera: "lrn:gov:us-st-06.dot:video:camera/CAM01", Agency: "us-st-06.dot", FrameTS: time.Now(), FPS: 5})
	core.log.Close()

	lg, err := OpenLog(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	st := NewState()
	n, err := lg.Replay(st.Apply)
	if err != nil || n != 4 {
		t.Fatalf("replay: n=%d err=%v", n, err)
	}
	a := st.GetAlert(id)
	if a == nil || a.Status != "validated" || a.Operator != "disp-2" {
		t.Fatalf("state not rebuilt: %+v", a)
	}
	if f := st.FeedList(time.Now()); len(f) != 1 || f[0].Status != "fresh" {
		t.Fatalf("feed not rebuilt: %+v", f)
	}
	// Idempotency survives restart too.
	core2 := NewCore(lg, st, dir)
	srv2 := httptest.NewServer(core2.Routes(""))
	defer srv2.Close()
	if code, dup := post(t, srv2.URL+"/v1/detections", detection("cam01:replay", false)); code != 200 || dup["duplicate"] != true {
		t.Fatalf("dedupe lost on restart: %d %v", code, dup)
	}
}

func TestWatchdogMarksStaleAndEscalates(t *testing.T) {
	core, srv := newTestCore(t, t.TempDir())
	post(t, srv.URL+"/v1/feeds/heartbeat", HeartbeatIn{Camera: "cam", Agency: "ag", FrameTS: time.Now(), FPS: 5})
	post(t, srv.URL+"/v1/detections", detection("cam:esc", false))
	time.Sleep(30 * time.Millisecond)
	now := time.Now()
	for _, f := range core.st.StaleFeeds(now, 10*time.Millisecond) {
		core.Commit(&Event{Type: EvFeedStale, Agency: f.Agency, Resource: f.Camera, Actor: "watchdog"})
	}
	for _, a := range core.st.UnacknowledgedSince(now, 10*time.Millisecond) {
		core.Commit(&Event{Type: EvAlertEscalated, Agency: a.Agency, Resource: a.Camera, Actor: "watchdog", Payload: mustJSON(map[string]any{"alert_id": a.ID})})
	}
	if f := core.st.FeedList(now); f[0].Status != "stale" {
		t.Fatalf("feed should be stale: %+v", f[0])
	}
	if al := core.st.OperatorAlerts(false); len(al) != 1 || !al[0].Escalated {
		t.Fatalf("alert should be escalated: %+v", al)
	}
	time.Sleep(20 * time.Millisecond)
	m := core.st.Snapshot(time.Now())
	if m.Coverage <= 0 || m.Coverage >= 1 {
		t.Fatalf("coverage should be partial after a stale period: %v", m.Coverage)
	}
}

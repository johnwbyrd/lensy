package main

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Alert is the projection of one hazard's life, rebuilt from events.
type Alert struct {
	ID             string               `json:"id"`
	TraceID        string               `json:"trace_id"`
	Agency         string               `json:"agency"`
	Camera         string               `json:"camera"`
	Hazard         string               `json:"hazard"`
	Confidence     float64              `json:"confidence"`
	ModelVersion   string               `json:"model_version"`
	Status         string               `json:"status"`
	Probe          bool                 `json:"probe"`
	Suppressed     bool                 `json:"suppressed"`
	Escalated      bool                 `json:"escalated"`
	DedupeKey      string               `json:"dedupe_key"`
	EvidenceURL    string               `json:"evidence_url,omitempty"`
	EvidenceSHA256 string               `json:"evidence_sha256,omitempty"`
	BBox           [4]int               `json:"bbox"`
	Stationary     float64              `json:"stationary_seconds"`
	Operator       string               `json:"operator,omitempty"`
	Note           string               `json:"note,omitempty"`
	Timeline       map[string]time.Time `json:"timeline"`
	Latency        map[string]float64   `json:"latency_ms"`
}

type Feed struct {
	Camera        string    `json:"camera"`
	Agency        string    `json:"agency"`
	Status        string    `json:"status"` // fresh | frozen | stale | unknown
	LastHeartbeat time.Time `json:"last_heartbeat"`
	LastFrameTS   time.Time `json:"last_frame_ts"`
	FPS           float64   `json:"fps"`
	Frozen        bool      `json:"frozen"`
	SceneChange   bool      `json:"scene_change"`
	Source        string    `json:"source,omitempty"`
	Label         string    `json:"label,omitempty"`
	FrameAgeS     float64   `json:"frame_age_s"` // age of newest decoded frame at last heartbeat
	// Coverage accounting: seconds in each status since first seen.
	Seconds       map[string]float64 `json:"seconds"`
	lastAccounted time.Time
}

type KillSwitch struct {
	Active   bool      `json:"active"`
	Operator string    `json:"operator,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Since    time.Time `json:"since"`
}

type ProbeStats struct {
	Total   int       `json:"total"`
	LastOK  time.Time `json:"last_ok"`
	LastAge float64   `json:"last_ok_age_s"`
}

// State is the in-memory projection. It is only ever mutated through Apply,
// and Apply is only ever called with events already in the log.
type State struct {
	mu         sync.RWMutex
	Alerts     map[string]*Alert
	Feeds      map[string]*Feed
	KillSwitch map[string]*KillSwitch
	Probes     ProbeStats
	dedupe     map[string]string
	lat        map[string]*window
	counts     map[string]int
	started    time.Time
	lastSeq    uint64
}

func NewState() *State {
	return &State{
		Alerts:     map[string]*Alert{},
		Feeds:      map[string]*Feed{},
		KillSwitch: map[string]*KillSwitch{},
		dedupe:     map[string]string{},
		lat:        map[string]*window{},
		counts:     map[string]int{},
		started:    time.Now(),
	}
}

// ---- payload types shared with the API ----

type detectionPayload struct {
	Hazard        string    `json:"hazard"`
	Confidence    float64   `json:"confidence"`
	ModelVersion  string    `json:"model_version"`
	DedupeKey     string    `json:"dedupe_key"`
	Probe         bool      `json:"probe"`
	CapturedAt    time.Time `json:"captured_at"`
	OnsetEstimate time.Time `json:"onset_estimate"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
	EmittedAt     time.Time `json:"emitted_at"`
	EvidenceURL   string    `json:"evidence_url"`
	EvidenceSHA   string    `json:"evidence_sha256"`
	BBox          [4]int    `json:"bbox"`
	Stationary    float64   `json:"stationary_seconds"`
}

type actionPayload struct {
	Note string `json:"note,omitempty"`
}

type renderedPayload struct {
	RenderedAt time.Time `json:"rendered_at"`
}

type heartbeatPayload struct {
	FrameTS     time.Time `json:"frame_ts"`
	FPS         float64   `json:"fps"`
	Frozen      bool      `json:"frozen"`
	SceneChange bool      `json:"scene_change"`
	Source      string    `json:"source,omitempty"`
	Label       string    `json:"label,omitempty"`
	FrameAgeS   float64   `json:"frame_age_s,omitempty"`
}

type killSwitchPayload struct {
	Active bool   `json:"active"`
	Reason string `json:"reason,omitempty"`
}

// Apply folds one event into the projection.
func (s *State) Apply(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSeq = ev.Seq
	s.counts[ev.Type]++
	switch ev.Type {
	case EvDetectionCandidate:
		var p detectionPayload
		_ = json.Unmarshal(ev.Payload, &p)
		a := &Alert{
			ID: ev.ID, TraceID: ev.TraceID, Agency: ev.Agency, Camera: ev.Resource,
			Hazard: p.Hazard, Confidence: p.Confidence, ModelVersion: p.ModelVersion,
			Status: "candidate", Probe: p.Probe, DedupeKey: p.DedupeKey,
			EvidenceURL: p.EvidenceURL, EvidenceSHA256: p.EvidenceSHA, BBox: p.BBox,
			Stationary: p.Stationary,
			Timeline: map[string]time.Time{
				"captured_at": p.CapturedAt, "onset_estimate": p.OnsetEstimate,
				"confirmed_at": p.ConfirmedAt, "emitted_at": p.EmittedAt, "committed_at": ev.TS,
			},
			Latency: map[string]float64{},
		}
		a.Latency["capture_to_commit"] = ms(ev.TS.Sub(p.CapturedAt))
		a.Latency["onset_to_confirm"] = ms(p.ConfirmedAt.Sub(p.OnsetEstimate))
		s.Alerts[a.ID] = a
		if p.DedupeKey != "" {
			s.dedupe[p.DedupeKey] = a.ID
		}
		if !p.Probe {
			s.observe("capture_to_commit", ev.TS.Sub(p.CapturedAt))
		}
	case EvAlertPresented, EvAlertSuppressed:
		if a := s.Alerts[alertRef(ev)]; a != nil {
			if ev.Type == EvAlertSuppressed {
				a.Status = "suppressed"
				a.Suppressed = true
			} else {
				a.Status = "presented"
			}
			a.Timeline["presented_at"] = ev.TS
		}
	case EvProbeReceived:
		if a := s.Alerts[alertRef(ev)]; a != nil {
			a.Status = "probe"
			s.Probes.Total++
			s.Probes.LastOK = ev.TS
			s.observe("probe_capture_to_commit", ev.TS.Sub(a.Timeline["captured_at"]))
		}
	case EvAlertRendered:
		if a := s.Alerts[alertRef(ev)]; a != nil {
			if _, done := a.Timeline["rendered_at"]; !done {
				var p renderedPayload
				_ = json.Unmarshal(ev.Payload, &p)
				a.Timeline["rendered_at"] = p.RenderedAt
				a.Latency["commit_to_render"] = ms(p.RenderedAt.Sub(a.Timeline["committed_at"]))
				a.Latency["capture_to_render"] = ms(p.RenderedAt.Sub(a.Timeline["captured_at"]))
				s.observe("commit_to_render", p.RenderedAt.Sub(a.Timeline["committed_at"]))
				s.observe("capture_to_render", p.RenderedAt.Sub(a.Timeline["captured_at"]))
			}
		}
	case EvAlertAcknowledged, EvAlertValidated, EvAlertRejected, EvAlertDispatched, EvAlertClosed:
		if a := s.Alerts[alertRef(ev)]; a != nil {
			var p actionPayload
			_ = json.Unmarshal(ev.Payload, &p)
			status := map[string]string{
				EvAlertAcknowledged: "acknowledged", EvAlertValidated: "validated",
				EvAlertRejected: "rejected", EvAlertDispatched: "dispatched", EvAlertClosed: "closed",
			}[ev.Type]
			a.Status = status
			a.Timeline[status+"_at"] = ev.TS
			if ev.Actor != "" {
				a.Operator = ev.Actor
			}
			if p.Note != "" {
				a.Note = p.Note
			}
			if ev.Type == EvAlertAcknowledged {
				a.Latency["present_to_ack"] = ms(ev.TS.Sub(a.Timeline["presented_at"]))
				s.observe("present_to_ack", ev.TS.Sub(a.Timeline["presented_at"]))
			}
			if ev.Type == EvAlertDispatched {
				a.Latency["capture_to_dispatch"] = ms(ev.TS.Sub(a.Timeline["captured_at"]))
				s.observe("capture_to_dispatch", ev.TS.Sub(a.Timeline["captured_at"]))
			}
		}
	case EvAlertEscalated:
		if a := s.Alerts[alertRef(ev)]; a != nil {
			a.Escalated = true
			a.Timeline["escalated_at"] = ev.TS
		}
	case EvFeedHeartbeat:
		var p heartbeatPayload
		_ = json.Unmarshal(ev.Payload, &p)
		f := s.feed(ev.Resource, ev.Agency)
		f.account(ev.TS)
		f.LastHeartbeat = ev.TS
		f.LastFrameTS = p.FrameTS
		f.FPS = p.FPS
		f.Frozen = p.Frozen
		f.SceneChange = p.SceneChange
		if p.Source != "" {
			f.Source = p.Source
		}
		if p.Label != "" {
			f.Label = p.Label
		}
		f.FrameAgeS = p.FrameAgeS
		if p.Frozen {
			f.Status = "frozen"
		} else {
			f.Status = "fresh"
		}
	case EvFeedStale:
		f := s.feed(ev.Resource, ev.Agency)
		f.account(ev.TS)
		f.Status = "stale"
	case EvFeedRecovered:
		f := s.feed(ev.Resource, ev.Agency)
		f.account(ev.TS)
		f.Status = "fresh"
	case EvAgencyKillSwitch:
		var p killSwitchPayload
		_ = json.Unmarshal(ev.Payload, &p)
		s.KillSwitch[ev.Agency] = &KillSwitch{Active: p.Active, Operator: ev.Actor, Reason: p.Reason, Since: ev.TS}
	}
}

func alertRef(ev Event) string {
	var p struct {
		AlertID string `json:"alert_id"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	return p.AlertID
}

func (s *State) feed(camera, agency string) *Feed {
	f := s.Feeds[camera]
	if f == nil {
		f = &Feed{Camera: camera, Agency: agency, Status: "unknown", Seconds: map[string]float64{}}
		s.Feeds[camera] = f
	}
	return f
}

// account attributes elapsed time to the feed's current status. This is the
// raw material for "effective coverage": camera-minutes actually watched.
func (f *Feed) account(now time.Time) {
	if !f.lastAccounted.IsZero() && now.After(f.lastAccounted) {
		f.Seconds[f.Status] += now.Sub(f.lastAccounted).Seconds()
	}
	f.lastAccounted = now
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// ---- read side ----

func (s *State) LookupDedupe(key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.dedupe[key]
	return id, ok
}

func (s *State) GetAlert(id string) *Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if a := s.Alerts[id]; a != nil {
		c := *a
		return &c
	}
	return nil
}

func (s *State) KillSwitchActive(agency string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k := s.KillSwitch[agency]
	return k != nil && k.Active
}

// OperatorAlerts returns non-probe alerts, newest first. Probes never appear.
func (s *State) OperatorAlerts(includeClosed bool) []*Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Alert, 0, len(s.Alerts))
	for _, a := range s.Alerts {
		if a.Probe {
			continue
		}
		if !includeClosed && (a.Status == "closed" || a.Status == "rejected" || a.Status == "suppressed") {
			continue
		}
		c := *a
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timeline["committed_at"].After(out[j].Timeline["committed_at"]) })
	return out
}

func (s *State) FeedList(now time.Time) []*Feed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Feed, 0, len(s.Feeds))
	for _, f := range s.Feeds {
		c := *f
		c.Seconds = map[string]float64{}
		for k, v := range f.Seconds {
			c.Seconds[k] = v
		}
		if !f.lastAccounted.IsZero() && now.After(f.lastAccounted) {
			c.Seconds[f.Status] += now.Sub(f.lastAccounted).Seconds()
		}
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Camera < out[j].Camera })
	return out
}

// StaleFeeds returns feeds whose heartbeat is older than maxAge and that are not already marked stale.
func (s *State) StaleFeeds(now time.Time, maxAge time.Duration) []*Feed {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Feed
	for _, f := range s.Feeds {
		if f.Status != "stale" && f.Status != "unknown" && now.Sub(f.LastHeartbeat) > maxAge {
			c := *f
			out = append(out, &c)
		}
	}
	return out
}

// UnacknowledgedSince returns presented, non-probe alerts older than maxAge not yet escalated.
func (s *State) UnacknowledgedSince(now time.Time, maxAge time.Duration) []*Alert {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Alert
	for _, a := range s.Alerts {
		if !a.Probe && a.Status == "presented" && !a.Escalated && now.Sub(a.Timeline["presented_at"]) > maxAge {
			c := *a
			out = append(out, &c)
		}
	}
	return out
}

type KillSwitchView struct {
	Agency string `json:"agency"`
	KillSwitch
}

func (s *State) KillSwitches() []KillSwitchView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []KillSwitchView{}
	for ag, k := range s.KillSwitch {
		out = append(out, KillSwitchView{Agency: ag, KillSwitch: *k})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agency < out[j].Agency })
	return out
}

// ---- metrics ----

type window struct {
	vals []float64 // ms
	max  int
}

func (s *State) observe(name string, d time.Duration) {
	w := s.lat[name]
	if w == nil {
		w = &window{max: 2000}
		s.lat[name] = w
	}
	w.vals = append(w.vals, ms(d))
	if len(w.vals) > w.max {
		w.vals = w.vals[len(w.vals)-w.max:]
	}
}

type Percentiles struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
	Max float64 `json:"max_ms"`
}

func (w *window) percentiles() Percentiles {
	if len(w.vals) == 0 {
		return Percentiles{}
	}
	c := append([]float64(nil), w.vals...)
	sort.Float64s(c)
	q := func(p float64) float64 {
		i := int(p*float64(len(c)-1) + 0.5)
		if i < 0 {
			i = 0
		}
		if i >= len(c) {
			i = len(c) - 1
		}
		return c[i]
	}
	return Percentiles{N: len(c), P50: q(0.50), P95: q(0.95), P99: q(0.99), Max: c[len(c)-1]}
}

type Metrics struct {
	UptimeS         float64                `json:"uptime_s"`
	LastSeq         uint64                 `json:"last_seq"`
	EventCounts     map[string]int         `json:"event_counts"`
	AlertsByStatus  map[string]int         `json:"alerts_by_status"`
	FeedsByStatus   map[string]int         `json:"feeds_by_status"`
	Coverage        float64                `json:"effective_coverage"`
	CoverageByCause map[string]float64     `json:"lost_coverage_seconds"`
	Latency         map[string]Percentiles `json:"latency"`
	Probes          ProbeStats             `json:"probes"`
	KillSwitches    []KillSwitchView       `json:"kill_switches"`
}

func (s *State) Snapshot(now time.Time) Metrics {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := Metrics{
		UptimeS: now.Sub(s.started).Seconds(), LastSeq: s.lastSeq,
		EventCounts: map[string]int{}, AlertsByStatus: map[string]int{}, FeedsByStatus: map[string]int{},
		CoverageByCause: map[string]float64{}, Latency: map[string]Percentiles{}, Probes: s.Probes,
	}
	for k, v := range s.counts {
		m.EventCounts[k] = v
	}
	for _, a := range s.Alerts {
		if a.Probe {
			continue
		}
		m.AlertsByStatus[a.Status]++
	}
	var fresh, total float64
	for _, f := range s.Feeds {
		m.FeedsByStatus[f.Status]++
		for st, sec := range f.Seconds {
			total += sec
			if st == "fresh" {
				fresh += sec
			} else {
				m.CoverageByCause[st] += sec
			}
		}
		if !f.lastAccounted.IsZero() && now.After(f.lastAccounted) {
			d := now.Sub(f.lastAccounted).Seconds()
			total += d
			if f.Status == "fresh" {
				fresh += d
			} else {
				m.CoverageByCause[f.Status] += d
			}
		}
	}
	if total > 0 {
		m.Coverage = fresh / total
	}
	for k, w := range s.lat {
		m.Latency[k] = w.percentiles()
	}
	if !s.Probes.LastOK.IsZero() {
		m.Probes.LastAge = now.Sub(s.Probes.LastOK).Seconds()
	}
	for ag, k := range s.KillSwitch {
		m.KillSwitches = append(m.KillSwitches, KillSwitchView{Agency: ag, KillSwitch: *k})
	}
	sort.Slice(m.KillSwitches, func(i, j int) bool { return m.KillSwitches[i].Agency < m.KillSwitches[j].Agency })
	return m
}

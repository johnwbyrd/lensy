package main

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Core owns the log, the projection and the fan-out to connected consoles.
// Commit is the only write path: durable append, then apply, then broadcast.
type Core struct {
	log     *Log
	st      *State
	dataDir string

	subMu sync.Mutex
	subs  map[chan []byte]struct{}
}

func NewCore(l *Log, st *State, dataDir string) *Core {
	return &Core{log: l, st: st, dataDir: dataDir, subs: map[chan []byte]struct{}{}}
}

// Commit writes ev durably, folds it into state and notifies subscribers.
func (c *Core) Commit(ev *Event) error {
	if ev.Schema == "" {
		ev.Schema = SchemaVersion
	}
	if ev.ID == "" {
		ev.ID = newID()
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	if err := c.log.Append(ev); err != nil {
		return err
	}
	c.st.Apply(*ev)
	c.broadcast(*ev)
	return nil
}

// streamFrame is what consoles receive: the event plus the refreshed
// projections it touched, so the browser never has to re-derive state.
type streamFrame struct {
	Kind    string           `json:"kind"` // snapshot | event | heartbeat
	Event   *Event           `json:"event,omitempty"`
	Alert   *Alert           `json:"alert,omitempty"`
	Feed    *Feed            `json:"feed,omitempty"`
	Alerts  []*Alert         `json:"alerts,omitempty"`
	Feeds   []*Feed          `json:"feeds,omitempty"`
	Kill    []KillSwitchView `json:"kill_switches,omitempty"`
	Metrics *Metrics         `json:"metrics,omitempty"`
	Now     time.Time        `json:"now"`
}

func (c *Core) broadcast(ev Event) {
	fr := streamFrame{Kind: "event", Event: &ev, Now: time.Now().UTC()}
	if id := alertRef(ev); id != "" {
		fr.Alert = c.st.GetAlert(id)
	} else if ev.Type == EvDetectionCandidate {
		fr.Alert = c.st.GetAlert(ev.ID)
	}
	if ev.Type == EvFeedHeartbeat || ev.Type == EvFeedStale || ev.Type == EvFeedRecovered {
		for _, f := range c.st.FeedList(fr.Now) {
			if f.Camera == ev.Resource {
				fr.Feed = f
			}
		}
	}
	if ev.Type == EvAgencyKillSwitch {
		fr.Kill = c.st.KillSwitches()
	}
	m := c.st.Snapshot(fr.Now)
	fr.Metrics = &m
	b, _ := json.Marshal(fr)
	c.subMu.Lock()
	defer c.subMu.Unlock()
	for ch := range c.subs {
		select {
		case ch <- b:
		default:
			// A console that cannot keep up is dropped rather than allowed to
			// stall the alert path. It reconnects and receives a fresh snapshot.
			delete(c.subs, ch)
			close(ch)
		}
	}
}

func (c *Core) subscribe() chan []byte {
	ch := make(chan []byte, 256)
	c.subMu.Lock()
	c.subs[ch] = struct{}{}
	c.subMu.Unlock()
	return ch
}

func (c *Core) unsubscribe(ch chan []byte) {
	c.subMu.Lock()
	if _, ok := c.subs[ch]; ok {
		delete(c.subs, ch)
		close(ch)
	}
	c.subMu.Unlock()
}

func (c *Core) snapshot() []byte {
	now := time.Now().UTC()
	m := c.st.Snapshot(now)
	fr := streamFrame{
		Kind: "snapshot", Alerts: c.st.OperatorAlerts(true), Feeds: c.st.FeedList(now),
		Kill: c.st.KillSwitches(), Metrics: &m, Now: now,
	}
	b, _ := json.Marshal(fr)
	return b
}

// watchdog turns silence into events. A feed that stops heartbeating becomes
// feed.stale; an alert nobody acknowledges becomes alert.escalated. Both are
// written to the log like any other fact, so they are auditable and replayable.
func (c *Core) watchdog(ctx context.Context, staleAfter, escalateAfter time.Duration) {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			for _, f := range c.st.StaleFeeds(now, staleAfter) {
				ev := &Event{Type: EvFeedStale, Agency: f.Agency, Resource: f.Camera, Actor: "watchdog",
					Payload: mustJSON(map[string]any{"last_heartbeat": f.LastHeartbeat, "silence_s": now.Sub(f.LastHeartbeat).Seconds()})}
				if err := c.Commit(ev); err != nil {
					log.Printf("watchdog: %v", err)
				}
			}
			for _, a := range c.st.UnacknowledgedSince(now, escalateAfter) {
				ev := &Event{Type: EvAlertEscalated, TraceID: a.TraceID, Agency: a.Agency, Resource: a.Camera, Actor: "watchdog",
					Payload: mustJSON(map[string]any{"alert_id": a.ID, "unacknowledged_s": now.Sub(a.Timeline["presented_at"]).Seconds()})}
				if err := c.Commit(ev); err != nil {
					log.Printf("watchdog: %v", err)
				}
			}
		}
	}
}

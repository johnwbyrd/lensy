package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// SchemaVersion is the versioned envelope every record in the log carries.
// The event log is the system of record: every alert's life is reconstructed
// from these records and nothing else.
const SchemaVersion = "lens.event.v1"

// Event is the append-only record. Payload is type specific.
type Event struct {
	Schema   string          `json:"schema"`
	ID       string          `json:"event_id"`
	Seq      uint64          `json:"seq"`
	TraceID  string          `json:"trace_id,omitempty"`
	Type     string          `json:"type"`
	TS       time.Time       `json:"ts"`
	Agency   string          `json:"agency,omitempty"`
	Resource string          `json:"resource,omitempty"`
	Actor    string          `json:"actor,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Event types. The alert lifecycle is candidate -> presented -> acknowledged ->
// validated | rejected -> dispatched -> closed. Probe events never enter the
// operator queue. Feed and agency events are control signals.
const (
	EvDetectionCandidate = "detection.candidate"
	EvAlertPresented     = "alert.presented"
	EvAlertSuppressed    = "alert.suppressed"
	EvAlertRendered      = "alert.rendered"
	EvAlertAcknowledged  = "alert.acknowledged"
	EvAlertValidated     = "alert.validated"
	EvAlertRejected      = "alert.rejected"
	EvAlertDispatched    = "alert.dispatched"
	EvAlertClosed        = "alert.closed"
	EvAlertEscalated     = "alert.escalated"
	EvProbeReceived      = "probe.received"
	EvFeedHeartbeat      = "feed.heartbeat"
	EvFeedStale          = "feed.stale"
	EvFeedRecovered      = "feed.recovered"
	EvAgencyKillSwitch   = "agency.killswitch"
)

// newID returns a time-ordered, opaque identifier (ULID-like). Identifiers
// never carry names, locations or case numbers because they end up in logs.
func newID() string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	ms := uint64(time.Now().UnixMilli())
	var out [26]byte
	for i := 9; i >= 0; i-- {
		out[i] = alphabet[ms&31]
		ms >>= 5
	}
	var rnd [16]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		panic(err)
	}
	for i := 0; i < 16; i++ {
		out[10+i] = alphabet[rnd[i]&31]
	}
	return string(out[:])
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("marshal payload: %v", err))
	}
	return b
}

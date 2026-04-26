package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// memCrashesWriter is a CrashesWriter that records every InsertCrash call
// in memory so tests can assert on what was persisted.
type memCrashesWriter struct {
	mu        sync.Mutex
	inserts   []db.InsertCrashParams
	insertErr error
}

func (m *memCrashesWriter) InsertCrash(_ context.Context, arg db.InsertCrashParams) (db.Crash, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.insertErr != nil {
		return db.Crash{}, m.insertErr
	}
	m.inserts = append(m.inserts, arg)
	return db.Crash{
		ID:      int32(len(m.inserts)),
		EventID: arg.EventID,
	}, nil
}

func buildEnvelope(t *testing.T, eventPayload map[string]interface{}) []byte {
	t.Helper()
	hdr, _ := json.Marshal(map[string]interface{}{
		"event_id": eventPayload["event_id"],
		"sent_at":  "2026-04-24T18:30:00Z",
	})
	itemHdr, _ := json.Marshal(map[string]interface{}{
		"type": "event",
	})
	payload, _ := json.Marshal(eventPayload)

	var buf bytes.Buffer
	buf.Write(hdr)
	buf.WriteByte('\n')
	buf.Write(itemHdr)
	buf.WriteByte('\n')
	buf.Write(payload)
	buf.WriteByte('\n')
	return buf.Bytes()
}

func postEnvelope(t *testing.T, h *SentryRelay, body []byte, gzipped bool) *httptest.ResponseRecorder {
	t.Helper()
	if gzipped {
		var compressed bytes.Buffer
		gw := gzip.NewWriter(&compressed)
		if _, err := gw.Write(body); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		body = compressed.Bytes()
	}

	req := httptest.NewRequest(http.MethodPost, "/api/123/envelope/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-sentry-envelope")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.SetParamNames("project_id")
	c.SetParamValues("123")
	if err := h.HandleEnvelope(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

func TestSentryRelay_PersistsEvent(t *testing.T) {
	store := &memCrashesWriter{}
	relay := NewSentryRelay(store)

	envelope := buildEnvelope(t, map[string]interface{}{
		"event_id":  "ab123",
		"level":     "error",
		"message":   "boom",
		"timestamp": 1714000000.0,
		"user":      map[string]interface{}{"id": "abc123def4567890"},
		"tags":      map[string]interface{}{"branch": "master"},
		"exception": map[string]interface{}{
			"values": []interface{}{
				map[string]interface{}{"type": "RuntimeError", "value": "boom"},
			},
		},
		"fingerprint": []string{"manual"},
	})

	rec := postEnvelope(t, relay, envelope, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v body=%s", err, rec.Body.String())
	}
	if resp["id"] != "ab123" {
		t.Errorf("response id = %q, want ab123", resp["id"])
	}

	if got := len(store.inserts); got != 1 {
		t.Fatalf("inserts = %d, want 1", got)
	}
	in := store.inserts[0]
	if in.EventID != "ab123" {
		t.Errorf("event_id = %q, want ab123", in.EventID)
	}
	if in.Level != "error" {
		t.Errorf("level = %q, want error", in.Level)
	}
	if in.Message != "boom" {
		t.Errorf("message = %q, want boom", in.Message)
	}
	if !in.DongleID.Valid || in.DongleID.String != "abc123def4567890" {
		t.Errorf("dongle_id = %v, want abc123def4567890", in.DongleID)
	}
	if !in.OccurredAt.Valid {
		t.Errorf("occurred_at not set despite numeric timestamp")
	}
}

func TestSentryRelay_DecompressesGzipBody(t *testing.T) {
	store := &memCrashesWriter{}
	relay := NewSentryRelay(store)

	envelope := buildEnvelope(t, map[string]interface{}{
		"event_id": "gz1",
		"level":    "error",
	})

	rec := postEnvelope(t, relay, envelope, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}

	if got := len(store.inserts); got != 1 {
		t.Fatalf("inserts = %d, want 1", got)
	}
	if store.inserts[0].EventID != "gz1" {
		t.Errorf("event_id = %q, want gz1", store.inserts[0].EventID)
	}
}

func TestSentryRelay_SkipsNonEventItems(t *testing.T) {
	store := &memCrashesWriter{}
	relay := NewSentryRelay(store)

	// Build an envelope with a transaction item only -- should be ignored.
	hdr, _ := json.Marshal(map[string]interface{}{"event_id": "tx1"})
	itemHdr, _ := json.Marshal(map[string]interface{}{"type": "transaction"})
	payload, _ := json.Marshal(map[string]interface{}{"event_id": "tx1"})

	var buf bytes.Buffer
	buf.Write(hdr)
	buf.WriteByte('\n')
	buf.Write(itemHdr)
	buf.WriteByte('\n')
	buf.Write(payload)
	buf.WriteByte('\n')

	rec := postEnvelope(t, relay, buf.Bytes(), false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := len(store.inserts); got != 0 {
		t.Errorf("expected no inserts for transaction-only envelope, got %d", got)
	}

	// The response id should fall back to the envelope header's event_id
	// when no event items were ingested.
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["id"] != "tx1" {
		t.Errorf("response id = %q, want fallback tx1", resp["id"])
	}
}

func TestSentryRelay_RejectsEmptyEnvelope(t *testing.T) {
	store := &memCrashesWriter{}
	relay := NewSentryRelay(store)

	rec := postEnvelope(t, relay, []byte(""), false)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

func TestSentryRelay_RegisterRoutesRegistersBothPaths(t *testing.T) {
	relay := NewSentryRelay(&memCrashesWriter{})
	e := echo.New()
	relay.RegisterRoutes(e)

	want := map[string]bool{
		"/api/:project_id/envelope/": false,
		"/api/:project_id/envelope":  false,
	}
	for _, r := range e.Routes() {
		if r.Method == http.MethodPost {
			if _, ok := want[r.Path]; ok {
				want[r.Path] = true
			}
		}
	}
	for path, found := range want {
		if !found {
			t.Errorf("expected POST %s to be registered", path)
		}
	}
}

func TestSentryRelay_TimestampString(t *testing.T) {
	store := &memCrashesWriter{}
	relay := NewSentryRelay(store)

	envelope := buildEnvelope(t, map[string]interface{}{
		"event_id":  "ts1",
		"timestamp": "2026-04-24T18:30:00Z",
	})

	postEnvelope(t, relay, envelope, false)
	if got := len(store.inserts); got != 1 {
		t.Fatalf("inserts = %d, want 1", got)
	}
	if !store.inserts[0].OccurredAt.Valid {
		t.Errorf("occurred_at not set despite ISO8601 timestamp")
	}
}

func TestSentryRelay_RawEventPreserved(t *testing.T) {
	store := &memCrashesWriter{}
	relay := NewSentryRelay(store)

	envelope := buildEnvelope(t, map[string]interface{}{
		"event_id": "raw1",
		"unknown":  map[string]interface{}{"future_field": 42},
	})

	postEnvelope(t, relay, envelope, false)
	if got := len(store.inserts); got != 1 {
		t.Fatalf("inserts = %d, want 1", got)
	}
	if !strings.Contains(string(store.inserts[0].RawEvent), "future_field") {
		t.Errorf("raw_event did not preserve unknown fields: %s", string(store.inserts[0].RawEvent))
	}
}

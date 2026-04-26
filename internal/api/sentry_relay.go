package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
)

// SentryRelay accepts Sentry envelopes pushed by sentry_sdk on devices.
//
// Configuration on the device is just the DSN: the operator points
// `sentry_sdk.init(dsn=...)` at e.g.
//
//	`https://<public_key>@my-backend/<project_id>`
//
// and the SDK posts to `/api/<project_id>/envelope/` with a newline-
// delimited JSON envelope. The DSN public key is unused; this is a personal
// backend with a single tenant, so per-project auth would be theatrical.
//
// Envelope format reference:
//
//	https://develop.sentry.dev/sdk/envelopes/
//
// We only ingest items of type "event". Transactions, attachments, and
// other item types are dropped silently because the device doesn't send
// them in practice and we have no UI for them.
type SentryRelay struct {
	queries CrashesWriter
}

// CrashesWriter is the subset of *db.Queries the relay depends on. Kept
// narrow so tests can stub a single method.
type CrashesWriter interface {
	InsertCrash(ctx context.Context, arg db.InsertCrashParams) (db.Crash, error)
}

// NewSentryRelay creates a Sentry envelope ingest handler.
func NewSentryRelay(queries CrashesWriter) *SentryRelay {
	return &SentryRelay{queries: queries}
}

// maxEnvelopeBytes caps the size of an envelope after gzip decompression.
// A gzip bomb that decompresses to many gigabytes would otherwise fill RAM.
const maxEnvelopeBytes = 16 * 1024 * 1024

// envelopeHeader is the first newline-delimited JSON object in a Sentry
// envelope. We only read event_id from it for logging context.
type envelopeHeader struct {
	EventID string `json:"event_id"`
}

// itemHeader precedes each payload in the envelope. We only act on
// type=event; the optional length field tells us how many bytes to read for
// the payload, but most SDKs omit it and rely on the trailing newline.
type itemHeader struct {
	Type   string `json:"type"`
	Length int64  `json:"length,omitempty"`
}

// sentryEvent is the subset of the event payload the dashboard cares about.
// We deliberately ignore most fields and rely on the raw_event JSONB column
// to round-trip everything else for future viewing.
type sentryEvent struct {
	EventID     string                 `json:"event_id"`
	Level       string                 `json:"level"`
	Message     string                 `json:"message"`
	Timestamp   interface{}            `json:"timestamp"`
	Fingerprint []string               `json:"fingerprint"`
	Tags        map[string]interface{} `json:"tags"`
	Exception   map[string]interface{} `json:"exception"`
	Breadcrumbs map[string]interface{} `json:"breadcrumbs"`
	User        map[string]interface{} `json:"user"`
	Logentry    *struct {
		Message string `json:"message"`
	} `json:"logentry"`
}

// HandleEnvelope serves POST /api/:project_id/envelope/. It is intentionally
// authless: the DSN public key is the only "secret" the device has, and it
// is embedded in the binary; rejecting requests without it adds no real
// security, and reading the body to extract it would break sdk-compatible
// flow.
func (r *SentryRelay) HandleEnvelope(c echo.Context) error {
	body, err := readEnvelopeBody(c.Request())
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: fmt.Sprintf("read envelope: %v", err),
			Code:  http.StatusBadRequest,
		})
	}

	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxEnvelopeBytes)

	if !scanner.Scan() {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "empty envelope",
			Code:  http.StatusBadRequest,
		})
	}
	var hdr envelopeHeader
	_ = json.Unmarshal(scanner.Bytes(), &hdr)

	var firstID string
	for {
		if !scanner.Scan() {
			break
		}
		itemHdrBytes := append([]byte(nil), scanner.Bytes()...)
		if len(itemHdrBytes) == 0 {
			continue
		}
		var item itemHeader
		if err := json.Unmarshal(itemHdrBytes, &item); err != nil {
			// Skip malformed item header, but try to keep reading; the next
			// payload line is unrecoverable, so we stop on the next item header
			// after consuming one more line as best-effort.
			scanner.Scan()
			continue
		}
		if !scanner.Scan() {
			break
		}
		payloadBytes := append([]byte(nil), scanner.Bytes()...)

		if item.Type != "event" {
			continue
		}

		stored, storeErr := r.persistEvent(c.Request().Context(), payloadBytes)
		if storeErr != nil {
			log.Printf("sentry-relay: failed to persist event: %v", storeErr)
			continue
		}
		if firstID == "" {
			firstID = stored.EventID
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("sentry-relay: scanner error: %v", err)
	}

	if firstID == "" {
		firstID = hdr.EventID
	}
	return c.JSON(http.StatusOK, map[string]string{"id": firstID})
}

// persistEvent decodes a single event payload and writes it to the crashes
// table. Returns the stored row so the caller can echo the event_id back.
func (r *SentryRelay) persistEvent(ctx context.Context, payload []byte) (db.Crash, error) {
	var ev sentryEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return db.Crash{}, fmt.Errorf("decode event: %w", err)
	}

	// Pull dongle_id out of user.id when present. Sentry SDK puts the
	// device's dongle_id there via system/sentry.py:set_user.
	dongleID := pgtype.Text{}
	if ev.User != nil {
		if s, ok := ev.User["id"].(string); ok && s != "" {
			dongleID = pgtype.Text{String: s, Valid: true}
		}
	}

	level := ev.Level
	if level == "" {
		level = "error"
	}
	message := ev.Message
	if message == "" && ev.Logentry != nil {
		message = ev.Logentry.Message
	}

	occurredAt := decodeSentryTimestamp(ev.Timestamp)

	fingerprintJSON, _ := json.Marshal(orEmptyList(ev.Fingerprint))
	tagsJSON, _ := json.Marshal(orEmptyMap(ev.Tags))
	exceptionJSON, _ := json.Marshal(orEmptyMap(ev.Exception))
	breadcrumbsJSON, _ := json.Marshal(orEmptyMap(ev.Breadcrumbs))

	return r.queries.InsertCrash(ctx, db.InsertCrashParams{
		EventID:     ev.EventID,
		DongleID:    dongleID,
		Level:       level,
		Message:     message,
		Fingerprint: fingerprintJSON,
		Tags:        tagsJSON,
		Exception:   exceptionJSON,
		Breadcrumbs: breadcrumbsJSON,
		RawEvent:    payload,
		OccurredAt:  occurredAt,
	})
}

// readEnvelopeBody reads the request body, transparently decompressing
// gzip when Content-Encoding indicates it. Bounded at maxEnvelopeBytes to
// defend against decompression bombs.
func readEnvelopeBody(req *http.Request) ([]byte, error) {
	body := io.LimitReader(req.Body, maxEnvelopeBytes+1)
	if req.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gr.Close()
		body = io.LimitReader(gr, maxEnvelopeBytes+1)
	}
	out, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(out) > maxEnvelopeBytes {
		return nil, fmt.Errorf("envelope exceeds %d bytes", maxEnvelopeBytes)
	}
	return out, nil
}

// decodeSentryTimestamp accepts the unix-seconds float, ISO8601 string, or
// nil that Sentry SDKs commonly emit, and returns a pgtype.Timestamptz.
// Invalid input falls back to "no timestamp" (Valid=false).
func decodeSentryTimestamp(v interface{}) pgtype.Timestamptz {
	switch t := v.(type) {
	case float64:
		secs := int64(t)
		nsec := int64((t - float64(secs)) * 1e9)
		return pgtype.Timestamptz{Time: time.Unix(secs, nsec).UTC(), Valid: true}
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return pgtype.Timestamptz{Time: parsed.UTC(), Valid: true}
		}
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return pgtype.Timestamptz{Time: parsed.UTC(), Valid: true}
		}
	}
	return pgtype.Timestamptz{}
}

func orEmptyList(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return map[string]interface{}{}
	}
	return m
}

// RegisterRoutes wires up the Sentry envelope endpoint. The route is
// authless and lives at the top level so the device's DSN can simply
// embed our hostname.
func (r *SentryRelay) RegisterRoutes(e *echo.Echo) {
	e.POST("/api/:project_id/envelope/", r.HandleEnvelope)
	e.POST("/api/:project_id/envelope", r.HandleEnvelope)
}

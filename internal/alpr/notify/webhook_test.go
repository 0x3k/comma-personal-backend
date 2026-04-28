package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"comma-personal-backend/internal/alpr"
	"comma-personal-backend/internal/alpr/heuristic"
)

func TestNewWebhookSender_NilOnEmptyURL(t *testing.T) {
	if NewWebhookSender(WebhookConfig{}) != nil {
		t.Fatal("expected nil sender when URL is empty")
	}
	if NewWebhookSender(WebhookConfig{URL: "  "}) != nil {
		t.Fatal("expected nil sender when URL is whitespace-only")
	}
}

func TestWebhookSender_PostsJSONPayload(t *testing.T) {
	var got webhookPayload
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("unmarshal: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := NewWebhookSender(WebhookConfig{URL: srv.URL})
	if s == nil {
		t.Fatal("NewWebhookSender returned nil for valid URL")
	}
	alert := AlertPayload{
		Severity:     5,
		Plate:        "ABC-123",
		PlateHashB64: "AAAA",
		Vehicle:      &alpr.VehicleAttributes{Color: "Silver", Make: "Toyota", Model: "Camry"},
		Evidence: []heuristic.Component{
			{Name: heuristic.ComponentCrossRouteCount, Points: 2.5},
		},
		Route:        "abc1234567890abc|2026-04-27--10-00-00",
		DongleID:     "abc1234567890abc",
		DashboardURL: "https://comma.example.com/alpr/plates/AAAA",
	}
	if err := s.Send(context.Background(), alert); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("server got %d calls, want 1", calls.Load())
	}
	if got.Type != "alpr_alert" {
		t.Errorf("type = %q, want alpr_alert", got.Type)
	}
	if got.Version != WebhookPayloadVersion {
		t.Errorf("version = %q, want %q", got.Version, WebhookPayloadVersion)
	}
	if got.Severity != 5 {
		t.Errorf("severity = %d, want 5", got.Severity)
	}
	if got.Plate != "ABC-123" {
		t.Errorf("plate = %q, want ABC-123", got.Plate)
	}
	if got.PlateHashB64 != "AAAA" {
		t.Errorf("plate_hash_b64 = %q, want AAAA", got.PlateHashB64)
	}
	if got.Vehicle == nil || got.Vehicle.Make != "Toyota" {
		t.Errorf("vehicle missing or wrong: %+v", got.Vehicle)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Name != heuristic.ComponentCrossRouteCount {
		t.Errorf("evidence missing or wrong: %+v", got.Evidence)
	}
	if got.DashboardURL != "https://comma.example.com/alpr/plates/AAAA" {
		t.Errorf("dashboard_url = %q, want exact match", got.DashboardURL)
	}
	if got.SentAt == "" {
		t.Errorf("sent_at empty")
	}
	if _, err := time.Parse(time.RFC3339, got.SentAt); err != nil {
		t.Errorf("sent_at not RFC3339: %v", err)
	}
}

func TestWebhookSender_NonSuccessStatus_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewWebhookSender(WebhookConfig{URL: srv.URL})
	err := s.Send(context.Background(), AlertPayload{Severity: 5, Plate: "ABC-123"})
	if err == nil {
		t.Fatal("expected error on 500 status")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not mention status code", err)
	}
}

func TestWebhookSender_EmptyPlate_RejectedClientSide(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("server should not have been hit")
	}))
	defer srv.Close()

	s := NewWebhookSender(WebhookConfig{URL: srv.URL})
	err := s.Send(context.Background(), AlertPayload{Severity: 5, Plate: ""})
	if err == nil {
		t.Fatal("expected error when plate is empty")
	}
}

func TestBuildWebhookPayload_NilEvidenceMarshalsAsEmptyArray(t *testing.T) {
	p := buildWebhookPayload(AlertPayload{Severity: 4, Plate: "X"}, time.Unix(0, 0).UTC())
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"evidence":[]`) {
		t.Errorf("expected evidence to render as empty array, got: %s", raw)
	}
}

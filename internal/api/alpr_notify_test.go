package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/notify"
)

// stubSender is a minimal Sender for the API-handler test. It records
// nothing and always succeeds; the handler test only cares about the
// envelope, not transport behaviour (transport behaviour is covered by
// dispatcher/email/webhook tests).
type stubSender struct{ name string }

func (s stubSender) Name() string                                        { return s.name }
func (s stubSender) Send(_ context.Context, _ notify.AlertPayload) error { return nil }

func TestALPRNotifyHandler_NoDispatcher_ConfiguredFalse(t *testing.T) {
	h := NewALPRNotifyHandler(nil)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/alpr/notify/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.PostNotifyTest(c); err != nil {
		t.Fatalf("PostNotifyTest error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp alprNotifyTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Configured {
		t.Errorf("Configured = true, want false when no dispatcher wired")
	}
	if len(resp.Results) != 0 {
		t.Errorf("Results = %v, want empty when no senders configured", resp.Results)
	}
}

func TestALPRNotifyHandler_DispatcherWithSenders_ReturnsResults(t *testing.T) {
	d := notify.New(nil, []notify.Sender{
		stubSender{name: "email"},
		stubSender{name: "webhook"},
	}, nil, notify.Config{MinSeverity: 4})
	h := NewALPRNotifyHandler(d)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/alpr/notify/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if err := h.PostNotifyTest(c); err != nil {
		t.Fatalf("PostNotifyTest error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp alprNotifyTestResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Configured {
		t.Errorf("Configured = false, want true when senders are wired")
	}
	if len(resp.Results) != 2 {
		t.Fatalf("Results length = %d, want 2", len(resp.Results))
	}
	for _, r := range resp.Results {
		if !r.OK {
			t.Errorf("sender %q reported failure: %s", r.Sender, r.Error)
		}
	}
	if resp.Results[0].Sender != "email" || resp.Results[1].Sender != "webhook" {
		t.Errorf("sender names out of order: %+v", resp.Results)
	}
}

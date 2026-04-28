package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"comma-personal-backend/internal/alpr/heuristic"
)

// webhookSenderName is the stable Name() value reported by every
// WebhookSender. The test endpoint reports back this string for the
// webhook result row.
const webhookSenderName = "webhook"

// WebhookPayloadVersion is the wire version stamped onto every webhook
// payload. Bump this if the schema changes in a way receivers might
// not parse; consumers should refuse unrecognised versions.
const WebhookPayloadVersion = "1"

// WebhookConfig is the construction-time config of a WebhookSender.
// Empty URL means the sender is not viable; NewWebhookSender returns
// nil in that case so the dispatcher transparently skips it.
type WebhookConfig struct {
	// URL is the HTTPS endpoint the alert payload is POSTed to.
	URL string

	// Timeout caps the entire request (dial + TLS + send + read).
	// Defaults to 10s when zero. The dispatcher's per-send context
	// already enforces 10s; this is the http.Client side of the same
	// budget so a stuck server returns an error rather than starving
	// the dispatcher's wait group.
	Timeout time.Duration

	// HTTPClient is overridable for tests. When nil, a fresh client
	// with the configured Timeout is used. The httptest server in
	// webhook_test.go relies on a real http.Client so we default to
	// the standard one.
	HTTPClient *http.Client
}

// WebhookSender POSTs a JSON-encoded AlertPayload to a user-provided
// URL with a 10s timeout. The payload schema is documented inline with
// webhookPayload below; any change to those fields requires a
// WebhookPayloadVersion bump.
type WebhookSender struct {
	cfg    WebhookConfig
	client *http.Client
}

// NewWebhookSender returns a configured sender, or nil when the URL is
// empty. Mirroring NewEmailSender's pattern lets the dispatcher iterate
// over [emailSender, webhookSender] without nil-checking each entry.
func NewWebhookSender(cfg WebhookConfig) *WebhookSender {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &WebhookSender{cfg: cfg, client: client}
}

// Name implements Sender.
func (s *WebhookSender) Name() string { return webhookSenderName }

// Send implements Sender. It POSTs a fresh payload (with a server-time
// SentAt) on every call, even for retried alerts; receivers are
// expected to dedup on PlateHashB64 + SentAt minute precision, but the
// dispatcher's own dedup ledger already prevents most retries.
func (s *WebhookSender) Send(ctx context.Context, alert AlertPayload) error {
	if alert.Plate == "" {
		return errors.New("notify/webhook: refusing to send with empty plate text")
	}
	body, err := json.Marshal(buildWebhookPayload(alert, time.Now().UTC()))
	if err != nil {
		return fmt.Errorf("notify/webhook: marshal payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify/webhook: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "comma-personal-backend/alpr-notify")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("notify/webhook: POST %s: %w", s.cfg.URL, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify/webhook: %s returned status %d", s.cfg.URL, resp.StatusCode)
	}
	return nil
}

// webhookPayload is the on-the-wire shape. Keep field names stable;
// renaming any of them requires a WebhookPayloadVersion bump and a doc
// note. New fields are additive (existing receivers ignore them).
type webhookPayload struct {
	Type         string                `json:"type"`
	Version      string                `json:"version"`
	Severity     int                   `json:"severity"`
	PlateHashB64 string                `json:"plate_hash_b64"`
	Plate        string                `json:"plate"`
	Evidence     []heuristic.Component `json:"evidence"`
	Route        string                `json:"route,omitempty"`
	DongleID     string                `json:"dongle_id,omitempty"`
	DashboardURL string                `json:"dashboard_url,omitempty"`
	SentAt       string                `json:"sent_at"`
}

// buildWebhookPayload converts an AlertPayload to the wire shape. Kept
// as a free function (rather than a method) so the test endpoint can
// reuse it for synthetic alerts.
func buildWebhookPayload(alert AlertPayload, sentAt time.Time) webhookPayload {
	// Always emit Evidence as a non-nil slice so receivers can rely on
	// the field's presence. nil json-marshals to null which is
	// inconvenient for typed clients.
	evidence := alert.Evidence
	if evidence == nil {
		evidence = []heuristic.Component{}
	}
	return webhookPayload{
		Type:         "alpr_alert",
		Version:      WebhookPayloadVersion,
		Severity:     alert.Severity,
		PlateHashB64: alert.PlateHashB64,
		Plate:        alert.Plate,
		Evidence:     evidence,
		Route:        alert.Route,
		DongleID:     alert.DongleID,
		DashboardURL: alert.DashboardURL,
		SentAt:       sentAt.Format(time.RFC3339),
	}
}

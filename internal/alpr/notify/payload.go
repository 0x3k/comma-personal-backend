// Package notify dispatches out-of-band ALPR alert notifications (email
// and webhook) when the stalking heuristic produces a high-severity
// alert. It is intentionally narrow:
//
//   - Sender is a simple "send one alert" interface so the dispatcher
//     does not need to know about transport-specific details.
//   - The dispatcher applies the severity threshold + per-plate dedup
//     window + whitelist suppression; senders only render and transmit.
//   - Failures are logged at warn but never block other senders or the
//     upstream heuristic worker. The dedup ledger is updated on any
//     successful send so a partial failure does not re-fire the
//     surviving channel on the next event.
//
// The package is event-driven (subscribed via cmd/server.startWorkers
// onto deps.alprAlertCreated). It does NOT poll the database or own its
// own goroutine pool: every Dispatch is initiated by an upstream event
// or by the operator-facing test endpoint.
//
// Privacy: plate text is decrypted only at the boundary of an outbound
// payload and is never logged. The dispatcher refuses to construct a
// payload with empty plate text; that is treated as a transient error
// and the alert is dropped (a future event will re-fire once the
// detection ciphertext is available).
package notify

import (
	"comma-personal-backend/internal/alpr"
	"comma-personal-backend/internal/alpr/heuristic"
)

// AlertPayload is the in-memory shape every sender consumes. It carries
// the decrypted plate text (the user is the trusted recipient of their
// own outbound channel) plus the heuristic evidence so the operator can
// quickly judge whether the alert is real or a false positive.
//
// The exact JSON schema for the webhook is encoded in WebhookSender.Send;
// AlertPayload is the internal representation, not the wire shape.
type AlertPayload struct {
	// Severity is the integer severity 0..5 emitted by the heuristic.
	// The dispatcher's threshold filter has already accepted the
	// payload by the time it reaches a Sender.
	Severity int

	// Plate is the decrypted plate text. Always non-empty (the
	// dispatcher refuses payloads with empty plate text). The label
	// "TEST-123" is permitted via the test endpoint.
	Plate string

	// PlateHashB64 is the base64-URL-encoded plate hash (no padding) so
	// the payload can deep-link into the plate detail page.
	PlateHashB64 string

	// Vehicle, when non-nil, is the canonical vehicle attribute set
	// for this plate (make / model / color / body type). Nil renders
	// as "Vehicle attributes unknown" in the email body.
	Vehicle *alpr.VehicleAttributes

	// Evidence is the heuristic's component breakdown ("why we
	// alerted"). The order of entries mirrors the order Score
	// produced. Nil-safe -- the email/webhook formatters handle the
	// empty case by emitting a stub "no evidence available" line.
	Evidence []heuristic.Component

	// Route is the route id that fired this alert. Empty when the
	// alert came from a non-route source (currently only the test
	// endpoint).
	Route string

	// DongleID is the device the encounter belongs to. Useful in
	// multi-device installs where the operator wants to know which
	// vehicle reported the encounter.
	DongleID string

	// DashboardURL is the absolute URL of the plate detail page in
	// the operator's dashboard. Empty when ALPR_DASHBOARD_URL is
	// unset; in that case the formatters omit the deep link rather
	// than emit a half-built URL.
	DashboardURL string
}

// VehicleBadge returns a short human-readable summary of the vehicle
// attributes ("Silver Toyota Camry") or a fixed unknown label when the
// engine produced no attributes for this plate. Used by the email body.
//
// Format: "<Color> <Make> <Model>" with empty fields squeezed out so a
// nil-or-mostly-empty Vehicle does not render as "  Camry". An entirely
// empty Vehicle (or nil) renders as "Vehicle attributes unknown".
func (p AlertPayload) VehicleBadge() string {
	if p.Vehicle == nil {
		return "Vehicle attributes unknown"
	}
	parts := make([]string, 0, 3)
	for _, s := range []string{p.Vehicle.Color, p.Vehicle.Make, p.Vehicle.Model} {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "Vehicle attributes unknown"
	}
	out := parts[0]
	for _, s := range parts[1:] {
		out = out + " " + s
	}
	return out
}

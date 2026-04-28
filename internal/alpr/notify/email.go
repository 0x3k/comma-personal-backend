package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"comma-personal-backend/internal/alpr/heuristic"
)

// EmailConfig is the construction-time configuration of EmailSender.
// All fields except Auth are mirrored 1:1 from ALPRConfig.NotifySMTP*.
// Empty Host / empty To means the email sender is not viable; callers
// should not construct an EmailSender in that case (NewEmailSender
// returns nil and the dispatcher skips it).
type EmailConfig struct {
	// Host is the SMTP relay hostname. Required.
	Host string
	// Port is the SMTP relay port. Required (defaults to 587 in
	// alprDefaults; the caller passes the resolved value here).
	Port int
	// Username / Password are the auth credentials. Both empty means
	// anonymous submission; for STARTTLS / implicit-TLS modes that
	// the relay accepts (uncommon but legitimate for an internal LAN
	// relay).
	Username string
	Password string
	// From is the sender address. Required (the sender refuses to
	// start with an empty From; the caller falls back to Username).
	From string
	// To is the comma-separated list of recipients. Required.
	To string
	// TLSMode is one of "starttls" (default), "implicit", or "plain".
	// Validated by config.LoadALPR before reaching this struct.
	TLSMode string

	// DialTimeout caps the TCP dial. Defaults to 10s when zero. The
	// dispatcher's 10s ctx already bounds the call; this is a fail-
	// fast on a black-holed relay so we don't spend the entire
	// dispatcher budget on the connect.
	DialTimeout time.Duration
}

// emailSenderName is the stable Name() value reported by every
// EmailSender. The test endpoint uses this string in its results array.
const emailSenderName = "email"

// EmailSender renders + transmits an alert as both a text/plain and a
// text/html email part using net/smtp. It speaks the three TLS modes
// supported by config (starttls / implicit / plain) and authenticates
// via PLAIN when Username is set. Construct via NewEmailSender so
// configuration is validated before the dispatcher takes a reference.
type EmailSender struct {
	cfg EmailConfig

	// dial is overridable for tests. Defaults to net.Dialer.DialContext.
	// Returns a raw TCP conn the sender then wraps in tls.Client (for
	// implicit TLS) or hands directly to smtp.NewClient (for starttls
	// or plain).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)

	// tlsConfig is the shared *tls.Config for both STARTTLS and
	// implicit TLS connections. Defaults to a config with
	// ServerName=Host so the relay's certificate is verified against
	// the configured hostname.
	tlsConfig *tls.Config
}

// NewEmailSender returns a configured sender or nil when the config is
// not viable (empty Host, empty To, or empty effective From). The nil
// return makes "email is not configured" indistinguishable from "sender
// constructor was never called", which is what the dispatcher wants:
// it iterates over a slice of non-nil senders.
func NewEmailSender(cfg EmailConfig) *EmailSender {
	if cfg.Host == "" || cfg.To == "" {
		return nil
	}
	if cfg.From == "" {
		cfg.From = cfg.Username
	}
	if cfg.From == "" {
		return nil
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.TLSMode == "" {
		cfg.TLSMode = "starttls"
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &EmailSender{
		cfg:  cfg,
		dial: (&net.Dialer{Timeout: cfg.DialTimeout}).DialContext,
		tlsConfig: &tls.Config{
			ServerName: cfg.Host,
			MinVersion: tls.VersionTLS12,
		},
	}
}

// Name implements Sender.
func (s *EmailSender) Name() string { return emailSenderName }

// Send implements Sender.
func (s *EmailSender) Send(ctx context.Context, alert AlertPayload) error {
	if alert.Plate == "" {
		return errors.New("notify/email: refusing to send with empty plate text")
	}
	addr := net.JoinHostPort(s.cfg.Host, strconv.Itoa(s.cfg.Port))

	conn, err := s.dial(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("notify/email: dial %s: %w", addr, err)
	}
	// Apply the dispatcher's deadline to the underlying conn so a
	// stalled relay does not exceed the per-send budget. The 10s ctx
	// the dispatcher hands us is the upper bound; any caller-set
	// deadline narrower than that wins.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	defer conn.Close()

	switch strings.ToLower(s.cfg.TLSMode) {
	case "implicit":
		conn = tls.Client(conn, s.tlsConfig)
	}

	client, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		return fmt.Errorf("notify/email: smtp NewClient: %w", err)
	}
	defer client.Quit() //nolint:errcheck // best-effort close

	if strings.EqualFold(s.cfg.TLSMode, "starttls") {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("notify/email: relay does not advertise STARTTLS but TLS mode is starttls")
		}
		if err := client.StartTLS(s.tlsConfig); err != nil {
			return fmt.Errorf("notify/email: STARTTLS: %w", err)
		}
	}

	if s.cfg.Username != "" {
		auth := smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("notify/email: auth: %w", err)
		}
	}

	if err := client.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("notify/email: MAIL FROM: %w", err)
	}

	recipients := splitRecipients(s.cfg.To)
	if len(recipients) == 0 {
		return errors.New("notify/email: no recipients configured")
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("notify/email: RCPT TO %s: %w", rcpt, err)
		}
	}

	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("notify/email: DATA: %w", err)
	}
	if _, err := wc.Write(buildEmailMessage(s.cfg.From, recipients, alert)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("notify/email: write body: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("notify/email: close DATA: %w", err)
	}
	return nil
}

// splitRecipients parses the comma-separated To list into trimmed
// non-empty addresses. A single-address To string is the common case;
// the comma path is here so a small ops team can share an alias
// without standing up a mailing list.
func splitRecipients(to string) []string {
	parts := strings.Split(to, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildEmailMessage renders the alert as a multipart/alternative
// message with text/plain + text/html parts. The format is intentionally
// boring: a simple boundary, no MIME-Word encoding for the subject (the
// content is ASCII), and no quoted-printable encoding (most modern
// relays accept 8bit; a strict 7bit relay would still receive the
// payload as the characters used here are all printable ASCII).
//
// Returned bytes include the SMTP-style headers and a trailing CRLF.CRLF
// is supplied separately by the smtp.DataWriter close.
func buildEmailMessage(from string, to []string, alert AlertPayload) []byte {
	const boundary = "alpr-notify-boundary"

	sanitizedTo := make([]string, len(to))
	for i, addr := range to {
		sanitizedTo[i] = sanitizeHeaderValue(addr)
	}

	var b bytes.Buffer
	b.WriteString("From: " + sanitizeHeaderValue(from) + "\r\n")
	b.WriteString("To: " + strings.Join(sanitizedTo, ", ") + "\r\n")
	b.WriteString("Subject: " + buildSubject(alert) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/alternative; boundary=\"" + boundary + "\"\r\n")
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(buildTextBody(alert))
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=\"utf-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(buildHTMLBody(alert))
	b.WriteString("\r\n")

	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

// buildSubject is the canonical subject line; the dispatcher's test
// endpoint reuses this format so a synthetic alert is visually
// indistinguishable from a real one (apart from the plate text).
//
// Plate text is sanitized before concatenation: it reaches us from the
// engine's OCR JSON or the manual-correction endpoint, neither of
// which strips CR/LF, so a raw concat would let an attacker inject
// extra SMTP headers (Bcc, From override, body splitting). Subjects
// are header-position, not body-position.
func buildSubject(alert AlertPayload) string {
	return "ALPR alert: severity " + strconv.Itoa(alert.Severity) + " - " + sanitizeHeaderValue(alert.Plate)
}

// sanitizeHeaderValue replaces CR/LF and NUL with spaces so a value
// concatenated into an SMTP header cannot terminate the header or
// inject additional ones. Replacement (rather than rejection) keeps
// the sender from silently dropping notifications when an OCR run
// emits a malformed plate string; the body / dashboard link remain
// the source of truth for the operator and an unusual subject is a
// visible signal worth investigating.
func sanitizeHeaderValue(s string) string {
	r := strings.NewReplacer(
		"\r", " ",
		"\n", " ",
		"\x00", " ",
	)
	return r.Replace(s)
}

// buildTextBody is the text/plain alternative. Plain text fields are
// preferable for forwarding into other systems (chat, ticketing) that
// do not parse HTML.
func buildTextBody(alert AlertPayload) string {
	var b strings.Builder
	b.WriteString("ALPR alert\n")
	b.WriteString("Severity: " + strconv.Itoa(alert.Severity) + "\n")
	b.WriteString("Plate: " + alert.Plate + "\n")
	b.WriteString("Vehicle: " + alert.VehicleBadge() + "\n")
	b.WriteString("Evidence: " + summarizeEvidence(alert.Evidence) + "\n")
	if alert.Route != "" {
		b.WriteString("Route: " + alert.Route + "\n")
	}
	if alert.DongleID != "" {
		b.WriteString("Device: " + alert.DongleID + "\n")
	}
	if len(alert.Evidence) > 0 {
		b.WriteString("\nEvidence detail:\n")
		for _, c := range alert.Evidence {
			b.WriteString("  - " + c.Name + " (+" + strconv.FormatFloat(c.Points, 'f', 2, 64) + ")")
			if len(c.Evidence) > 0 {
				if raw, err := json.Marshal(c.Evidence); err == nil {
					b.WriteString(" " + string(raw))
				}
			}
			b.WriteString("\n")
		}
	}
	if alert.DashboardURL != "" {
		b.WriteString("\nView in dashboard: " + alert.DashboardURL + "\n")
	}
	b.WriteString("\nYou are receiving this because ALPR notifications are enabled. Configure or disable in Settings.\n")
	return b.String()
}

// buildHTMLBody is the text/html alternative. Kept deliberately simple
// (no inline CSS, no remote images) so the message renders cleanly in
// most clients without crossing into "tracking pixel" territory.
func buildHTMLBody(alert AlertPayload) string {
	var b strings.Builder
	b.WriteString("<!doctype html><html><body>")
	b.WriteString("<h2>ALPR alert</h2>")
	b.WriteString("<p><strong>Severity:</strong> " + strconv.Itoa(alert.Severity) + "</p>")
	b.WriteString("<p><strong>Plate:</strong> " + htmlEscape(alert.Plate) + "</p>")
	b.WriteString("<p><strong>Vehicle:</strong> " + htmlEscape(alert.VehicleBadge()) + "</p>")
	b.WriteString("<p><strong>Evidence:</strong> " + htmlEscape(summarizeEvidence(alert.Evidence)) + "</p>")
	if alert.Route != "" {
		b.WriteString("<p><strong>Route:</strong> " + htmlEscape(alert.Route) + "</p>")
	}
	if alert.DongleID != "" {
		b.WriteString("<p><strong>Device:</strong> " + htmlEscape(alert.DongleID) + "</p>")
	}
	if len(alert.Evidence) > 0 {
		b.WriteString("<p><strong>Evidence detail:</strong></p><ul>")
		for _, c := range alert.Evidence {
			b.WriteString("<li>")
			b.WriteString(htmlEscape(c.Name))
			b.WriteString(" (+" + strconv.FormatFloat(c.Points, 'f', 2, 64) + ")")
			if len(c.Evidence) > 0 {
				if raw, err := json.Marshal(c.Evidence); err == nil {
					b.WriteString(" <code>" + htmlEscape(string(raw)) + "</code>")
				}
			}
			b.WriteString("</li>")
		}
		b.WriteString("</ul>")
	}
	if alert.DashboardURL != "" {
		// The URL is operator-controlled (ALPR_DASHBOARD_URL +
		// known-safe path), so we link it directly. No untrusted
		// input lands in href.
		b.WriteString("<p><a href=\"" + htmlEscape(alert.DashboardURL) + "\">View in dashboard</a></p>")
	}
	b.WriteString("<p style=\"color:#666;font-size:smaller\">You are receiving this because ALPR notifications are enabled. Configure or disable in Settings.</p>")
	b.WriteString("</body></html>")
	return b.String()
}

// summarizeEvidence is the one-line summary used in the email subject
// flavour text and the body opener. It joins each component name with a
// "+" so the operator can scan severity composition at a glance.
func summarizeEvidence(comps []heuristic.Component) string {
	if len(comps) == 0 {
		return "no evidence components recorded"
	}
	parts := make([]string, 0, len(comps))
	for _, c := range comps {
		parts = append(parts, c.Name)
	}
	return strings.Join(parts, " + ")
}

// htmlEscape is a thin local wrapper so we do not import html/template
// for two-character substitutions. Sufficient for the strict subset of
// content we render (plate text, vehicle badge, evidence keys --
// none of which legitimately contain HTML markup).
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

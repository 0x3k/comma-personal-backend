package notify

import (
	"strings"
	"testing"

	"comma-personal-backend/internal/alpr"
	"comma-personal-backend/internal/alpr/heuristic"
)

func TestNewEmailSender_NilOnMissingHost(t *testing.T) {
	if NewEmailSender(EmailConfig{To: "user@example.com", From: "alerts@example.com"}) != nil {
		t.Fatal("expected nil sender when Host is empty")
	}
}

func TestNewEmailSender_NilOnMissingTo(t *testing.T) {
	if NewEmailSender(EmailConfig{Host: "smtp.example.com", From: "alerts@example.com"}) != nil {
		t.Fatal("expected nil sender when To is empty")
	}
}

func TestNewEmailSender_FromFallsBackToUsername(t *testing.T) {
	s := NewEmailSender(EmailConfig{
		Host:     "smtp.example.com",
		To:       "user@example.com",
		Username: "alerts@example.com",
	})
	if s == nil {
		t.Fatal("expected non-nil sender when Username supplies From")
	}
	if s.cfg.From != "alerts@example.com" {
		t.Errorf("From = %q, want fallback to Username", s.cfg.From)
	}
}

func TestNewEmailSender_NilWhenNoFromOrUsername(t *testing.T) {
	if NewEmailSender(EmailConfig{Host: "smtp.example.com", To: "user@example.com"}) != nil {
		t.Fatal("expected nil sender when both From and Username are empty")
	}
}

func TestBuildEmailMessage_ContainsRequiredFields(t *testing.T) {
	alert := AlertPayload{
		Severity:     5,
		Plate:        "ABC-123",
		PlateHashB64: "AAAA",
		Vehicle:      &alpr.VehicleAttributes{Color: "Silver", Make: "Toyota", Model: "Camry"},
		Evidence: []heuristic.Component{
			{Name: heuristic.ComponentCrossRouteCount, Points: 2.5, Evidence: map[string]any{"distinct_routes": 5}},
		},
		Route:        "abc1234567890abc|2026-04-27--10-00-00",
		DongleID:     "abc1234567890abc",
		DashboardURL: "https://comma.example.com/alpr/plates/AAAA",
	}
	msg := string(buildEmailMessage("alerts@example.com", []string{"user@example.com"}, alert))

	wantSubstrings := []string{
		"From: alerts@example.com",
		"To: user@example.com",
		"Subject: ALPR alert: severity 5 - ABC-123",
		"multipart/alternative",
		"text/plain",
		"text/html",
		"ABC-123",
		"Silver Toyota Camry",
		heuristic.ComponentCrossRouteCount,
		"abc1234567890abc",
		"comma.example.com/alpr/plates/AAAA",
		"You are receiving this because ALPR notifications are enabled",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\n--- message ---\n%s", want, msg)
		}
	}
}

func TestBuildEmailMessage_VehicleUnknownBadge(t *testing.T) {
	alert := AlertPayload{
		Severity: 4,
		Plate:    "XYZ-789",
	}
	msg := string(buildEmailMessage("alerts@example.com", []string{"user@example.com"}, alert))
	if !strings.Contains(msg, "Vehicle attributes unknown") {
		t.Errorf("missing unknown-vehicle badge in:\n%s", msg)
	}
}

func TestBuildEmailMessage_HTMLEscapesPlateText(t *testing.T) {
	alert := AlertPayload{
		Severity: 5,
		// Synthetic plate-shaped text that would break HTML output if
		// not escaped. Real plates do not contain '<' or '>', but the
		// escape path is a defense-in-depth.
		Plate: "<script>alert(1)</script>",
	}
	msg := string(buildEmailMessage("alerts@example.com", []string{"user@example.com"}, alert))
	if strings.Contains(msg, "<script>alert(1)</script>") {
		// We expect the HTML body to escape, but the text body
		// renders as-is. Look for the escaped form to confirm the
		// HTML part is safe.
		if !strings.Contains(msg, "&lt;script&gt;") {
			t.Errorf("HTML body does not escape script tags:\n%s", msg)
		}
	}
}

func TestSplitRecipients_HandlesMultiple(t *testing.T) {
	got := splitRecipients(" a@example.com , b@example.com ,, c@example.com ")
	want := []string{"a@example.com", "b@example.com", "c@example.com"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildEmailMessage_StripsCRLFFromHeaders(t *testing.T) {
	// A plate string carrying CR/LF must not break out of the Subject
	// header. normalizePlateForCheck (the manual-correction validator)
	// only uppercases and strips space/dash/dot/tab, so a malicious or
	// compromised engine can submit "AAA\r\nBcc: attacker@x" and have
	// it round-trip to here. The sanitizer collapses CR/LF to spaces
	// so the Subject stays on a single line and no header injection
	// is possible.
	alert := AlertPayload{
		Severity: 5,
		Plate:    "AAA\r\nBcc: attacker@example.com",
	}
	msg := string(buildEmailMessage(
		"alerts@example.com\r\nBcc: from-attacker@example.com",
		[]string{"user@example.com\r\nBcc: to-attacker@example.com"},
		alert,
	))

	headers, _, ok := strings.Cut(msg, "\r\n\r\n")
	if !ok {
		t.Fatalf("no header/body separator in:\n%s", msg)
	}

	// What we care about: the attacker's text MUST NOT appear as a
	// standalone header line. After sanitization the literal substring
	// "Bcc: ..." can still appear inside the From / To / Subject value
	// (because we replace CR/LF with space rather than drop the rest
	// of the string), but no SMTP relay will treat it as a header.
	for line := range strings.SplitSeq(headers, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") || strings.HasPrefix(line, "bcc:") {
			t.Errorf("header injection: standalone Bcc line in header section:\n%s", headers)
		}
	}

	// Subject / From / To must each be exactly one line (i.e. contain
	// no embedded CR/LF). This is the load-bearing invariant for
	// header-injection prevention.
	for line := range strings.SplitSeq(headers, "\r\n") {
		switch {
		case strings.HasPrefix(line, "Subject:"),
			strings.HasPrefix(line, "From:"),
			strings.HasPrefix(line, "To:"):
			if strings.ContainsAny(line, "\r\n") {
				t.Errorf("header line contains CR/LF: %q", line)
			}
		}
	}
}

func TestSanitizeHeaderValue(t *testing.T) {
	cases := map[string]string{
		"plain":               "plain",
		"with\rcr":            "with cr",
		"with\nlf":            "with lf",
		"with\r\ncrlf":        "with  crlf",
		"with\x00nul":         "with nul",
		"AAA\r\nBcc: x@y.com": "AAA  Bcc: x@y.com",
		"":                    "",
	}
	for in, want := range cases {
		if got := sanitizeHeaderValue(in); got != want {
			t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestVehicleBadge_PartialFields(t *testing.T) {
	cases := []struct {
		name string
		v    *alpr.VehicleAttributes
		want string
	}{
		{"nil", nil, "Vehicle attributes unknown"},
		{"empty", &alpr.VehicleAttributes{}, "Vehicle attributes unknown"},
		{"color only", &alpr.VehicleAttributes{Color: "Silver"}, "Silver"},
		{"make+model", &alpr.VehicleAttributes{Make: "Toyota", Model: "Camry"}, "Toyota Camry"},
		{"all", &alpr.VehicleAttributes{Color: "Silver", Make: "Toyota", Model: "Camry"}, "Silver Toyota Camry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := AlertPayload{Vehicle: tc.v}.VehicleBadge()
			if got != tc.want {
				t.Errorf("VehicleBadge() = %q, want %q", got, tc.want)
			}
		})
	}
}

package geocode

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient returns a Client pointed at srv with a tiny HTTP timeout and
// no rate limit. Tests that care about rate limiting override minInterval
// explicitly.
func newTestClient(srv *httptest.Server) *Client {
	c := NewClient(srv.URL, "geocode-test/1.0 (+contact@example.com)")
	c.HTTP = srv.Client()
	c.HTTP.Timeout = 2 * time.Second
	c.minInterval = 0
	return c
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("", "")
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL: got %q, want %q", c.BaseURL, DefaultBaseURL)
	}
	if c.UserAgent != DefaultUserAgent {
		t.Errorf("UserAgent: got %q, want %q", c.UserAgent, DefaultUserAgent)
	}
	if c.HTTP == nil {
		t.Fatal("HTTP client is nil")
	}
	if c.HTTP.Timeout != defaultTimeout {
		t.Errorf("HTTP.Timeout: got %v, want %v", c.HTTP.Timeout, defaultTimeout)
	}
	if c.minInterval != defaultMinInterval {
		t.Errorf("minInterval: got %v, want %v", c.minInterval, defaultMinInterval)
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	c := NewClient("https://example.org/nominatim/", "ua")
	if c.BaseURL != "https://example.org/nominatim" {
		t.Errorf("BaseURL: got %q, want trailing slash trimmed", c.BaseURL)
	}
}

func TestReverseSuccess(t *testing.T) {
	body := `{
		"display_name": "Hayes Valley, San Francisco, California, United States",
		"address": {
			"suburb": "Hayes Valley",
			"city": "San Francisco",
			"state": "California",
			"country": "United States"
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reverse" {
			t.Errorf("path: got %q, want /reverse", r.URL.Path)
		}
		if r.URL.Query().Get("lat") != "37.7759" {
			t.Errorf("lat: got %q, want 37.7759", r.URL.Query().Get("lat"))
		}
		if r.URL.Query().Get("lon") != "-122.4245" {
			t.Errorf("lon: got %q, want -122.4245", r.URL.Query().Get("lon"))
		}
		if r.URL.Query().Get("format") == "" {
			t.Error("format query param is missing")
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("User-Agent header is missing")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestClient(srv)

	got, err := c.Reverse(context.Background(), 37.7759, -122.4245)
	if err != nil {
		t.Fatalf("Reverse: unexpected error: %v", err)
	}
	want := "Hayes Valley, San Francisco, United States"
	if got != want {
		t.Errorf("Reverse: got %q, want %q", got, want)
	}
}

func TestReverseFallsBackToDisplayName(t *testing.T) {
	// No structured address components; must fall back to display_name.
	body := `{"display_name": "Some Place, Country", "address": {}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestClient(srv)

	got, err := c.Reverse(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("Reverse: unexpected error: %v", err)
	}
	if got != "Some Place, Country" {
		t.Errorf("Reverse: got %q, want fallback to display_name", got)
	}
}

func TestReverseNoResult(t *testing.T) {
	// Nominatim returns 200 with an "error" field when it finds no result.
	body := `{"error": "Unable to geocode"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newTestClient(srv)

	_, err := c.Reverse(context.Background(), 0, 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reverse: got %v, want ErrNotFound", err)
	}
}

func TestReverseEmptyPayload(t *testing.T) {
	// An empty JSON object has neither error nor usable fields; treat as not found.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := newTestClient(srv)

	_, err := c.Reverse(context.Background(), 0, 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reverse: got %v, want ErrNotFound", err)
	}
}

func TestReverseHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(srv)

	_, err := c.Reverse(context.Background(), 0, 0)
	if err == nil {
		t.Fatal("Reverse: expected error for 500 response")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("Reverse: HTTP error should not map to ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("Reverse: error should mention status code, got %v", err)
	}
}

func TestReverseRateLimitSpacing(t *testing.T) {
	// Two consecutive calls must be spaced at least 1 second apart, matching
	// Nominatim's public usage policy. This test uses the real default
	// minInterval to exercise the production behavior end-to-end.
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		_, _ = w.Write([]byte(`{"display_name": "X", "address": {"country": "X"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "rate-limit-test/1.0")
	c.HTTP = srv.Client()
	c.HTTP.Timeout = 2 * time.Second
	// Keep the default minInterval (1s) so we validate real production spacing.

	ctx := context.Background()
	start := time.Now()
	if _, err := c.Reverse(ctx, 1, 1); err != nil {
		t.Fatalf("first Reverse: %v", err)
	}
	if _, err := c.Reverse(ctx, 2, 2); err != nil {
		t.Fatalf("second Reverse: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 1*time.Second {
		t.Errorf("two calls completed in %v, want >= 1s", elapsed)
	}
	if got := atomic.LoadInt32(&count); got != 2 {
		t.Errorf("server saw %d requests, want 2", got)
	}
}

func TestReverseRateLimitRespectsContext(t *testing.T) {
	// When the caller's context is canceled while waiting out the rate
	// limit, Reverse must return promptly with ctx.Err().
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"display_name": "X", "address": {"country": "X"}}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "rate-limit-ctx-test/1.0")
	c.HTTP = srv.Client()
	c.minInterval = 5 * time.Second

	// Prime lastCall so the next call must wait.
	if _, err := c.Reverse(context.Background(), 1, 1); err != nil {
		t.Fatalf("first Reverse: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Reverse(ctx, 2, 2)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Reverse: got %v, want context.DeadlineExceeded", err)
	}
}

func TestFormatDisplayPrefersStructuredAddress(t *testing.T) {
	r := nominatimResponse{
		DisplayName: "ignored",
		Address: address{
			Suburb:  "SoMa",
			City:    "San Francisco",
			Country: "United States",
		},
	}
	if got := formatDisplay(r); got != "SoMa, San Francisco, United States" {
		t.Errorf("formatDisplay: got %q", got)
	}
}

func TestFormatDisplayDedupesSuburbAndCity(t *testing.T) {
	r := nominatimResponse{
		Address: address{
			Suburb:  "Berlin",
			City:    "Berlin",
			Country: "Germany",
		},
	}
	if got := formatDisplay(r); got != "Berlin, Germany" {
		t.Errorf("formatDisplay: got %q", got)
	}
}

func TestFormatDisplayFallsBackToVillage(t *testing.T) {
	r := nominatimResponse{
		Address: address{
			Village: "Little Hamlet",
			Country: "Scotland",
		},
	}
	if got := formatDisplay(r); got != "Little Hamlet, Scotland" {
		t.Errorf("formatDisplay: got %q", got)
	}
}

// Package geocode provides a reusable reverse-geocoding client backed by
// an OpenStreetMap Nominatim endpoint. Consumers pass a latitude/longitude
// pair and receive a short human-readable address string.
//
// The default endpoint is the public Nominatim service. Operators running
// a high-volume server should point at their own Nominatim instance by
// supplying a custom base URL, since the public service imposes a strict
// usage policy (1 request per second, meaningful User-Agent required).
package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is the public Nominatim service endpoint.
const DefaultBaseURL = "https://nominatim.openstreetmap.org"

// DefaultUserAgent is used when the caller does not supply a User-Agent.
// Nominatim's usage policy requires a meaningful, contactable identifier;
// callers should override this with something specific to their deployment.
const DefaultUserAgent = "comma-personal-backend/1.0 (+https://github.com/commaai/openpilot)"

// defaultTimeout is the per-request HTTP timeout used when the caller does
// not supply a custom http.Client.
const defaultTimeout = 1 * time.Second

// defaultMinInterval is the minimum spacing between outgoing requests. It
// matches Nominatim's public usage policy of 1 request per second.
const defaultMinInterval = 1 * time.Second

// ErrNotFound is returned when Nominatim responds successfully but has no
// matching address for the supplied coordinates.
var ErrNotFound = errors.New("geocode: no result for coordinates")

// Client performs reverse-geocoding lookups against a Nominatim-compatible
// HTTP endpoint. A zero-value Client is not usable; construct one with
// NewClient.
//
// All exported fields may be overridden after construction, but callers
// must not do so while a Reverse call is in flight.
type Client struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client

	// minInterval is the minimum spacing between outgoing requests.
	// It is not exported to keep the public surface small; tests that
	// need a reduced interval use the internal helper.
	minInterval time.Duration

	mu       sync.Mutex
	lastCall time.Time
}

// NewClient constructs a Client rooted at baseURL. If baseURL is empty,
// DefaultBaseURL is used. If userAgent is empty, DefaultUserAgent is used.
// The returned client enforces a 1-second rate limit and a 1-second HTTP
// timeout by default.
func NewClient(baseURL, userAgent string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	return &Client{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		UserAgent:   userAgent,
		HTTP:        &http.Client{Timeout: defaultTimeout},
		minInterval: defaultMinInterval,
	}
}

// nominatimResponse models the subset of the Nominatim /reverse JSON
// response we care about. Nominatim returns an error field instead of
// 404 when no result is found.
type nominatimResponse struct {
	Error       string  `json:"error,omitempty"`
	DisplayName string  `json:"display_name,omitempty"`
	Address     address `json:"address,omitempty"`
}

// address captures the most common Nominatim address components. Real
// responses contain many more fields, but these are sufficient to build
// a short display string.
type address struct {
	Suburb        string `json:"suburb,omitempty"`
	Neighbourhood string `json:"neighbourhood,omitempty"`
	Village       string `json:"village,omitempty"`
	Town          string `json:"town,omitempty"`
	City          string `json:"city,omitempty"`
	County        string `json:"county,omitempty"`
	State         string `json:"state,omitempty"`
	Country       string `json:"country,omitempty"`
}

// Reverse looks up (lat, lng) and returns a short display string. When
// Nominatim returns no result, ErrNotFound is returned. Any other failure
// (network error, non-2xx HTTP status, malformed JSON) is wrapped and
// returned verbatim.
//
// Reverse serializes calls through a per-client mutex and enforces the
// configured rate limit, so concurrent callers will queue rather than
// overwhelm the remote service.
func (c *Client) Reverse(ctx context.Context, lat, lng float64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.waitLocked(ctx); err != nil {
		return "", err
	}

	endpoint, err := c.buildURL(lat, lng)
	if err != nil {
		return "", fmt.Errorf("geocode: build url: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("geocode: build request: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	// Record the completion time regardless of success so a failure still
	// counts against the rate budget.
	c.lastCall = time.Now()
	if err != nil {
		return "", fmt.Errorf("geocode: http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("geocode: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed nominatimResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", fmt.Errorf("geocode: decode response: %w", err)
	}

	if parsed.Error != "" {
		return "", ErrNotFound
	}

	if display := formatDisplay(parsed); display != "" {
		return display, nil
	}
	return "", ErrNotFound
}

// waitLocked blocks until it is safe to issue the next outgoing request.
// It must be called with c.mu held. The wait respects ctx cancellation.
func (c *Client) waitLocked(ctx context.Context) error {
	if c.minInterval <= 0 || c.lastCall.IsZero() {
		return nil
	}
	elapsed := time.Since(c.lastCall)
	if elapsed >= c.minInterval {
		return nil
	}
	remaining := c.minInterval - elapsed
	t := time.NewTimer(remaining)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// buildURL assembles a /reverse query with JSON formatting and the given
// coordinates. Nominatim expects lat/lon as query parameters.
func (c *Client) buildURL(lat, lng float64) (string, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/reverse"

	q := u.Query()
	q.Set("format", "jsonv2")
	q.Set("lat", strconv.FormatFloat(lat, 'f', -1, 64))
	q.Set("lon", strconv.FormatFloat(lng, 'f', -1, 64))
	q.Set("zoom", "14")
	q.Set("addressdetails", "1")
	u.RawQuery = q.Encode()

	return u.String(), nil
}

// formatDisplay builds a short human-readable string from the Nominatim
// response, preferring suburb+city+country and falling back to display_name.
// Blank components are skipped and duplicate adjacent components (e.g.
// suburb equal to city) are deduplicated.
func formatDisplay(r nominatimResponse) string {
	suburb := firstNonEmpty(r.Address.Suburb, r.Address.Neighbourhood)
	city := firstNonEmpty(r.Address.City, r.Address.Town, r.Address.Village, r.Address.County)
	country := r.Address.Country

	parts := make([]string, 0, 3)
	if suburb != "" {
		parts = append(parts, suburb)
	}
	if city != "" && city != suburb {
		parts = append(parts, city)
	}
	if country != "" {
		parts = append(parts, country)
	}

	if len(parts) > 0 {
		return strings.Join(parts, ", ")
	}
	return strings.TrimSpace(r.DisplayName)
}

// firstNonEmpty returns the first non-empty string in the argument list.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

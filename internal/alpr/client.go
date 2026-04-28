// Package alpr is the HTTP client for the ALPR engine sidecar that ships
// in docker/alpr/. The Go backend is a pure consumer of the engine: image
// bytes in, plate detections out. The engine itself runs as a separate
// container under the `alpr` Docker Compose profile and exposes the
// contract documented in .projd/progress/alpr-engine-service.json.
//
// Callers should construct a Client once per engine endpoint via
// NewClient and reuse it. The lazy accessor on cmd/server's deps struct
// (deps.ALPRClient) handles single-instance construction so toggling the
// runtime alpr_enabled flag does not require a process restart.
//
// Errors returned by Detect / Health are typed (ErrEngineUnreachable,
// ErrEngineTimeout, ErrEngineBadResponse) so callers can degrade
// gracefully rather than treating every failure as catastrophic.
package alpr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Sentinel errors so callers can distinguish recoverable from
// unrecoverable engine failures with errors.Is.
var (
	// ErrEngineUnreachable signals a network-layer failure such as DNS
	// resolution failure or connection refused. The engine container is
	// likely not running; the caller should skip ALPR work for this
	// item rather than retry tightly.
	ErrEngineUnreachable = errors.New("alpr: engine unreachable")

	// ErrEngineTimeout signals the request did not complete within the
	// client timeout (or the caller's context deadline). The engine may
	// still be alive but is overloaded or warming up.
	ErrEngineTimeout = errors.New("alpr: engine timeout")

	// ErrEngineBadResponse signals the engine returned a non-2xx status
	// or a body that did not parse as expected JSON. Indicates a
	// version mismatch between client and engine, or an internal engine
	// error; not retryable on the same input.
	ErrEngineBadResponse = errors.New("alpr: engine returned a bad response")
)

// Rect is the documented bbox shape (x, y, w, h) in image pixel space.
// Mirrors the JSON the engine emits.
type Rect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// VehicleAttributes is a forward-compatible placeholder for the second
// stage of ALPR (make / color / body type) owned by a later feature
// (alpr-vehicle-attributes-engine). All fields are optional so a Detection
// observed before that feature lands carries Vehicle == nil.
type VehicleAttributes struct {
	Make       string  `json:"make,omitempty"`
	Model      string  `json:"model,omitempty"`
	Color      string  `json:"color,omitempty"`
	BodyType   string  `json:"body_type,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
}

// Detection is one plate read returned by the engine for a single frame.
// Confidence is 0..1.
type Detection struct {
	PlateText  string             `json:"plate"`
	Confidence float64            `json:"confidence"`
	BBox       Rect               `json:"bbox"`
	Vehicle    *VehicleAttributes `json:"vehicle,omitempty"`
}

// HealthInfo is the parsed payload of GET /health.
type HealthInfo struct {
	OK                 bool   `json:"ok"`
	Model              string `json:"model"`
	Version            string `json:"version"`
	Region             string `json:"region"`
	SupportsAttributes bool   `json:"supports_attributes"`
	EngineLoaded       bool   `json:"engine_loaded"`
}

// Client is a thin HTTP client around the ALPR engine sidecar. It is safe
// to use from multiple goroutines concurrently; the underlying http.Client
// is goroutine-safe and the warning rate-limiter uses atomics.
type Client struct {
	baseURL string
	http    *http.Client

	// warnEvery throttles the "engine unreachable" log line so a long
	// outage does not flood the operator's log. Defaults to one warning
	// per 60s. Tested behaviour: at most one warning per window, with
	// the first error in any window always logged.
	warnEvery   time.Duration
	lastWarnNs  atomic.Int64
	warnCounter atomic.Uint64

	// constructorOnce protects against accidental NewClient mutation.
	// Currently a no-op guard but reserves space if we ever add lazy
	// per-client state.
	constructorOnce sync.Once
}

// NewClient returns a Client that talks to baseURL with the given
// per-request timeout. baseURL should be the engine origin (no trailing
// slash) -- e.g. "http://alpr:8081". Timeout 0 falls back to 10 seconds,
// which is comfortably above the engine's documented 5s server-side
// budget but still bounded so a stuck container does not stall callers
// indefinitely.
func NewClient(baseURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: timeout,
		},
		warnEvery: 60 * time.Second,
	}
	c.constructorOnce.Do(func() {})
	return c
}

// BaseURL returns the engine origin this client targets. Useful for
// diagnostic logging.
func (c *Client) BaseURL() string {
	return c.baseURL
}

// Health probes GET /health on the engine. Returns a typed error so the
// caller can branch on engine-up vs engine-down without parsing log
// strings.
func (c *Client) Health(ctx context.Context) (HealthInfo, error) {
	var info HealthInfo
	endpoint, err := c.url("/health")
	if err != nil {
		return info, fmt.Errorf("alpr: build health url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return info, fmt.Errorf("alpr: build health request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return info, c.classifyTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return info, fmt.Errorf("%w: GET /health: status %d", ErrEngineBadResponse, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return info, fmt.Errorf("%w: read /health body: %v", ErrEngineBadResponse, err)
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return info, fmt.Errorf("%w: parse /health body: %v", ErrEngineBadResponse, err)
	}
	return info, nil
}

// Detect uploads a single JPEG-encoded frame to POST /v1/detect and
// returns the plate detections the engine reports. frameJPEG must be the
// raw bytes of an encoded image (JPEG or any format Pillow can decode --
// see docker/alpr/app/main.py); we send it as multipart form-data on the
// `image` field per the documented contract.
//
// Returns a typed error so workers can fall back: ErrEngineUnreachable
// for network failures, ErrEngineTimeout for context deadlines or client
// timeouts, ErrEngineBadResponse for any other engine-side failure.
func (c *Client) Detect(ctx context.Context, frameJPEG []byte) ([]Detection, error) {
	if len(frameJPEG) == 0 {
		return nil, fmt.Errorf("alpr: detect called with empty frame")
	}

	body, contentType, err := buildMultipart(frameJPEG)
	if err != nil {
		return nil, fmt.Errorf("alpr: encode multipart: %w", err)
	}

	endpoint, err := c.url("/v1/detect")
	if err != nil {
		return nil, fmt.Errorf("alpr: build detect url: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("alpr: build detect request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.classifyTransportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 4 KiB of the error body so the wrapped error
		// preserves the engine's diagnostic string. Anything larger
		// gets truncated; a 500 dump is not useful in a log line.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%w: POST /v1/detect: status %d: %s",
			ErrEngineBadResponse, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	var out struct {
		Detections []Detection `json:"detections"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: parse detect body: %v", ErrEngineBadResponse, err)
	}
	return out.Detections, nil
}

// classifyTransportError maps a net/http transport error to one of the
// public sentinels. Any caller-cancelled or deadline-exceeded context is
// reported as ErrEngineTimeout; DNS failures and refused connections fall
// under ErrEngineUnreachable. All other transport errors map to
// ErrEngineBadResponse so the caller still sees a typed error rather
// than a raw transport string.
func (c *Client) classifyTransportError(err error) error {
	if err == nil {
		return nil
	}
	// Context deadline / cancellation -> timeout. Surface here BEFORE
	// inspecting net.OpError because the http client wraps context
	// errors inside a net/url *net.OpError on some Go versions.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrEngineTimeout, err)
	}

	// http.Client.Timeout firing wraps the context deadline in a
	// url.Error. Detect via the Timeout() interface that net/http
	// provides on the standard error types.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return fmt.Errorf("%w: %v", ErrEngineTimeout, err)
	}

	// Connection refused / no route to host / DNS failure -> unreachable.
	// We treat any net.OpError that is not a timeout as unreachable so a
	// stopped sidecar produces a single clear error class. Includes
	// EOF on dial which surfaces as a syscall error wrapped here.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		c.warnUnreachable(err)
		return fmt.Errorf("%w: %v", ErrEngineUnreachable, err)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return fmt.Errorf("%w: %v", ErrEngineTimeout, err)
		}
		c.warnUnreachable(err)
		return fmt.Errorf("%w: %v", ErrEngineUnreachable, err)
	}

	// Generic url.Error from http.Client without a timeout flag -- most
	// often "EOF" mid-response. Treat as bad response so the caller does
	// not infinitely retry against an actually-running but flaky engine.
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%w: %v", ErrEngineBadResponse, err)
	}

	return fmt.Errorf("%w: %v", ErrEngineBadResponse, err)
}

// warnUnreachable logs at most one "engine unreachable" warning per
// warnEvery window. Atomic CAS on the timestamp protects against the
// log racing across goroutines without forcing a mutex on the hot path.
// The accumulated counter is included in the log line so the operator
// can see how many requests were absorbed during a quiet window.
func (c *Client) warnUnreachable(err error) {
	c.warnCounter.Add(1)
	now := time.Now().UnixNano()
	last := c.lastWarnNs.Load()
	window := int64(c.warnEvery)
	if last != 0 && now-last < window {
		return
	}
	if !c.lastWarnNs.CompareAndSwap(last, now) {
		// Another goroutine just took the slot.
		return
	}
	suppressed := c.warnCounter.Swap(0)
	log.Printf("alpr: engine unreachable at %s (%d errors in last window): %v", c.baseURL, suppressed, err)
}

// url builds a fully-qualified URL for the given engine path.
func (c *Client) url(path string) (string, error) {
	if c.baseURL == "" {
		return "", fmt.Errorf("alpr: client baseURL is empty")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.baseURL + path, nil
}

// buildMultipart encodes a single JPEG frame as the multipart form body
// expected by the /v1/detect endpoint. Returns the body reader, the
// content-type header (which carries the boundary), and any encoding
// error.
func buildMultipart(frame []byte) (io.Reader, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// Use a stable filename + content-type so the engine's UploadFile
	// content-type sniff lands on the JPEG path even if the bytes are
	// actually PNG or webp -- the engine's image decoder handles the
	// real format from the bytes themselves.
	header := make(map[string][]string)
	header["Content-Disposition"] = []string{`form-data; name="image"; filename="frame.jpg"`}
	header["Content-Type"] = []string{"image/jpeg"}
	part, err := mw.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(frame); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return &buf, mw.FormDataContentType(), nil
}

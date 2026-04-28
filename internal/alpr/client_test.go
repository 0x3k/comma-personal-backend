package alpr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tests cover the four required failure modes per the feature
// acceptance criteria: success, timeout, unreachable, malformed JSON.
// Each maps to a typed error so callers can degrade gracefully.

func TestClient_Health_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"model":"yolo+mvit","version":"0.1.5","region":"us","supports_attributes":false,"engine_loaded":true}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !got.OK || got.Region != "us" || got.Version != "0.1.5" || !got.EngineLoaded {
		t.Fatalf("unexpected health payload: %+v", got)
	}
}

func TestClient_Health_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{not-json`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrEngineBadResponse) {
		t.Fatalf("expected ErrEngineBadResponse, got %v", err)
	}
}

func TestClient_Health_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	_, err := c.Health(context.Background())
	if !errors.Is(err, ErrEngineBadResponse) {
		t.Fatalf("expected ErrEngineBadResponse, got %v", err)
	}
}

func TestClient_Detect_Success(t *testing.T) {
	frame := []byte("\xff\xd8\xff\xe0not-a-real-jpeg-but-bytes-are-bytes")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/detect" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		// Verify the multipart body carries the expected `image` part.
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "multipart/form-data") {
			t.Fatalf("expected multipart/form-data, got %q", ct)
		}
		_, params, err := mimeParams(ct)
		if err != nil {
			t.Fatalf("parse content-type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		part, err := mr.NextPart()
		if err != nil {
			t.Fatalf("next part: %v", err)
		}
		if got := part.FormName(); got != "image" {
			t.Fatalf("expected form name image, got %q", got)
		}
		gotBody, _ := io.ReadAll(part)
		if !bytes.Equal(gotBody, frame) {
			t.Fatalf("frame bytes round-trip mismatch")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"detections":[{"plate":"ABC123","confidence":0.91,"bbox":{"x":10,"y":20,"w":80,"h":40},"vehicle":null}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 2*time.Second)
	dets, err := c.Detect(context.Background(), frame)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(dets) != 1 {
		t.Fatalf("expected 1 detection, got %d", len(dets))
	}
	d := dets[0]
	if d.PlateText != "ABC123" || d.Confidence < 0.9 || d.BBox.W != 80 {
		t.Fatalf("unexpected detection: %+v", d)
	}
	if d.Vehicle != nil {
		t.Fatalf("expected vehicle nil for explicit null, got %+v", d.Vehicle)
	}
}

// TestClient_Detect_VehicleFullAttributes covers the happy path where
// the engine emits every attribute including a stable signature_key.
// All fields must round-trip through JSON unchanged.
func TestClient_Detect_VehicleFullAttributes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"detections":[{
			"plate":"ABC123",
			"confidence":0.91,
			"bbox":{"x":0,"y":0,"w":10,"h":10},
			"vehicle":{
				"make":"toyota",
				"model":"camry",
				"year_min":2018,
				"year_max":2022,
				"color":"silver",
				"body_type":"sedan",
				"confidence":0.83,
				"signature_key":"toyota|camry|silver|sedan"
			}
		}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	dets, err := c.Detect(context.Background(), []byte("frame"))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if len(dets) != 1 {
		t.Fatalf("expected 1 detection, got %d", len(dets))
	}
	v := dets[0].Vehicle
	if v == nil {
		t.Fatalf("expected non-nil Vehicle")
	}
	if v.Make != "toyota" || v.Model != "camry" || v.Color != "silver" || v.BodyType != "sedan" {
		t.Fatalf("unexpected attributes: %+v", v)
	}
	if v.YearMin == nil || *v.YearMin != 2018 {
		t.Fatalf("expected YearMin=2018, got %v", v.YearMin)
	}
	if v.YearMax == nil || *v.YearMax != 2022 {
		t.Fatalf("expected YearMax=2022, got %v", v.YearMax)
	}
	if v.Confidence == nil || *v.Confidence < 0.82 || *v.Confidence > 0.84 {
		t.Fatalf("expected Confidence~=0.83, got %v", v.Confidence)
	}
	if v.SignatureKey != "toyota|camry|silver|sedan" {
		t.Fatalf("unexpected signature_key %q", v.SignatureKey)
	}
}

// TestClient_Detect_VehiclePartialAttributes covers the case where the
// engine confidently identified color + body_type but dropped make and
// model to JSON null. The signature_key reflects only the non-null
// attributes.
func TestClient_Detect_VehiclePartialAttributes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"detections":[{
			"plate":"ABC123",
			"confidence":0.91,
			"bbox":{"x":0,"y":0,"w":10,"h":10},
			"vehicle":{
				"make":null,
				"model":null,
				"year_min":null,
				"year_max":null,
				"color":"silver",
				"body_type":"sedan",
				"confidence":0.61,
				"signature_key":"silver|sedan"
			}
		}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	dets, err := c.Detect(context.Background(), []byte("frame"))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	v := dets[0].Vehicle
	if v == nil {
		t.Fatalf("expected non-nil Vehicle")
	}
	if v.Make != "" || v.Model != "" {
		t.Fatalf("expected make/model to be empty for JSON null, got make=%q model=%q", v.Make, v.Model)
	}
	if v.YearMin != nil || v.YearMax != nil {
		t.Fatalf("expected year_min/year_max nil, got %v / %v", v.YearMin, v.YearMax)
	}
	if v.Color != "silver" || v.BodyType != "sedan" {
		t.Fatalf("expected color=silver body_type=sedan, got %+v", v)
	}
	if v.SignatureKey != "silver|sedan" {
		t.Fatalf("unexpected signature_key %q", v.SignatureKey)
	}
}

// TestClient_Detect_VehicleAllNull covers the case where the engine
// ran the classifier but every attribute came back below confidence.
// Vehicle is non-nil (the engine attempted and reported), but every
// field is zero / empty and signature_key is empty.
func TestClient_Detect_VehicleAllNull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"detections":[{
			"plate":"ABC123",
			"confidence":0.91,
			"bbox":{"x":0,"y":0,"w":10,"h":10},
			"vehicle":{
				"make":null,
				"model":null,
				"year_min":null,
				"year_max":null,
				"color":null,
				"body_type":null,
				"confidence":null,
				"signature_key":""
			}
		}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	dets, err := c.Detect(context.Background(), []byte("frame"))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	v := dets[0].Vehicle
	if v == nil {
		t.Fatalf("expected non-nil Vehicle (engine ran the classifier)")
	}
	if v.Make != "" || v.Model != "" || v.Color != "" || v.BodyType != "" {
		t.Fatalf("expected all string attributes empty, got %+v", v)
	}
	if v.YearMin != nil || v.YearMax != nil || v.Confidence != nil {
		t.Fatalf("expected nil pointer attributes, got %+v", v)
	}
	if v.SignatureKey != "" {
		t.Fatalf("expected empty signature_key for all-null, got %q", v.SignatureKey)
	}
}

// TestClient_Detect_VehicleKeyMissing covers the case where the engine
// is built without the attributes plugin (or ALPR_ATTRIBUTES_ENABLED=false)
// and omits the vehicle key entirely. Vehicle must be nil.
func TestClient_Detect_VehicleKeyMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Note: no "vehicle" key at all in the detection object.
		_, _ = io.WriteString(w, `{"detections":[{
			"plate":"ABC123",
			"confidence":0.91,
			"bbox":{"x":0,"y":0,"w":10,"h":10}
		}]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	dets, err := c.Detect(context.Background(), []byte("frame"))
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if dets[0].Vehicle != nil {
		t.Fatalf("expected nil Vehicle when key absent, got %+v", dets[0].Vehicle)
	}
}

// TestClient_Health_SupportsAttributesTrue confirms the supports_attributes
// flag from /health round-trips through HealthInfo. alpr-detection-worker
// uses this to decide whether to wait for vehicle attributes or call the
// engine in plate-only mode.
func TestClient_Health_SupportsAttributesTrue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"model":"yolo+mvit","version":"0.1.5","region":"us","supports_attributes":true,"engine_loaded":true}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !got.SupportsAttributes {
		t.Fatalf("expected SupportsAttributes=true")
	}
}

func TestClient_Detect_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep longer than the client's per-request timeout so the
		// http.Client cancels the request and returns a timeout error.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, 50*time.Millisecond)
	_, err := c.Detect(context.Background(), []byte("frame"))
	if !errors.Is(err, ErrEngineTimeout) {
		t.Fatalf("expected ErrEngineTimeout, got %v", err)
	}
}

func TestClient_Detect_ContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := c.Detect(ctx, []byte("frame"))
	if !errors.Is(err, ErrEngineTimeout) {
		t.Fatalf("expected ErrEngineTimeout, got %v", err)
	}
}

func TestClient_Detect_Unreachable(t *testing.T) {
	// Bind a listener, immediately close it. Connecting to the now-dead
	// address yields a connection refused error which the client must
	// classify as ErrEngineUnreachable.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	c := NewClient("http://"+addr, 500*time.Millisecond)
	_, err = c.Detect(context.Background(), []byte("frame"))
	if !errors.Is(err, ErrEngineUnreachable) {
		t.Fatalf("expected ErrEngineUnreachable, got %v", err)
	}
}

func TestClient_Detect_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"detections": [`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	_, err := c.Detect(context.Background(), []byte("frame"))
	if !errors.Is(err, ErrEngineBadResponse) {
		t.Fatalf("expected ErrEngineBadResponse, got %v", err)
	}
}

func TestClient_Detect_4xxBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "image too small", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	_, err := c.Detect(context.Background(), []byte("frame"))
	if !errors.Is(err, ErrEngineBadResponse) {
		t.Fatalf("expected ErrEngineBadResponse, got %v", err)
	}
	if !strings.Contains(err.Error(), "image too small") {
		t.Fatalf("expected error to include engine message, got %v", err)
	}
}

func TestClient_Detect_EmptyFrame(t *testing.T) {
	c := NewClient("http://127.0.0.1:0", time.Second)
	_, err := c.Detect(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error for empty frame")
	}
	if errors.Is(err, ErrEngineUnreachable) || errors.Is(err, ErrEngineBadResponse) {
		t.Fatalf("empty-frame error should not be a typed transport error: %v", err)
	}
}

func TestNewClient_DefaultsTimeout(t *testing.T) {
	c := NewClient("http://example.test", 0)
	if c.http.Timeout != 10*time.Second {
		t.Fatalf("expected default 10s timeout, got %v", c.http.Timeout)
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	c := NewClient("http://example.test/", time.Second)
	if c.BaseURL() != "http://example.test" {
		t.Fatalf("expected trailing slash trimmed, got %q", c.BaseURL())
	}
}

// mimeParams is a thin alias over mime.ParseMediaType so the success
// path in TestClient_Detect_Success reads cleanly.
func mimeParams(ct string) (string, map[string]string, error) {
	return mime.ParseMediaType(ct)
}

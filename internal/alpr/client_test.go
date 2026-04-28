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
		_, _ = io.WriteString(w, `{"ok":true,"model":"yolo+mvit","version":"0.1.5","region":"us","engine_loaded":true}`)
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
		_, _ = io.WriteString(w, `{"detections":[{"plate":"ABC123","confidence":0.91,"bbox":{"x":10,"y":20,"w":80,"h":40}}]}`)
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

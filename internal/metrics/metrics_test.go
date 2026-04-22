package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	dto "github.com/prometheus/client_model/go"
)

// TestMetricsNamesAndLabelsExist drives every metric once via its helper and
// then scrapes the registry to verify the metric name + label set actually
// appears in the exposition output. This guards both the name strings and
// the label keys promised by the feature specification.
func TestMetricsNamesAndLabelsExist(t *testing.T) {
	m := New()

	// Drive every collector at least once with the labels from the spec.
	m.ObserveHTTPRequest("GET", "/v1/route/:id", "200", 12*time.Millisecond)
	m.AddUploadBytes("dongle-abc", 4096)
	m.ObserveTranscode("fcamera", "success", 3*time.Second)
	m.ObserveRPCCall("getNetworkType", "success", 150*time.Millisecond)
	m.SetConnectedDevices(2)
	m.ObserveWorkerRun("transcoder", 800*time.Millisecond)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, f := range families {
		byName[f.GetName()] = f
	}

	// Each entry asserts: the metric is registered *and* it has exposed a
	// sample with the expected label keys (and values where we can match).
	cases := []struct {
		name   string
		labels map[string]string // key -> expected value
	}{
		{"http_requests_total", map[string]string{"method": "GET", "path": "/v1/route/:id", "status": "200"}},
		{"http_request_duration_seconds", map[string]string{"method": "GET", "path": "/v1/route/:id"}},
		{"upload_bytes_total", map[string]string{"dongle_id": "dongle-abc"}},
		{"transcode_duration_seconds", map[string]string{"camera": "fcamera", "result": "success"}},
		{"rpc_call_duration_seconds", map[string]string{"method": "getNetworkType"}},
		{"rpc_calls_total", map[string]string{"method": "getNetworkType", "status": "success"}},
		{"ws_connected_devices", nil},
		{"worker_run_duration_seconds", map[string]string{"worker": "transcoder"}},
	}

	for _, tc := range cases {
		fam, ok := byName[tc.name]
		if !ok {
			t.Errorf("metric family %q not registered", tc.name)
			continue
		}
		if len(fam.Metric) == 0 {
			t.Errorf("metric family %q has no samples", tc.name)
			continue
		}

		if tc.labels == nil {
			continue
		}

		if !familyHasLabels(fam, tc.labels) {
			t.Errorf("metric %q missing expected labels %v; got %s",
				tc.name, tc.labels, describeLabels(fam))
		}
	}
}

// TestHandlerExposesMetrics verifies that the /metrics HTTP handler returns
// the Prometheus exposition format with the expected metric names.
func TestHandlerExposesMetrics(t *testing.T) {
	m := New()
	m.ObserveHTTPRequest("GET", "/health", "200", 1*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("metrics handler status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(body), "http_requests_total") {
		t.Errorf("exposition body missing http_requests_total; body=%q", body)
	}
}

// TestNilMetricsIsNoOp ensures every helper can be safely called on a nil
// Metrics. This keeps existing callers that do not inject metrics working.
func TestNilMetricsIsNoOp(t *testing.T) {
	var m *Metrics
	// None of these should panic.
	m.ObserveHTTPRequest("GET", "/x", "200", time.Millisecond)
	m.AddUploadBytes("d", 1)
	m.ObserveTranscode("fcamera", "success", time.Millisecond)
	m.ObserveRPCCall("m", "success", time.Millisecond)
	m.SetConnectedDevices(1)
	m.IncConnectedDevices()
	m.DecConnectedDevices()
	m.ObserveWorkerRun("w", time.Millisecond)

	// And the handler should still serve a response (a 503) instead of
	// panicking, so the route can be mounted unconditionally.
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil Metrics handler status = %d, want 503", rec.Code)
	}
}

// TestEchoMiddlewareObserves exercises the HTTP middleware end-to-end using
// Echo's test recorder. It confirms the middleware records both a request
// count and a latency observation for the matched route template.
func TestEchoMiddlewareObserves(t *testing.T) {
	m := New()
	e := echo.New()
	e.Use(EchoMiddleware(m))
	e.GET("/hello/:name", func(c echo.Context) error {
		return c.String(http.StatusOK, "hi "+c.Param("name"))
	})

	req := httptest.NewRequest(http.MethodGet, "/hello/world", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200", rec.Code)
	}

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather failed: %v", err)
	}

	var foundCount, foundDuration bool
	for _, f := range families {
		switch f.GetName() {
		case "http_requests_total":
			foundCount = familyHasLabels(f, map[string]string{
				"method": "GET",
				"path":   "/hello/:name",
				"status": "200",
			})
		case "http_request_duration_seconds":
			foundDuration = familyHasLabels(f, map[string]string{
				"method": "GET",
				"path":   "/hello/:name",
			})
		}
	}
	if !foundCount {
		t.Error("http_requests_total missing for /hello/:name")
	}
	if !foundDuration {
		t.Error("http_request_duration_seconds missing for /hello/:name")
	}
}

// familyHasLabels returns true if at least one sample in fam has labels that
// match every key=value in want.
func familyHasLabels(fam *dto.MetricFamily, want map[string]string) bool {
	for _, metric := range fam.Metric {
		labels := make(map[string]string, len(metric.Label))
		for _, lp := range metric.Label {
			labels[lp.GetName()] = lp.GetValue()
		}
		ok := true
		for k, v := range want {
			if labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func describeLabels(fam *dto.MetricFamily) string {
	var sb strings.Builder
	for i, metric := range fam.Metric {
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString("{")
		for j, lp := range metric.Label {
			if j > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(lp.GetName())
			sb.WriteString("=")
			sb.WriteString(lp.GetValue())
		}
		sb.WriteString("}")
	}
	return sb.String()
}

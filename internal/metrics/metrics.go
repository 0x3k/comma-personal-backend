// Package metrics provides a single Metrics struct that owns all Prometheus
// collectors for the server. Components (HTTP middleware, the transcoder, the
// WebSocket RPC caller, background workers) take a *Metrics in their
// constructors and call the Observe / Inc helpers defined here.
//
// A nil *Metrics is treated as a no-op: every helper method is safe to call
// on a nil receiver. That keeps existing call sites and tests working without
// having to thread a metrics instance everywhere, while still satisfying the
// acceptance criterion that constructors accept a *Metrics.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// defaultDurationBuckets is used by every *_duration_seconds histogram so that
// HTTP, RPC, transcode, and worker latencies can be compared on the same
// buckets. The range (1ms .. ~16s) covers both fast endpoints and slow
// transcodes without exploding cardinality.
var defaultDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10, 30, 60, 120, 300,
}

// Metrics owns every Prometheus collector exposed by the server.
type Metrics struct {
	registry *prometheus.Registry

	httpRequestsTotal   *prometheus.CounterVec
	httpRequestDuration *prometheus.HistogramVec

	uploadBytesTotal *prometheus.CounterVec

	transcodeDuration *prometheus.HistogramVec

	rpcCallDuration *prometheus.HistogramVec
	rpcCallsTotal   *prometheus.CounterVec

	wsConnectedDevices prometheus.Gauge

	workerRunDuration *prometheus.HistogramVec

	thumbnailGenerationsTotal  *prometheus.CounterVec
	thumbnailGenerationSeconds prometheus.Histogram
	thumbnailQueueDepth        prometheus.Gauge

	turnDetectorRunsTotal    *prometheus.CounterVec
	turnDetectorTurnsEmitted prometheus.Counter
	turnDetectorRunSeconds   prometheus.Histogram

	alprFramesExtractedTotal *prometheus.CounterVec
	alprExtractorSegmentSecs prometheus.Histogram
	alprExtractorQueueDepth  prometheus.Gauge

	alprFramesProcessedTotal *prometheus.CounterVec
	alprDetectionsTotal      prometheus.Counter
	alprEngineLatencySeconds prometheus.Histogram
	alprEngineErrorsTotal    *prometheus.CounterVec
	alprDetectorQueueDepth   prometheus.Gauge
}

// New creates a Metrics backed by a fresh registry. Use NewWithRegistry to
// share a registry with other packages (for example, to add Go runtime
// collectors).
func New() *Metrics {
	return NewWithRegistry(prometheus.NewRegistry())
}

// NewWithRegistry creates a Metrics that registers its collectors on the
// provided registry. Callers typically use prometheus.NewRegistry so metrics
// are isolated per-test.
func NewWithRegistry(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		registry: reg,

		httpRequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "http_requests_total",
				Help: "Total number of HTTP requests handled, labeled by method, route, and status code.",
			},
			[]string{"method", "path", "status"},
		),
		httpRequestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "http_request_duration_seconds",
				Help:    "Latency distribution of HTTP requests, labeled by method and route.",
				Buckets: defaultDurationBuckets,
			},
			[]string{"method", "path"},
		),

		uploadBytesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "upload_bytes_total",
				Help: "Total bytes received by the upload endpoint, labeled by device dongle_id.",
			},
			[]string{"dongle_id"},
		),

		transcodeDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "transcode_duration_seconds",
				Help:    "Wall-clock duration of video transcodes, labeled by camera and result (success|error).",
				Buckets: defaultDurationBuckets,
			},
			[]string{"camera", "result"},
		),

		rpcCallDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "rpc_call_duration_seconds",
				Help:    "Latency of WebSocket JSON-RPC calls to devices, labeled by method.",
				Buckets: defaultDurationBuckets,
			},
			[]string{"method"},
		),
		rpcCallsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "rpc_calls_total",
				Help: "Total number of WebSocket JSON-RPC calls issued, labeled by method and status (success|error|timeout).",
			},
			[]string{"method", "status"},
		),

		wsConnectedDevices: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "ws_connected_devices",
				Help: "Number of devices currently connected via WebSocket.",
			},
		),

		workerRunDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "worker_run_duration_seconds",
				Help:    "Duration of a single background-worker run, labeled by worker name.",
				Buckets: defaultDurationBuckets,
			},
			[]string{"worker"},
		),

		thumbnailGenerationsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "thumbnail_generations_total",
				Help: "Total number of route thumbnail generation attempts, labeled by result (success|failure).",
			},
			[]string{"result"},
		),
		thumbnailGenerationSeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "thumbnail_generation_duration_seconds",
				Help:    "Wall-clock duration of a single route thumbnail generation run.",
				Buckets: defaultDurationBuckets,
			},
		),
		thumbnailQueueDepth: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "thumbnail_queue_depth",
				Help: "Current number of routes queued for thumbnail generation.",
			},
		),

		turnDetectorRunsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "turn_detector_runs_total",
				Help: "Total number of turn-detector route processing runs, labeled by result (emitted|empty|skipped|error).",
			},
			[]string{"result"},
		),
		turnDetectorTurnsEmitted: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "turn_detector_turns_emitted_total",
				Help: "Total number of turns persisted by the turn-detector worker across all routes.",
			},
		),
		turnDetectorRunSeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "turn_detector_run_seconds",
				Help:    "Wall-clock duration of a single turn-detector route processing run.",
				Buckets: defaultDurationBuckets,
			},
		),

		alprFramesExtractedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "alpr_frames_extracted_total",
				Help: "Total number of frames the ALPR extractor pushed onto its output channel, labeled by result (ok|error).",
			},
			[]string{"result"},
		),
		alprExtractorSegmentSecs: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "alpr_extractor_segment_seconds",
				Help:    "Wall-clock duration of the ALPR extractor processing a single fcamera segment end-to-end.",
				Buckets: defaultDurationBuckets,
			},
		),
		alprExtractorQueueDepth: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "alpr_extractor_queue_depth",
				Help: "Current depth of the ALPR extractor->detector frame channel.",
			},
		),

		alprFramesProcessedTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "alpr_frames_processed_total",
				Help: "Total number of frames the ALPR detection worker processed, labeled by result (detected|empty|dropped_no_gps|dropped_disabled|engine_error).",
			},
			[]string{"result"},
		),
		alprDetectionsTotal: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "alpr_detections_total",
				Help: "Total number of plate_detections rows persisted by the ALPR detection worker.",
			},
		),
		alprEngineLatencySeconds: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name:    "alpr_engine_latency_seconds",
				Help:    "Wall-clock duration of a single ALPR engine /v1/detect call.",
				Buckets: defaultDurationBuckets,
			},
		),
		alprEngineErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "alpr_engine_errors_total",
				Help: "Total number of ALPR engine call failures, labeled by type (unreachable|timeout|bad_response).",
			},
			[]string{"type"},
		),
		alprDetectorQueueDepth: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "alpr_detector_queue_depth",
				Help: "Instantaneous depth of the extractor->detector frame channel as observed by the detection worker.",
			},
		),
	}

	reg.MustRegister(
		m.httpRequestsTotal,
		m.httpRequestDuration,
		m.uploadBytesTotal,
		m.transcodeDuration,
		m.rpcCallDuration,
		m.rpcCallsTotal,
		m.wsConnectedDevices,
		m.workerRunDuration,
		m.thumbnailGenerationsTotal,
		m.thumbnailGenerationSeconds,
		m.thumbnailQueueDepth,
		m.turnDetectorRunsTotal,
		m.turnDetectorTurnsEmitted,
		m.turnDetectorRunSeconds,
		m.alprFramesExtractedTotal,
		m.alprExtractorSegmentSecs,
		m.alprExtractorQueueDepth,
		m.alprFramesProcessedTotal,
		m.alprDetectionsTotal,
		m.alprEngineLatencySeconds,
		m.alprEngineErrorsTotal,
		m.alprDetectorQueueDepth,
	)

	return m
}

// Registry returns the registry that backs this Metrics. It is useful when
// building the /metrics handler or when a test wants to gather metrics.
func (m *Metrics) Registry() *prometheus.Registry {
	if m == nil {
		return nil
	}
	return m.registry
}

// Handler returns an http.Handler that serves the Prometheus exposition
// format for this registry. A nil Metrics returns a 503 handler so callers
// can still mount the endpoint unconditionally.
func (m *Metrics) Handler() http.Handler {
	if m == nil || m.registry == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "metrics unavailable", http.StatusServiceUnavailable)
		})
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}

// ObserveHTTPRequest records a single HTTP request's outcome and latency.
// It is called by the Echo middleware.
func (m *Metrics) ObserveHTTPRequest(method, path, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.httpRequestsTotal.WithLabelValues(method, path, status).Inc()
	m.httpRequestDuration.WithLabelValues(method, path).Observe(d.Seconds())
}

// AddUploadBytes records bytes received for a given device.
func (m *Metrics) AddUploadBytes(dongleID string, n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.uploadBytesTotal.WithLabelValues(dongleID).Add(float64(n))
}

// ObserveTranscode records the duration and outcome of a single camera
// transcode. result should be "success" or "error".
func (m *Metrics) ObserveTranscode(camera, result string, d time.Duration) {
	if m == nil {
		return
	}
	m.transcodeDuration.WithLabelValues(camera, result).Observe(d.Seconds())
}

// ObserveRPCCall records the latency and outcome of a WebSocket RPC call.
// status should be "success", "error", or "timeout".
func (m *Metrics) ObserveRPCCall(method, status string, d time.Duration) {
	if m == nil {
		return
	}
	m.rpcCallDuration.WithLabelValues(method).Observe(d.Seconds())
	m.rpcCallsTotal.WithLabelValues(method, status).Inc()
}

// SetConnectedDevices sets the current count of active WebSocket clients.
func (m *Metrics) SetConnectedDevices(n int) {
	if m == nil {
		return
	}
	m.wsConnectedDevices.Set(float64(n))
}

// IncConnectedDevices increments the connected device gauge by one.
func (m *Metrics) IncConnectedDevices() {
	if m == nil {
		return
	}
	m.wsConnectedDevices.Inc()
}

// DecConnectedDevices decrements the connected device gauge by one.
func (m *Metrics) DecConnectedDevices() {
	if m == nil {
		return
	}
	m.wsConnectedDevices.Dec()
}

// ObserveWorkerRun records the duration of a single background-worker run.
func (m *Metrics) ObserveWorkerRun(worker string, d time.Duration) {
	if m == nil {
		return
	}
	m.workerRunDuration.WithLabelValues(worker).Observe(d.Seconds())
}

// ObserveThumbnailGeneration records the outcome and duration of a single
// thumbnail generation attempt. result should be "success" or "failure".
func (m *Metrics) ObserveThumbnailGeneration(result string, d time.Duration) {
	if m == nil {
		return
	}
	m.thumbnailGenerationsTotal.WithLabelValues(result).Inc()
	m.thumbnailGenerationSeconds.Observe(d.Seconds())
}

// SetThumbnailQueueDepth publishes the current depth of the thumbnail
// generation queue.
func (m *Metrics) SetThumbnailQueueDepth(n int) {
	if m == nil {
		return
	}
	m.thumbnailQueueDepth.Set(float64(n))
}

// IncTurnDetectorRun increments the per-run counter labeled by result.
// Valid values: "emitted", "empty", "skipped", "error". Anything else
// is still recorded -- Prometheus will simply expose a new label value.
func (m *Metrics) IncTurnDetectorRun(result string) {
	if m == nil {
		return
	}
	m.turnDetectorRunsTotal.WithLabelValues(result).Inc()
}

// AddTurnDetectorTurnsEmitted increments the cumulative count of turns
// the worker has persisted. n may be zero (a no-op call) so callers
// don't need to special-case empty results.
func (m *Metrics) AddTurnDetectorTurnsEmitted(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.turnDetectorTurnsEmitted.Add(float64(n))
}

// ObserveTurnDetectorRun records the wall-clock duration of a single
// route's turn-detection run.
func (m *Metrics) ObserveTurnDetectorRun(d time.Duration) {
	if m == nil {
		return
	}
	m.turnDetectorRunSeconds.Observe(d.Seconds())
}

// IncALPRFrameExtracted increments the per-frame counter labeled by
// outcome. result should be "ok" or "error" but any string is accepted.
func (m *Metrics) IncALPRFrameExtracted(result string) {
	if m == nil {
		return
	}
	m.alprFramesExtractedTotal.WithLabelValues(result).Inc()
}

// AddALPRFramesExtracted bumps the per-frame counter by n. Useful for
// emitting a single counter add per segment rather than once per frame
// when the worker has tallied a batch.
func (m *Metrics) AddALPRFramesExtracted(result string, n int) {
	if m == nil || n <= 0 {
		return
	}
	m.alprFramesExtractedTotal.WithLabelValues(result).Add(float64(n))
}

// ObserveALPRExtractorSegment records the wall-clock duration of a
// single fcamera segment passing through the ALPR extractor.
func (m *Metrics) ObserveALPRExtractorSegment(d time.Duration) {
	if m == nil {
		return
	}
	m.alprExtractorSegmentSecs.Observe(d.Seconds())
}

// SetALPRExtractorQueueDepth publishes the current depth of the
// extractor->detector frame channel.
func (m *Metrics) SetALPRExtractorQueueDepth(n int) {
	if m == nil {
		return
	}
	m.alprExtractorQueueDepth.Set(float64(n))
}

// IncALPRFrameProcessed increments the per-frame outcome counter for
// the detection worker. result is one of "detected", "empty",
// "dropped_no_gps", "dropped_disabled", or "engine_error" -- any other
// string still records, surfacing as a new label value in Prometheus.
func (m *Metrics) IncALPRFrameProcessed(result string) {
	if m == nil {
		return
	}
	m.alprFramesProcessedTotal.WithLabelValues(result).Inc()
}

// IncALPRDetection bumps the cumulative count of plate_detections
// rows persisted by the detection worker. Called once per row
// successfully committed.
func (m *Metrics) IncALPRDetection() {
	if m == nil {
		return
	}
	m.alprDetectionsTotal.Inc()
}

// ObserveALPREngineLatency records the wall-clock duration of one
// engine.Detect call. Called from both the success and the error
// paths so a stuck engine still produces observable latency tails.
func (m *Metrics) ObserveALPREngineLatency(d time.Duration) {
	if m == nil {
		return
	}
	m.alprEngineLatencySeconds.Observe(d.Seconds())
}

// IncALPREngineError increments the typed engine-error counter.
// kind is one of "unreachable", "timeout", or "bad_response" --
// matches the alpr.ErrEngine* sentinel set.
func (m *Metrics) IncALPREngineError(kind string) {
	if m == nil {
		return
	}
	m.alprEngineErrorsTotal.WithLabelValues(kind).Inc()
}

// SetALPRDetectorQueueDepth publishes the instantaneous depth of the
// extractor->detector frame channel as observed by the detection
// worker. Sampled at a low rate so the metric is observable but
// cheap.
func (m *Metrics) SetALPRDetectorQueueDepth(n int) {
	if m == nil {
		return
	}
	m.alprDetectorQueueDepth.Set(float64(n))
}

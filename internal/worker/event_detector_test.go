package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/klauspost/compress/zstd"

	"comma-personal-backend/internal/cereal"
	"comma-personal-backend/internal/cereal/schema"
	"comma-personal-backend/internal/storage"
)

// buildSignals is a small helper for building a column-oriented
// DrivingSignals from a row-oriented list of fixtures. It saves each test
// from writing seven parallel slice literals.
type row struct {
	dt     time.Duration // offset from origin
	vEgo   float64
	brake  bool
	gas    bool
	engSet bool // was the Engaged column carried on this row?
	eng    bool
	alert  string
}

func buildSignals(rows []row) *cereal.DrivingSignals {
	origin := time.Unix(1700000000, 0).UTC()
	sig := &cereal.DrivingSignals{}
	for _, r := range rows {
		sig.Times = append(sig.Times, origin.Add(r.dt))
		sig.VEgo = append(sig.VEgo, r.vEgo)
		sig.SteeringAngleDeg = append(sig.SteeringAngleDeg, 0)
		sig.BrakePressed = append(sig.BrakePressed, r.brake)
		sig.GasPressed = append(sig.GasPressed, r.gas)
		sig.Engaged = append(sig.Engaged, r.eng)
		sig.AlertText = append(sig.AlertText, r.alert)
		_ = r.engSet
	}
	return sig
}

func eventTypes(evs []DetectedEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	sort.Strings(out)
	return out
}

// TestDetectHardBrake verifies the rule fires once on a dense deceleration
// that exceeds both the threshold and the minimum duration.
func TestDetectHardBrake(t *testing.T) {
	// 6 samples at 0.1s intervals, constant -10 m/s^2 deceleration.
	// Duration from first to last: 0.5s, exceeds the 0.3s min.
	rows := []row{
		{dt: 0, vEgo: 10},
		{dt: 100 * time.Millisecond, vEgo: 9},
		{dt: 200 * time.Millisecond, vEgo: 8},
		{dt: 300 * time.Millisecond, vEgo: 7},
		{dt: 400 * time.Millisecond, vEgo: 6},
		{dt: 500 * time.Millisecond, vEgo: 5},
	}
	events := DetectEvents(buildSignals(rows), Thresholds{})
	var hb []DetectedEvent
	for _, e := range events {
		if e.Type == EventTypeHardBrake {
			hb = append(hb, e)
		}
	}
	if len(hb) != 1 {
		t.Fatalf("expected 1 hard_brake event, got %d (%v)", len(hb), eventTypes(events))
	}
	if hb[0].Severity != EventSeverityWarn {
		t.Errorf("hard_brake severity = %q, want %q", hb[0].Severity, EventSeverityWarn)
	}
	if hb[0].RouteOffsetSeconds != 0 {
		t.Errorf("hard_brake offset = %v, want 0 (chain starts at first sample)", hb[0].RouteOffsetSeconds)
	}
	if _, ok := hb[0].Payload["peak_decel_mps2"]; !ok {
		t.Errorf("expected peak_decel_mps2 in payload, got %+v", hb[0].Payload)
	}
}

// TestDetectHardBrake_BelowThreshold verifies that mild deceleration
// (below mps2 threshold) does not fire the rule.
func TestDetectHardBrake_BelowThreshold(t *testing.T) {
	// 2 m/s^2 decel -- well under the 4.5 default.
	rows := []row{
		{dt: 0, vEgo: 10},
		{dt: 100 * time.Millisecond, vEgo: 9.8},
		{dt: 200 * time.Millisecond, vEgo: 9.6},
		{dt: 300 * time.Millisecond, vEgo: 9.4},
		{dt: 400 * time.Millisecond, vEgo: 9.2},
	}
	events := DetectEvents(buildSignals(rows), Thresholds{})
	for _, e := range events {
		if e.Type == EventTypeHardBrake {
			t.Errorf("unexpected hard_brake event at mild decel: %+v", e)
		}
	}
}

// TestDetectHardBrake_TooShort verifies the min-duration guard: a single
// large-decel step that only lasts one interval should not fire.
func TestDetectHardBrake_TooShort(t *testing.T) {
	// One 0.05s step of -20 m/s^2 then recovery. 0.05s < 0.3s min.
	rows := []row{
		{dt: 0, vEgo: 10},
		{dt: 50 * time.Millisecond, vEgo: 9},
		{dt: 100 * time.Millisecond, vEgo: 9},
		{dt: 150 * time.Millisecond, vEgo: 9},
	}
	events := DetectEvents(buildSignals(rows), Thresholds{})
	for _, e := range events {
		if e.Type == EventTypeHardBrake {
			t.Errorf("unexpected hard_brake event at short-decel: %+v", e)
		}
	}
}

// TestDetectDisengage verifies the trailing edge of Engaged fires exactly
// once at the disengagement moment. The falling edge must carry an alert
// text so the detector believes it was a real engagement carrier.
func TestDetectDisengage(t *testing.T) {
	rows := []row{
		{dt: 0, vEgo: 10, eng: false, alert: ""},
		{dt: 100 * time.Millisecond, vEgo: 10, eng: true, alert: ""},
		{dt: 200 * time.Millisecond, vEgo: 10, eng: true, alert: ""},
		{dt: 300 * time.Millisecond, vEgo: 10, eng: false, alert: "Disengaged"},
	}
	events := DetectEvents(buildSignals(rows), Thresholds{})
	var d []DetectedEvent
	for _, e := range events {
		if e.Type == EventTypeDisengage {
			d = append(d, e)
		}
	}
	if len(d) != 1 {
		t.Fatalf("expected 1 disengage event, got %d (%v)", len(d), eventTypes(events))
	}
	if d[0].Severity != EventSeverityInfo {
		t.Errorf("disengage severity = %q, want %q", d[0].Severity, EventSeverityInfo)
	}
	if d[0].RouteOffsetSeconds != 0.3 {
		t.Errorf("disengage offset = %v, want 0.3", d[0].RouteOffsetSeconds)
	}
}

// TestDetectDisengage_EndOfLog verifies that a disengage at the last row
// still fires even when no explicit alert text is carried. This is the
// "isLastEngagementCarrier" fallback.
func TestDetectDisengage_EndOfLog(t *testing.T) {
	rows := []row{
		{dt: 0, vEgo: 10, eng: false},
		{dt: 100 * time.Millisecond, vEgo: 10, eng: true, alert: "Engaged"},
		{dt: 200 * time.Millisecond, vEgo: 10, eng: false},
	}
	events := DetectEvents(buildSignals(rows), Thresholds{})
	var d []DetectedEvent
	for _, e := range events {
		if e.Type == EventTypeDisengage {
			d = append(d, e)
		}
	}
	if len(d) != 1 {
		t.Fatalf("expected 1 end-of-log disengage, got %d", len(d))
	}
}

// TestDetectFCW verifies both spellings of the FCW alert fire the rule.
// alert_warning also fires because FCW is a subset of any-alert.
func TestDetectFCW(t *testing.T) {
	cases := []string{
		"FCW: Active",
		"Forward Collision Warning",
	}
	for _, alert := range cases {
		t.Run(alert, func(t *testing.T) {
			rows := []row{
				{dt: 0, vEgo: 10},
				{dt: 100 * time.Millisecond, vEgo: 10, alert: alert},
				{dt: 200 * time.Millisecond, vEgo: 10, alert: alert},
				{dt: 300 * time.Millisecond, vEgo: 10},
			}
			events := DetectEvents(buildSignals(rows), Thresholds{})
			var fcw, warn int
			for _, e := range events {
				switch e.Type {
				case EventTypeFCW:
					fcw++
					if e.Payload["alert_text"] != alert {
						t.Errorf("fcw payload alert_text = %v, want %q", e.Payload["alert_text"], alert)
					}
				case EventTypeAlertWarning:
					warn++
				}
			}
			if fcw != 1 {
				t.Errorf("expected 1 fcw event, got %d", fcw)
			}
			if warn != 1 {
				t.Errorf("expected 1 alert_warning event, got %d", warn)
			}
		})
	}
}

// TestDetectAlertWarning_NonFCWOnly verifies a generic alert fires only
// alert_warning (and not fcw) when the text doesn't match the FCW prefixes.
func TestDetectAlertWarning_NonFCWOnly(t *testing.T) {
	rows := []row{
		{dt: 0, vEgo: 10},
		{dt: 100 * time.Millisecond, vEgo: 10, alert: "Take Control"},
		{dt: 200 * time.Millisecond, vEgo: 10, alert: "Take Control"},
	}
	events := DetectEvents(buildSignals(rows), Thresholds{})
	var warn, fcw int
	for _, e := range events {
		if e.Type == EventTypeAlertWarning {
			warn++
		}
		if e.Type == EventTypeFCW {
			fcw++
		}
	}
	if warn != 1 {
		t.Errorf("expected 1 alert_warning, got %d", warn)
	}
	if fcw != 0 {
		t.Errorf("expected no fcw, got %d", fcw)
	}
}

// TestDetectEvents_Deterministic verifies that running detection twice over
// the same input yields identical event slices. This underwrites the DB
// idempotency guarantee: if the pure detector is deterministic and the
// InsertEvent query uses ON CONFLICT DO NOTHING on
// (route_id, type, route_offset_seconds), a re-run of the worker is a no-op.
func TestDetectEvents_Deterministic(t *testing.T) {
	rows := []row{
		{dt: 0, vEgo: 10},
		{dt: 100 * time.Millisecond, vEgo: 9, eng: true, alert: "Engaged"},
		{dt: 200 * time.Millisecond, vEgo: 8},
		{dt: 300 * time.Millisecond, vEgo: 7, alert: "FCW: Lead Braking"},
		{dt: 400 * time.Millisecond, vEgo: 6},
		{dt: 500 * time.Millisecond, vEgo: 5, eng: false, alert: "Disengaged"},
	}
	sig := buildSignals(rows)
	first := DetectEvents(sig, Thresholds{})
	second := DetectEvents(sig, Thresholds{})
	if !reflect.DeepEqual(first, second) {
		t.Errorf("DetectEvents not deterministic:\n  first=%+v\n  second=%+v", first, second)
	}
	// Idempotent re-run must still produce the same (type, offset) set for
	// every event -- these three form the UNIQUE key enforced by the DB.
	keys := make(map[[2]any]int)
	for _, e := range first {
		k := [2]any{e.Type, e.RouteOffsetSeconds}
		keys[k]++
	}
	for _, n := range keys {
		if n != 1 {
			t.Errorf("duplicate (type, offset) in single run: %+v", keys)
		}
	}
}

// TestDetectEvents_EmptyInput tolerates nil / empty inputs.
func TestDetectEvents_EmptyInput(t *testing.T) {
	if got := DetectEvents(nil, Thresholds{}); got != nil {
		t.Errorf("DetectEvents(nil) = %+v, want nil", got)
	}
	if got := DetectEvents(&cereal.DrivingSignals{}, Thresholds{}); got != nil {
		t.Errorf("DetectEvents(empty) = %+v, want nil", got)
	}
}

// TestThresholdsOverride verifies that passing non-default thresholds is
// respected (smaller threshold -> more events fire).
func TestThresholdsOverride(t *testing.T) {
	// 2 m/s^2 sustained over 0.5s. Default threshold (4.5) skips this,
	// but a 1.0 threshold should catch it.
	rows := []row{
		{dt: 0, vEgo: 10},
		{dt: 100 * time.Millisecond, vEgo: 9.8},
		{dt: 200 * time.Millisecond, vEgo: 9.6},
		{dt: 300 * time.Millisecond, vEgo: 9.4},
		{dt: 400 * time.Millisecond, vEgo: 9.2},
		{dt: 500 * time.Millisecond, vEgo: 9.0},
	}
	sig := buildSignals(rows)

	defaults := DetectEvents(sig, Thresholds{})
	for _, e := range defaults {
		if e.Type == EventTypeHardBrake {
			t.Errorf("default thresholds: unexpected hard_brake %+v", e)
		}
	}

	loose := DetectEvents(sig, Thresholds{HardBrakeMps2: 1.0, HardBrakeMinDurationSec: 0.1})
	var n int
	for _, e := range loose {
		if e.Type == EventTypeHardBrake {
			n++
		}
	}
	if n != 1 {
		t.Errorf("loose thresholds: expected 1 hard_brake, got %d (%v)", n, eventTypes(loose))
	}
}

// buildWorkerQlogFixture builds a small uncompressed cap'n proto qlog
// stream containing one carState (vEgo=10) and one selfdriveState
// (engaged=true) event. We keep it self-contained here rather than reach
// across packages so the worker test stays readable.
func buildWorkerQlogFixture(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)

	// carState frame
	msg0, seg0, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	evt0, err := schema.NewRootEvent(seg0)
	if err != nil {
		t.Fatalf("NewRootEvent: %v", err)
	}
	evt0.SetLogMonoTime(0)
	evt0.SetValid(true)
	cs, err := evt0.NewCarState()
	if err != nil {
		t.Fatalf("NewCarState: %v", err)
	}
	cs.SetVEgo(10)
	if err := enc.Encode(msg0); err != nil {
		t.Fatalf("Encode 0: %v", err)
	}

	// selfdriveState frame
	msg1, seg1, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	evt1, err := schema.NewRootEvent(seg1)
	if err != nil {
		t.Fatalf("NewRootEvent: %v", err)
	}
	evt1.SetLogMonoTime(100_000_000)
	evt1.SetValid(true)
	ss, err := evt1.NewSelfdriveState()
	if err != nil {
		t.Fatalf("NewSelfdriveState: %v", err)
	}
	ss.SetEnabled(true)
	if err := ss.SetAlertText1("Engaged"); err != nil {
		t.Fatalf("SetAlertText1: %v", err)
	}
	if err := enc.Encode(msg1); err != nil {
		t.Fatalf("Encode 1: %v", err)
	}

	return buf.Bytes()
}

// writeWorkerQlogFile drops a file named filename into the segment dir on
// disk under the given storage root, creating parent directories. Returns
// the absolute path written.
func writeWorkerQlogFile(t *testing.T, store *storage.Storage, dongle, route, segment, filename string, data []byte) string {
	t.Helper()
	dir := filepath.Dir(store.Path(dongle, route, segment, filename))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll %s: %v", dir, err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	return path
}

// TestEventDetectorExtractPrefersZstd is the qlog-zstd-support feature's
// worker-side regression guard: a route whose only qlog file is qlog.zst
// must still produce a non-empty DrivingSignals through the default
// (non-overridden) extract path. This exercises the qlogPickerOrder
// branch that picks qlog.zst plus the cereal parser's zstd decode.
func TestEventDetectorExtractPrefersZstd(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())

	// Compress the fixture with zstd and write only the .zst file. No
	// qlog.bz2 / qlog on disk -- this is the "modern device" case.
	fixture := buildWorkerQlogFixture(t)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(fixture, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	writeWorkerQlogFile(t, store, dongle, route, "0", "qlog.zst", compressed)

	d := &EventDetector{Storage: store}
	sig, err := d.extract(context.Background(), dongle, route)
	if err != nil {
		t.Fatalf("extract from qlog.zst: %v", err)
	}
	if sig == nil || len(sig.Times) == 0 {
		t.Fatalf("extract from qlog.zst produced empty signals: %+v", sig)
	}
	if got := len(sig.Times); got != 2 {
		t.Errorf("len(Times) = %d, want 2 from fixture", got)
	}
	if sig.VEgo[0] != 10 {
		t.Errorf("VEgo[0] = %v, want 10", sig.VEgo[0])
	}
	if !sig.Engaged[1] {
		t.Errorf("Engaged[1] = %v, want true", sig.Engaged[1])
	}
	if sig.AlertText[1] != "Engaged" {
		t.Errorf("AlertText[1] = %q, want %q", sig.AlertText[1], "Engaged")
	}
}

// TestEventDetectorPickerOrderPrefersZstdOverBz2 confirms that when both
// qlog.zst and qlog.bz2 are on disk, the picker opens qlog.zst first.
// The two files carry distinguishable payloads (different vEgo) so we can
// tell which one the extractor read.
func TestEventDetectorPickerOrderPrefersZstdOverBz2(t *testing.T) {
	const dongle = "dongle-1"
	const route = "2024-03-15--12-30-00"

	store := storage.New(t.TempDir())

	// Build a "winning" fixture (vEgo=42) and compress as zstd.
	winning := buildWorkerVEgoFixture(t, 42)
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll(winning, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	writeWorkerQlogFile(t, store, dongle, route, "0", "qlog.zst", compressed)

	// And a "losing" raw qlog on disk under the legacy name. The cereal
	// parser would happily decode this if the picker preferred it; the
	// test asserts it does not.
	losing := buildWorkerVEgoFixture(t, 7)
	writeWorkerQlogFile(t, store, dongle, route, "0", "qlog.bz2", losing) // raw bytes; never opened by parser, so the malformed framing is fine -- just needs to exist on disk

	d := &EventDetector{Storage: store}
	sig, err := d.extract(context.Background(), dongle, route)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if sig == nil || len(sig.VEgo) == 0 {
		t.Fatalf("extract produced empty signals")
	}
	if sig.VEgo[0] != 42 {
		t.Errorf("VEgo[0] = %v, want 42 (the zstd fixture); picker may be honoring qlog.bz2 ahead of qlog.zst", sig.VEgo[0])
	}
}

// buildWorkerVEgoFixture builds a one-frame uncompressed qlog with a
// single carState event whose vEgo is the supplied value. Used to make
// the picker-order test self-describing.
func buildWorkerVEgoFixture(t *testing.T, vEgo float32) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)

	msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	evt, err := schema.NewRootEvent(seg)
	if err != nil {
		t.Fatalf("NewRootEvent: %v", err)
	}
	evt.SetLogMonoTime(0)
	evt.SetValid(true)
	cs, err := evt.NewCarState()
	if err != nil {
		t.Fatalf("NewCarState: %v", err)
	}
	cs.SetVEgo(vEgo)
	if err := enc.Encode(msg); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return buf.Bytes()
}

// TestQlogPickerOrder pins the on-disk preference order in the worker's
// extract loop. Newer-first ordering means a modern device's qlog.zst
// upload wins over a stale qlog.bz2 left behind by a partial reupload.
func TestQlogPickerOrder(t *testing.T) {
	want := []string{"qlog.zst", "qlog.bz2", "qlog"}
	if len(qlogPickerOrder) != len(want) {
		t.Fatalf("qlogPickerOrder = %v, want %v", qlogPickerOrder, want)
	}
	for i, name := range want {
		if qlogPickerOrder[i] != name {
			t.Errorf("qlogPickerOrder[%d] = %q, want %q", i, qlogPickerOrder[i], name)
		}
	}
}

// TestLoadThresholdsFromEnv verifies env-var parsing with both defaults and
// overrides.
func TestLoadThresholdsFromEnv(t *testing.T) {
	t.Setenv("EVENT_HARD_BRAKE_MPS2", "")
	t.Setenv("EVENT_HARD_BRAKE_MIN_SEC", "")
	def := LoadThresholdsFromEnv()
	if def.HardBrakeMps2 != defaultHardBrakeMps2 {
		t.Errorf("default mps2 = %v, want %v", def.HardBrakeMps2, defaultHardBrakeMps2)
	}
	if def.HardBrakeMinDurationSec != defaultHardBrakeMinSec {
		t.Errorf("default min-sec = %v, want %v", def.HardBrakeMinDurationSec, defaultHardBrakeMinSec)
	}

	t.Setenv("EVENT_HARD_BRAKE_MPS2", "3.2")
	t.Setenv("EVENT_HARD_BRAKE_MIN_SEC", "0.5")
	override := LoadThresholdsFromEnv()
	if override.HardBrakeMps2 != 3.2 {
		t.Errorf("override mps2 = %v, want 3.2", override.HardBrakeMps2)
	}
	if override.HardBrakeMinDurationSec != 0.5 {
		t.Errorf("override min-sec = %v, want 0.5", override.HardBrakeMinDurationSec)
	}

	// Garbage values fall back to defaults (logged, not crashed).
	t.Setenv("EVENT_HARD_BRAKE_MPS2", "not-a-number")
	t.Setenv("EVENT_HARD_BRAKE_MIN_SEC", "-1")
	bad := LoadThresholdsFromEnv()
	if bad.HardBrakeMps2 != defaultHardBrakeMps2 {
		t.Errorf("bad mps2 fallback = %v, want %v", bad.HardBrakeMps2, defaultHardBrakeMps2)
	}
	if bad.HardBrakeMinDurationSec != defaultHardBrakeMinSec {
		t.Errorf("bad min-sec fallback = %v, want %v", bad.HardBrakeMinDurationSec, defaultHardBrakeMinSec)
	}
}

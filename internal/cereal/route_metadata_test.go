package cereal

import (
	"bytes"
	"testing"
	"time"

	capnp "capnproto.org/go/capnp/v3"

	"comma-personal-backend/internal/cereal/schema"
)

// buildRouteMetadataFixture assembles a tiny qlog stream that exercises
// every code path RouteMetadataExtractor cares about: an initData with
// wallTimeNanos, a couple of gpsLocation fixes, a gpsLocationExternal
// fix, a liveLocationKalman fix, plus a no-fix gpsLocation that should
// be filtered out and a duplicate fix that should be deduped.
//
// The wall-clock timestamps live well after 2010 so the
// plausibleWallClock guard accepts them. Latitudes/longitudes are tightly
// clustered around a single block so the dedupe epsilon eliminates the
// duplicate but lets the genuine motion through.
func buildRouteMetadataFixture(t *testing.T, includeInitData bool) []byte {
	t.Helper()

	// 2024-05-01T10:00:00Z and a few hundred milliseconds onwards.
	baseWall := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	type frame struct {
		monoTimeNs uint64
		// One of these is set per frame.
		initWallNs  uint64
		gpsLat      float64
		gpsLng      float64
		gpsHasFix   bool
		gpsTimeMs   int64
		gpsExt      bool
		llkLat      float64
		llkLng      float64
		llkValid    bool
		llkGpsOK    bool
		llkTimeMs   int64
		useGps      bool
		useLlk      bool
		useInitData bool
	}

	wallMs := baseWall.UnixMilli()

	frames := []frame{
		// initData carrying the canonical start wall-clock.
		{monoTimeNs: 0, useInitData: true, initWallNs: uint64(baseWall.UnixNano())},
		// First real GPS fix at +20 ms.
		{monoTimeNs: 20_000_000, useGps: true, gpsHasFix: true,
			gpsLat: 37.700001, gpsLng: -122.400001, gpsTimeMs: wallMs + 20},
		// Duplicate same-coords fix at +40 ms -- should be deduped out.
		{monoTimeNs: 40_000_000, useGps: true, gpsHasFix: true,
			gpsLat: 37.700001, gpsLng: -122.400001, gpsTimeMs: wallMs + 40},
		// External GPS fix at +60 ms moves a few meters east.
		{monoTimeNs: 60_000_000, useGps: true, gpsExt: true, gpsHasFix: true,
			gpsLat: 37.700050, gpsLng: -122.399950, gpsTimeMs: wallMs + 60},
		// No-fix GPS event at +80 ms -- must NOT be appended even though
		// the lat/lng would otherwise pass the validity check.
		{monoTimeNs: 80_000_000, useGps: true, gpsHasFix: false,
			gpsLat: 37.701, gpsLng: -122.401, gpsTimeMs: wallMs + 80},
		// liveLocationKalman valid + gpsOK at +100 ms moves further east.
		{monoTimeNs: 100_000_000, useLlk: true, llkValid: true, llkGpsOK: true,
			llkLat: 37.700100, llkLng: -122.399900, llkTimeMs: wallMs + 100},
		// liveLocationKalman that is NOT valid at +120 ms -- must be skipped.
		{monoTimeNs: 120_000_000, useLlk: true, llkValid: false, llkGpsOK: true,
			llkLat: 37.800, llkLng: -122.500, llkTimeMs: wallMs + 120},
		// Final GPS fix at +200 ms gives the end timestamp.
		{monoTimeNs: 200_000_000, useGps: true, gpsHasFix: true,
			gpsLat: 37.700200, gpsLng: -122.399800, gpsTimeMs: wallMs + 200},
	}

	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)
	for i, fr := range frames {
		if fr.useInitData && !includeInitData {
			continue
		}
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		if err != nil {
			t.Fatalf("frame %d: NewMessage: %v", i, err)
		}
		evt, err := schema.NewRootEvent(seg)
		if err != nil {
			t.Fatalf("frame %d: NewRootEvent: %v", i, err)
		}
		evt.SetLogMonoTime(fr.monoTimeNs)
		evt.SetValid(true)

		switch {
		case fr.useInitData:
			id, err := evt.NewInitData()
			if err != nil {
				t.Fatalf("frame %d: NewInitData: %v", i, err)
			}
			id.SetWallTimeNanos(fr.initWallNs)
		case fr.useGps && fr.gpsExt:
			gps, err := evt.NewGpsLocationExternal()
			if err != nil {
				t.Fatalf("frame %d: NewGpsLocationExternal: %v", i, err)
			}
			gps.SetLatitude(fr.gpsLat)
			gps.SetLongitude(fr.gpsLng)
			gps.SetUnixTimestampMillis(fr.gpsTimeMs)
			gps.SetHasFix(fr.gpsHasFix)
		case fr.useGps:
			gps, err := evt.NewGpsLocation()
			if err != nil {
				t.Fatalf("frame %d: NewGpsLocation: %v", i, err)
			}
			gps.SetLatitude(fr.gpsLat)
			gps.SetLongitude(fr.gpsLng)
			gps.SetUnixTimestampMillis(fr.gpsTimeMs)
			gps.SetHasFix(fr.gpsHasFix)
		case fr.useLlk:
			llk, err := evt.NewLiveLocationKalmanDEPRECATED()
			if err != nil {
				t.Fatalf("frame %d: NewLiveLocationKalmanDEPRECATED: %v", i, err)
			}
			if fr.llkValid {
				llk.SetStatus(schema.LiveLocationKalman_Status_valid)
			} else {
				llk.SetStatus(schema.LiveLocationKalman_Status_uncalibrated)
			}
			llk.SetGpsOK(fr.llkGpsOK)
			llk.SetUnixTimestampMillis(fr.llkTimeMs)
			pos, err := llk.NewPositionGeodetic()
			if err != nil {
				t.Fatalf("frame %d: NewPositionGeodetic: %v", i, err)
			}
			vals, err := pos.NewValue(3)
			if err != nil {
				t.Fatalf("frame %d: NewValue: %v", i, err)
			}
			vals.Set(0, fr.llkLat)
			vals.Set(1, fr.llkLng)
			vals.Set(2, 0) // altitude
			pos.SetValid(true)
		}

		if err := enc.Encode(msg); err != nil {
			t.Fatalf("frame %d: Encode: %v", i, err)
		}
	}
	return buf.Bytes()
}

func TestRouteMetadataExtractor_FullFixture(t *testing.T) {
	fx := buildRouteMetadataFixture(t, true)

	e := &RouteMetadataExtractor{}
	meta, err := e.ExtractRouteMetadata(bytes.NewReader(fx))
	if err != nil {
		t.Fatalf("ExtractRouteMetadata: %v", err)
	}
	if meta == nil {
		t.Fatal("nil RouteMetadata")
	}

	// initData wins for StartTime -- 2024-05-01T10:00:00Z exactly.
	wantStart := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	if !meta.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v", meta.StartTime, wantStart)
	}
	// EndTime should be the last GPS-derived wall-clock (+200ms).
	wantEnd := wantStart.Add(200 * time.Millisecond)
	if !meta.EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v", meta.EndTime, wantEnd)
	}

	// Track expectations:
	//   gpsLocation (+20ms)         -> kept
	//   gpsLocation duplicate (+40) -> deduped
	//   gpsLocationExternal (+60)   -> kept
	//   gpsLocation no-fix (+80)    -> dropped
	//   liveLocationKalman (+100)   -> kept (valid + gpsOK)
	//   liveLocationKalman invalid  -> dropped
	//   gpsLocation (+200)          -> kept
	// Expected count: 4 distinct points.
	if got, want := len(meta.Track), 4; got != want {
		t.Fatalf("len(Track) = %d, want %d (track=%+v)", got, want, meta.Track)
	}

	// First and last coordinates pin down ordering and source mixing.
	if meta.Track[0].Lat != 37.700001 || meta.Track[0].Lng != -122.400001 {
		t.Errorf("Track[0] = %+v, want first gpsLocation fix", meta.Track[0])
	}
	last := meta.Track[len(meta.Track)-1]
	if last.Lat != 37.700200 || last.Lng != -122.399800 {
		t.Errorf("Track[last] = %+v, want final gpsLocation fix", last)
	}
}

// TestRouteMetadataExtractor_NoInitDataFallsBackToGps confirms that when
// initData is missing the StartTime/EndTime envelope still gets populated
// from the earliest/latest GPS-derived event timestamps.
func TestRouteMetadataExtractor_NoInitDataFallsBackToGps(t *testing.T) {
	fx := buildRouteMetadataFixture(t, false)

	e := &RouteMetadataExtractor{}
	meta, err := e.ExtractRouteMetadata(bytes.NewReader(fx))
	if err != nil {
		t.Fatalf("ExtractRouteMetadata: %v", err)
	}

	baseWall := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	wantStart := baseWall.Add(20 * time.Millisecond)
	wantEnd := baseWall.Add(200 * time.Millisecond)
	if !meta.StartTime.Equal(wantStart) {
		t.Errorf("StartTime = %v, want %v (earliest GPS fix)", meta.StartTime, wantStart)
	}
	if !meta.EndTime.Equal(wantEnd) {
		t.Errorf("EndTime = %v, want %v (latest GPS fix)", meta.EndTime, wantEnd)
	}
	if len(meta.Track) == 0 {
		t.Error("Track is empty, want fallback path to still produce points")
	}
}

// TestRouteMetadataExtractor_StaleInitDataFallsBackToGps covers the
// stale-RTC scenario: the device boots with a wall-clock that is weeks
// behind the actual time (RTC battery dead, no NTP yet) so initData
// stamps a March wallTimeNanos onto a route that GPS later confirms
// happened in April. The extractor must reject the wildly off initData
// in favour of the GPS envelope; otherwise the trip gets a 30-day
// duration that pollutes every dashboard total downstream.
func TestRouteMetadataExtractor_StaleInitDataFallsBackToGps(t *testing.T) {
	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)

	staleInit := time.Date(2026, 3, 24, 13, 46, 17, 0, time.UTC)
	gpsStart := time.Date(2026, 4, 23, 17, 5, 30, 0, time.UTC)
	gpsEnd := gpsStart.Add(9 * time.Minute)

	type frame struct {
		monoTimeNs uint64
		isInitData bool
		isGps      bool
		gpsLat     float64
		gpsLng     float64
		gpsTimeMs  int64
		gpsHasFix  bool
		initWallNs uint64
	}
	frames := []frame{
		{monoTimeNs: 0, isInitData: true, initWallNs: uint64(staleInit.UnixNano())},
		{monoTimeNs: 1_000_000, isGps: true, gpsHasFix: true,
			gpsLat: 37.700, gpsLng: -122.400, gpsTimeMs: gpsStart.UnixMilli()},
		{monoTimeNs: 540_000_000_000, isGps: true, gpsHasFix: true,
			gpsLat: 37.701, gpsLng: -122.399, gpsTimeMs: gpsEnd.UnixMilli()},
	}

	for i, fr := range frames {
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		if err != nil {
			t.Fatalf("frame %d: NewMessage: %v", i, err)
		}
		evt, err := schema.NewRootEvent(seg)
		if err != nil {
			t.Fatalf("frame %d: NewRootEvent: %v", i, err)
		}
		evt.SetLogMonoTime(fr.monoTimeNs)
		evt.SetValid(true)
		switch {
		case fr.isInitData:
			id, err := evt.NewInitData()
			if err != nil {
				t.Fatalf("frame %d: NewInitData: %v", i, err)
			}
			id.SetWallTimeNanos(fr.initWallNs)
		case fr.isGps:
			gps, err := evt.NewGpsLocation()
			if err != nil {
				t.Fatalf("frame %d: NewGpsLocation: %v", i, err)
			}
			gps.SetLatitude(fr.gpsLat)
			gps.SetLongitude(fr.gpsLng)
			gps.SetUnixTimestampMillis(fr.gpsTimeMs)
			gps.SetHasFix(fr.gpsHasFix)
		}
		if err := enc.Encode(msg); err != nil {
			t.Fatalf("frame %d: Encode: %v", i, err)
		}
	}

	e := &RouteMetadataExtractor{}
	meta, err := e.ExtractRouteMetadata(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ExtractRouteMetadata: %v", err)
	}

	if !meta.StartTime.Equal(gpsStart) {
		t.Errorf("StartTime = %v, want %v (first GPS fix, not stale initData %v)",
			meta.StartTime, gpsStart, staleInit)
	}
	if !meta.EndTime.Equal(gpsEnd) {
		t.Errorf("EndTime = %v, want %v (last GPS fix)", meta.EndTime, gpsEnd)
	}
	if dur := meta.EndTime.Sub(meta.StartTime); dur > time.Hour {
		t.Errorf("derived duration = %s, want a few minutes; stale initData was not rejected", dur)
	}
}

// TestRouteMetadataExtractor_InitDataCloseToGpsKept verifies the inverse:
// when the initData wall-clock is only seconds or minutes off from the
// first GPS fix (the normal NTP-recently-synced path), it remains the
// authoritative StartTime because it captures the boot moment that
// precedes the first GPS lock. The drift-rejection logic must not
// regress this common case.
func TestRouteMetadataExtractor_InitDataCloseToGpsKept(t *testing.T) {
	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)

	initWall := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	// First GPS fix is 30s after init -- a realistic time-to-first-fix.
	gpsStart := initWall.Add(30 * time.Second)
	gpsEnd := initWall.Add(5 * time.Minute)

	type frame struct {
		monoTimeNs uint64
		isInitData bool
		isGps      bool
		gpsLat     float64
		gpsLng     float64
		gpsTimeMs  int64
		gpsHasFix  bool
		initWallNs uint64
	}
	frames := []frame{
		{monoTimeNs: 0, isInitData: true, initWallNs: uint64(initWall.UnixNano())},
		{monoTimeNs: 30_000_000_000, isGps: true, gpsHasFix: true,
			gpsLat: 37.700, gpsLng: -122.400, gpsTimeMs: gpsStart.UnixMilli()},
		{monoTimeNs: 300_000_000_000, isGps: true, gpsHasFix: true,
			gpsLat: 37.701, gpsLng: -122.399, gpsTimeMs: gpsEnd.UnixMilli()},
	}

	for i, fr := range frames {
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		if err != nil {
			t.Fatalf("frame %d: NewMessage: %v", i, err)
		}
		evt, err := schema.NewRootEvent(seg)
		if err != nil {
			t.Fatalf("frame %d: NewRootEvent: %v", i, err)
		}
		evt.SetLogMonoTime(fr.monoTimeNs)
		evt.SetValid(true)
		switch {
		case fr.isInitData:
			id, err := evt.NewInitData()
			if err != nil {
				t.Fatalf("frame %d: NewInitData: %v", i, err)
			}
			id.SetWallTimeNanos(fr.initWallNs)
		case fr.isGps:
			gps, err := evt.NewGpsLocation()
			if err != nil {
				t.Fatalf("frame %d: NewGpsLocation: %v", i, err)
			}
			gps.SetLatitude(fr.gpsLat)
			gps.SetLongitude(fr.gpsLng)
			gps.SetUnixTimestampMillis(fr.gpsTimeMs)
			gps.SetHasFix(fr.gpsHasFix)
		}
		if err := enc.Encode(msg); err != nil {
			t.Fatalf("frame %d: Encode: %v", i, err)
		}
	}

	e := &RouteMetadataExtractor{}
	meta, err := e.ExtractRouteMetadata(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ExtractRouteMetadata: %v", err)
	}

	if !meta.StartTime.Equal(initWall) {
		t.Errorf("StartTime = %v, want %v (initData when within drift threshold)", meta.StartTime, initWall)
	}
	if !meta.EndTime.Equal(gpsEnd) {
		t.Errorf("EndTime = %v, want %v (last GPS fix)", meta.EndTime, gpsEnd)
	}
}

// TestRouteMetadataExtractor_NoFixYieldsEmptyTrack guards the dedupe and
// hasFix filters: if every GPS event reports hasFix=false, the track must
// be empty even though the lat/lng values would otherwise be valid.
func TestRouteMetadataExtractor_NoFixYieldsEmptyTrack(t *testing.T) {
	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)

	baseWall := time.Date(2024, 5, 2, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		msg, seg, err := capnp.NewMessage(capnp.SingleSegment(nil))
		if err != nil {
			t.Fatalf("frame %d: NewMessage: %v", i, err)
		}
		evt, err := schema.NewRootEvent(seg)
		if err != nil {
			t.Fatalf("frame %d: NewRootEvent: %v", i, err)
		}
		evt.SetLogMonoTime(uint64(i) * 50_000_000)
		gps, err := evt.NewGpsLocation()
		if err != nil {
			t.Fatalf("frame %d: NewGpsLocation: %v", i, err)
		}
		gps.SetLatitude(37.0)
		gps.SetLongitude(-122.0)
		gps.SetUnixTimestampMillis(baseWall.Add(time.Duration(i*50) * time.Millisecond).UnixMilli())
		gps.SetHasFix(false)
		if err := enc.Encode(msg); err != nil {
			t.Fatalf("frame %d: Encode: %v", i, err)
		}
	}

	e := &RouteMetadataExtractor{}
	meta, err := e.ExtractRouteMetadata(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("ExtractRouteMetadata: %v", err)
	}
	if len(meta.Track) != 0 {
		t.Errorf("Track len = %d, want 0 when every fix reports hasFix=false", len(meta.Track))
	}
	// The fallback-only path also yields zero StartTime/EndTime because
	// noteFallback skips events without a usable wall-clock and we
	// explicitly skip the no-fix branch BEFORE we record fallback time --
	// the no-fix events still have valid unixTimestampMillis but they are
	// never observed by the extractor, which is the intended behaviour.
	if !meta.StartTime.IsZero() || !meta.EndTime.IsZero() {
		t.Errorf("expected zero start/end without any usable events: %+v", meta)
	}
}

// TestPlausibleWallClock locks in the bounds on what counts as a usable
// timestamp. The function is small but the bounds are part of the
// contract: a regression that lets pre-2010 timestamps through would
// silently mis-stamp every "no GPS yet" route at the Unix epoch.
func TestPlausibleWallClock(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want bool
	}{
		{"epoch", time.Unix(0, 0).UTC(), false},
		{"pre-2010", time.Date(2009, 12, 31, 23, 59, 59, 0, time.UTC), false},
		{"2010-boundary", time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"2024-typical", time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC), true},
		{"2099", time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC), true},
		{"2100-boundary", time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"zero", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := plausibleWallClock(tc.in); got != tc.want {
				t.Errorf("plausibleWallClock(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

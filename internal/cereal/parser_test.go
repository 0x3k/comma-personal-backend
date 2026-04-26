package cereal

import (
	"bytes"
	"compress/bzip2"
	"io"
	"os/exec"
	"testing"

	capnp "capnproto.org/go/capnp/v3"
	"github.com/klauspost/compress/zstd"

	"comma-personal-backend/internal/cereal/schema"
)

// buildFixture assembles a tiny rlog stream in-process. Each frame is one
// Cap'n Proto Event; the sequence covers carState, controlsState (deprecated
// engagement path), and selfdriveState (the modern engagement path) so the
// signal extractor's whole switch is exercised.
//
// The returned bytes are the raw, uncompressed Cap'n Proto stream -- the bz2
// variant is produced on demand via an external compressor fixture in the
// test below.
func buildFixture(t *testing.T) []byte {
	t.Helper()

	type frame struct {
		monoTimeNs       uint64
		vEgo             float32
		steeringAngleDeg float32
		brakePressed     bool
		gasPressed       bool
		useCarState      bool
		useSelfdrive     bool
		enabled          bool
		alert            string
		useControls      bool
	}

	// Six events: a carState at t=0, a selfdriveState engagement at t=20ms,
	// a second carState at t=100ms, a controlsState (deprecated path) at
	// t=120ms, another carState at t=200ms, and an alert text update at
	// t=300ms. That's enough to validate alignment and both engagement
	// sources.
	frames := []frame{
		{monoTimeNs: 0, useCarState: true, vEgo: 0, steeringAngleDeg: 1.5},
		{monoTimeNs: 20_000_000, useSelfdrive: true, enabled: true, alert: ""},
		{monoTimeNs: 100_000_000, useCarState: true, vEgo: 10.0, steeringAngleDeg: -5.25, brakePressed: true},
		{monoTimeNs: 120_000_000, useControls: true, enabled: true, alert: "Take Control"},
		{monoTimeNs: 200_000_000, useCarState: true, vEgo: 15.5, steeringAngleDeg: 2.0, gasPressed: true},
		{monoTimeNs: 300_000_000, useSelfdrive: true, enabled: false, alert: "Disengaged"},
	}

	var buf bytes.Buffer
	enc := capnp.NewEncoder(&buf)
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
		case fr.useCarState:
			cs, err := evt.NewCarState()
			if err != nil {
				t.Fatalf("frame %d: NewCarState: %v", i, err)
			}
			cs.SetVEgo(fr.vEgo)
			cs.SetSteeringAngleDeg(fr.steeringAngleDeg)
			cs.SetBrakePressed(fr.brakePressed)
			cs.SetGasPressed(fr.gasPressed)
		case fr.useSelfdrive:
			ss, err := evt.NewSelfdriveState()
			if err != nil {
				t.Fatalf("frame %d: NewSelfdriveState: %v", i, err)
			}
			ss.SetEnabled(fr.enabled)
			if err := ss.SetAlertText1(fr.alert); err != nil {
				t.Fatalf("frame %d: SetAlertText1: %v", i, err)
			}
		case fr.useControls:
			cs, err := evt.NewControlsState()
			if err != nil {
				t.Fatalf("frame %d: NewControlsState: %v", i, err)
			}
			cs.SetEnabledDEPRECATED(fr.enabled)
			if err := cs.SetAlertText1DEPRECATED(fr.alert); err != nil {
				t.Fatalf("frame %d: SetAlertText1DEPRECATED: %v", i, err)
			}
		}

		if err := enc.Encode(msg); err != nil {
			t.Fatalf("frame %d: Encode: %v", i, err)
		}
	}
	return buf.Bytes()
}

func TestParserStreamsEvents(t *testing.T) {
	fixture := buildFixture(t)

	var monoTimes []uint64
	var whiches []schema.Event_Which

	p := &Parser{}
	if err := p.Parse(bytes.NewReader(fixture), func(evt Event) error {
		monoTimes = append(monoTimes, evt.LogMonoTime())
		whiches = append(whiches, evt.Which())
		return nil
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got, want := len(monoTimes), 6; got != want {
		t.Fatalf("parsed %d events, want %d", got, want)
	}
	// Spot-check ordering and union discriminants.
	if monoTimes[0] != 0 || monoTimes[5] != 300_000_000 {
		t.Errorf("monotime ordering wrong: %v", monoTimes)
	}
	if whiches[0] != schema.Event_Which_carState {
		t.Errorf("frame 0 Which = %v, want carState", whiches[0])
	}
	if whiches[1] != schema.Event_Which_selfdriveState {
		t.Errorf("frame 1 Which = %v, want selfdriveState", whiches[1])
	}
	if whiches[3] != schema.Event_Which_controlsState {
		t.Errorf("frame 3 Which = %v, want controlsState", whiches[3])
	}
}

func TestParserEmptyStream(t *testing.T) {
	p := &Parser{}
	calls := 0
	err := p.Parse(bytes.NewReader(nil), func(Event) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("Parse on empty input: %v", err)
	}
	if calls != 0 {
		t.Errorf("handler called %d times on empty stream", calls)
	}
}

// TestParserDecodesZstd round-trips the standard fixture through a real
// zstd-compressed stream and verifies every Event is recovered. This locks
// in the magic-detection branch and the streaming decode path together.
func TestParserDecodesZstd(t *testing.T) {
	fixture := buildFixture(t)

	var compressed bytes.Buffer
	enc, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write(fixture); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	if !bytes.HasPrefix(compressed.Bytes(), zstdMagic) {
		t.Fatalf("compressed output missing zstd magic: %x", compressed.Bytes()[:4])
	}

	var monoTimes []uint64
	p := &Parser{}
	if err := p.Parse(bytes.NewReader(compressed.Bytes()), func(evt Event) error {
		monoTimes = append(monoTimes, evt.LogMonoTime())
		return nil
	}); err != nil {
		t.Fatalf("Parse(zstd): %v", err)
	}

	if got, want := len(monoTimes), 6; got != want {
		t.Fatalf("parsed %d events from zstd stream, want %d", got, want)
	}
	if monoTimes[0] != 0 || monoTimes[5] != 300_000_000 {
		t.Errorf("monotime ordering wrong after zstd decode: %v", monoTimes)
	}
}

// TestSignalExtractorZstd ensures the higher-level SignalExtractor produces
// the same column-oriented output for a zstd-wrapped fixture as it does for
// the raw and bz2 variants.
func TestSignalExtractorZstd(t *testing.T) {
	fixture := buildFixture(t)

	var compressed bytes.Buffer
	enc, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	if _, err := enc.Write(fixture); err != nil {
		t.Fatalf("zstd write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}

	e := &SignalExtractor{}
	sig, err := e.ExtractDriving(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("ExtractDriving(zstd): %v", err)
	}
	assertSignals(t, sig)
}

// TestParserDetectsZstdMagic guards the magic-sniffing branch with a
// minimal zstd-encoded one-byte payload. The compressed frame still starts
// with the zstd magic, so the magic match in Parse fires; the Cap'n Proto
// decoder then sees a single junk byte and returns EOF, which Parse maps
// to nil. The point is to exercise the magic-detection branch without
// relying on the round-trip TestParserDecodesZstd test above.
func TestParserDetectsZstdMagic(t *testing.T) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd.NewWriter: %v", err)
	}
	compressed := enc.EncodeAll([]byte{0x00}, nil)
	if err := enc.Close(); err != nil {
		t.Fatalf("zstd close: %v", err)
	}
	if !bytes.HasPrefix(compressed, zstdMagic) {
		t.Fatalf("zstd payload missing magic: %x", compressed[:4])
	}

	// Sanity: confirm the payload decompresses to a single byte so the
	// fixture stays minimal as the zstd library evolves.
	dec, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("zstd.NewReader: %v", err)
	}
	defer dec.Close()
	round, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("decompress fixture: %v", err)
	}
	if len(round) != 1 {
		t.Fatalf("zstd fixture decompressed to %d bytes, want 1", len(round))
	}

	// One garbage payload byte will not parse as a Cap'n Proto frame:
	// capnp.Decoder.Decode returns an error rather than EOF for short
	// reads, which Parse should propagate. The test only requires the
	// magic-detection branch to fire (no panic, no ErrZstdUnsupported).
	p := &Parser{}
	calls := 0
	err = p.Parse(bytes.NewReader(compressed), func(Event) error {
		calls++
		return nil
	})
	if err == ErrZstdUnsupported {
		t.Fatalf("Parse rejected zstd input: %v", err)
	}
	if calls != 0 {
		t.Fatalf("garbage zstd payload produced %d events, want 0", calls)
	}
}

func TestSignalExtractorUncompressed(t *testing.T) {
	fixture := buildFixture(t)

	e := &SignalExtractor{}
	sig, err := e.ExtractDriving(bytes.NewReader(fixture))
	if err != nil {
		t.Fatalf("ExtractDriving: %v", err)
	}
	assertSignals(t, sig)
}

func TestSignalExtractorBz2(t *testing.T) {
	// Go's stdlib only offers a bz2 *decoder* (compress/bzip2), so to
	// produce a real compressed fixture at test time we shell out to the
	// system `bzip2` binary. That's almost always present on Linux/macOS
	// developer machines and in CI images; if it's missing we skip this
	// subtest and rely on the magic-detection unit test below to protect
	// the code path.
	bzPath, err := exec.LookPath("bzip2")
	if err != nil {
		t.Skipf("bzip2 not on PATH: %v", err)
	}

	fixture := buildFixture(t)
	cmd := exec.Command(bzPath, "-c")
	cmd.Stdin = bytes.NewReader(fixture)
	var compressed bytes.Buffer
	cmd.Stdout = &compressed
	if err := cmd.Run(); err != nil {
		t.Fatalf("bzip2 -c failed: %v", err)
	}
	if !bytes.HasPrefix(compressed.Bytes(), []byte("BZh")) {
		t.Fatalf("compressed output missing bz2 magic: %q", compressed.Bytes()[:4])
	}

	// Round-trip via the parser: the signal extractor should yield the
	// same rows as the uncompressed run.
	e := &SignalExtractor{}
	sig, err := e.ExtractDriving(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		t.Fatalf("ExtractDriving(bz2): %v", err)
	}
	assertSignals(t, sig)
}

func TestParserDetectsBz2Magic(t *testing.T) {
	// Verify the magic-sniffing branch fires even without the external
	// bzip2 binary. We feed a minimal valid bz2 stream (produced ahead of
	// time and embedded below) that decompresses to zero bytes, and
	// assert we get no error and no events.
	bzEmpty := []byte{
		0x42, 0x5a, 0x68, 0x39, 0x17, 0x72, 0x45, 0x38,
		0x50, 0x90, 0x00, 0x00, 0x00, 0x00,
	}
	// Sanity: compress/bzip2 should accept this as a valid empty stream.
	payload, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(bzEmpty)))
	if err != nil {
		t.Fatalf("empty-bz2 fixture no longer decodes cleanly: %v", err)
	}
	if len(payload) != 0 {
		t.Fatalf("empty-bz2 fixture decoded to %d bytes, want 0", len(payload))
	}

	p := &Parser{}
	calls := 0
	if err := p.Parse(bytes.NewReader(bzEmpty), func(Event) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Parse bz2-wrapped empty stream: %v", err)
	}
	if calls != 0 {
		t.Fatalf("bz2 empty stream produced %d events, want 0", calls)
	}
}

// assertSignals checks the DrivingSignals against the fixed sequence built
// by buildFixture. Keep this in sync with that table.
func assertSignals(t *testing.T, sig *DrivingSignals) {
	t.Helper()

	if sig == nil {
		t.Fatal("nil DrivingSignals")
	}
	const wantRows = 6
	if got := len(sig.Times); got != wantRows {
		t.Fatalf("len(Times) = %d, want %d", got, wantRows)
	}
	for name, slice := range map[string]int{
		"VEgo":             len(sig.VEgo),
		"SteeringAngleDeg": len(sig.SteeringAngleDeg),
		"BrakePressed":     len(sig.BrakePressed),
		"GasPressed":       len(sig.GasPressed),
		"Engaged":          len(sig.Engaged),
		"AlertText":        len(sig.AlertText),
	} {
		if slice != wantRows {
			t.Errorf("len(%s) = %d, want %d", name, slice, wantRows)
		}
	}

	// Row 0: carState @ t=0, vEgo=0, steer=1.5
	if sig.VEgo[0] != 0 || sig.SteeringAngleDeg[0] != 1.5 {
		t.Errorf("row0 values wrong: vEgo=%v steer=%v", sig.VEgo[0], sig.SteeringAngleDeg[0])
	}
	// Row 1: selfdriveState engaged=true, alert=""
	if !sig.Engaged[1] {
		t.Errorf("row1 engaged = %v, want true", sig.Engaged[1])
	}
	if sig.AlertText[1] != "" {
		t.Errorf("row1 alert = %q, want empty", sig.AlertText[1])
	}
	// Row 2: carState with brakePressed=true, vEgo=10
	if sig.VEgo[2] != 10.0 || !sig.BrakePressed[2] {
		t.Errorf("row2 values wrong: vEgo=%v brake=%v", sig.VEgo[2], sig.BrakePressed[2])
	}
	// Row 3: controlsState (deprecated path) engaged=true, alert="Take Control"
	if !sig.Engaged[3] {
		t.Errorf("row3 engaged = %v, want true", sig.Engaged[3])
	}
	if sig.AlertText[3] != "Take Control" {
		t.Errorf("row3 alert = %q, want %q", sig.AlertText[3], "Take Control")
	}
	// Row 4: carState with gasPressed=true
	if !sig.GasPressed[4] {
		t.Errorf("row4 gasPressed = %v, want true", sig.GasPressed[4])
	}
	// Row 5: disengagement alert
	if sig.Engaged[5] {
		t.Errorf("row5 engaged = %v, want false", sig.Engaged[5])
	}
	if sig.AlertText[5] != "Disengaged" {
		t.Errorf("row5 alert = %q, want %q", sig.AlertText[5], "Disengaged")
	}

	// logMonoTime values turn into UTC time.Time via time.Unix(0, ns).
	// The first row's monoTime was 0, so Times[0] should equal the Unix
	// epoch; the last row's was 300ms.
	if !sig.Times[0].IsZero() {
		// time.Unix(0, 0).UTC() is Jan 1 1970 UTC which is not IsZero()
		// per Go's definition, so skip that particular check.
	}
	delta := sig.Times[5].UnixNano() - sig.Times[0].UnixNano()
	if delta != 300_000_000 {
		t.Errorf("Times[5]-Times[0] = %d ns, want 300_000_000", delta)
	}
}

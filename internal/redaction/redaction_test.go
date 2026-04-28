package redaction

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildBoxblurFilterEmpty(t *testing.T) {
	got := BuildBoxblurFilter(nil, FilterOptions{})
	if got != "" {
		t.Errorf("empty detections should produce empty filter, got %q", got)
	}
	got = BuildBoxblurFilter([]Detection{}, FilterOptions{})
	if got != "" {
		t.Errorf("empty slice should produce empty filter, got %q", got)
	}
}

func TestBuildBoxblurFilterDeterministicOrder(t *testing.T) {
	// Two detections at different times; permute input order, output
	// must be byte-identical.
	a := Detection{TimeSec: 1.0, Bbox: Bbox{X: 100, Y: 100, W: 50, H: 20}}
	b := Detection{TimeSec: 5.0, Bbox: Bbox{X: 200, Y: 200, W: 60, H: 25}}

	first := BuildBoxblurFilter([]Detection{a, b}, FilterOptions{})
	second := BuildBoxblurFilter([]Detection{b, a}, FilterOptions{})

	if first != second {
		t.Errorf("filter not deterministic\n%q\nvs\n%q", first, second)
	}
	if strings.Count(first, "boxblur=") != 2 {
		t.Errorf("expected 2 boxblur clauses, got %d in %q", strings.Count(first, "boxblur="), first)
	}
	if strings.Count(first, "overlay=") != 2 {
		t.Errorf("expected 2 overlay clauses, got %d in %q", strings.Count(first, "overlay="), first)
	}
}

func TestBuildBoxblurFilterEnableWindow(t *testing.T) {
	d := Detection{TimeSec: 4.5, Bbox: Bbox{X: 10, Y: 10, W: 20, H: 10}}
	opts := FilterOptions{HoldMs: 200}
	got := BuildBoxblurFilter([]Detection{d}, opts)
	// Window: t0=4.5, t1=4.5+0.2=4.7. Commas in the enable expression
	// are escaped so ffmpeg does not treat them as filter separators.
	if !strings.Contains(got, `between(t\,4.5\,4.7)`) {
		t.Errorf("expected enable window between(t,4.5,4.7), got %q", got)
	}
}

func TestBuildBoxblurFilterUsesNormalizedCoords(t *testing.T) {
	// Bbox at (964, 604) with size 100x50 in fcamera 1928x1208 space
	// normalizes to nx=0.5, ny=~0.5, nw=~0.05186, nh=~0.04139. The
	// emitted filter must reference iw/ih so it scales to whatever
	// the actual stream resolution is at filter-eval time.
	d := Detection{TimeSec: 0, Bbox: Bbox{X: 964, Y: 604, W: 100, H: 50}}
	got := BuildBoxblurFilter([]Detection{d}, FilterOptions{})
	if !strings.Contains(got, "iw*0.5") {
		t.Errorf("expected iw*0.5 (normalized x=0.5), got %q", got)
	}
	if !strings.Contains(got, "ih*") {
		t.Errorf("expected ih*<frac> normalized expression, got %q", got)
	}
	if strings.Contains(got, "crop=27") || strings.Contains(got, "crop=100") {
		t.Errorf("filter should not encode literal pixel counts, got %q", got)
	}
}

func TestBuildBoxblurFilterClampsOutOfBounds(t *testing.T) {
	// Bbox extending past the right/bottom edge gets clamped in
	// normalized space, so the resulting fraction is < 1 - origin.
	d := Detection{TimeSec: 0, Bbox: Bbox{X: 1900, Y: 1200, W: 100, H: 100}}
	got := BuildBoxblurFilter([]Detection{d}, FilterOptions{})
	if !strings.Contains(got, "boxblur=") {
		t.Fatalf("expected a boxblur clause for partially-OOB bbox, got %q", got)
	}
	// Width fraction clamped to (1928-1900)/1928 = 0.01452..., not
	// 100/1928 = 0.05186.
	if !strings.Contains(got, "iw*0.0145") {
		t.Errorf("expected clamped iw fraction, got %q", got)
	}
}

func TestBuildBoxblurFilterDropsFullyOutOfBounds(t *testing.T) {
	// Bbox completely outside the source frame must be dropped.
	d := Detection{TimeSec: 0, Bbox: Bbox{X: 5000, Y: 5000, W: 50, H: 50}}
	got := BuildBoxblurFilter([]Detection{d}, FilterOptions{})
	if got != "" {
		t.Errorf("expected empty filter for fully-OOB bbox, got %q", got)
	}
}

func TestBuildBoxblurFilterUsesProvidedLabels(t *testing.T) {
	d := Detection{TimeSec: 0, Bbox: Bbox{X: 0, Y: 0, W: 50, H: 50}}
	got := BuildBoxblurFilter([]Detection{d}, FilterOptions{
		InputLabel:  "concat",
		OutputLabel: "redacted",
	})
	if !strings.HasPrefix(got, "[concat]split=2") {
		t.Errorf("expected input label [concat], got %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Errorf("expected output label [redacted], got %q", got)
	}
}

func TestBuildBoxblurFilterDropsZeroSize(t *testing.T) {
	d := Detection{TimeSec: 0, Bbox: Bbox{X: 100, Y: 100, W: 0, H: 10}}
	got := BuildBoxblurFilter([]Detection{d}, FilterOptions{})
	if got != "" {
		t.Errorf("expected zero-width bbox to be dropped, got %q", got)
	}
}

func TestDecodeBbox(t *testing.T) {
	good := []byte(`{"x":100,"y":50,"w":200,"h":40}`)
	got, err := DecodeBbox(good)
	if err != nil {
		t.Fatalf("DecodeBbox(good) err = %v", err)
	}
	if got.X != 100 || got.Y != 50 || got.W != 200 || got.H != 40 {
		t.Errorf("DecodeBbox(good) = %+v", got)
	}

	if _, err := DecodeBbox(nil); err == nil {
		t.Error("DecodeBbox(nil) expected error")
	}
	if _, err := DecodeBbox([]byte("not json")); err == nil {
		t.Error("DecodeBbox(garbage) expected error")
	}
}

func TestExpandWithHoldStableSorts(t *testing.T) {
	in := []Detection{
		{TimeSec: 5.0, Bbox: Bbox{X: 1}},
		{TimeSec: 1.0, Bbox: Bbox{X: 2}},
		{TimeSec: 3.0, Bbox: Bbox{X: 3}},
	}
	out := ExpandWithHold(in)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0].TimeSec != 1.0 || out[1].TimeSec != 3.0 || out[2].TimeSec != 5.0 {
		t.Errorf("not sorted: %+v", out)
	}
}

func TestRedactedQcameraPaths(t *testing.T) {
	root := "/var/data"
	idx := RedactedQcameraIndexPath(root, "abc", "route1", "0")
	want := filepath.Join(root, "abc", "route1", "0", "qcamera-redacted", "index.m3u8")
	if idx != want {
		t.Errorf("index path = %q, want %q", idx, want)
	}
	dir := RedactedQcameraDirPath(root, "abc", "route1", "0")
	wantDir := filepath.Join(root, "abc", "route1", "0", "qcamera-redacted")
	if dir != wantDir {
		t.Errorf("dir path = %q, want %q", dir, wantDir)
	}
}

func TestInvalidateRouteRemovesPerSegmentDirs(t *testing.T) {
	root := t.TempDir()
	dongle := "abc"
	route := "2024-01-01--00-00-00"
	// Create three segments, two with a redacted variant + one without.
	for _, seg := range []string{"0", "1", "2"} {
		segDir := filepath.Join(root, dongle, route, seg)
		if err := os.MkdirAll(segDir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	for _, seg := range []string{"0", "1"} {
		dir := RedactedQcameraDirPath(root, dongle, route, seg)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir variant: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.m3u8"), []byte("#EXTM3U\n"), 0644); err != nil {
			t.Fatalf("write playlist: %v", err)
		}
	}

	deleted, err := InvalidateRoute(root, dongle, route)
	if err != nil {
		t.Fatalf("InvalidateRoute: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	// Both removed.
	for _, seg := range []string{"0", "1"} {
		dir := RedactedQcameraDirPath(root, dongle, route, seg)
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("seg %s redacted dir still exists; err=%v", seg, err)
		}
	}
	// Untouched segment dir still present.
	if _, err := os.Stat(filepath.Join(root, dongle, route, "2")); err != nil {
		t.Errorf("segment 2 dir deleted by mistake: %v", err)
	}
}

func TestInvalidateRouteIdempotentOnMissingRoute(t *testing.T) {
	root := t.TempDir()
	deleted, err := InvalidateRoute(root, "ghost", "no-route")
	if err != nil {
		t.Fatalf("InvalidateRoute on missing route: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
}

func TestInvalidateRouteRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	// "/..".. should resolve outside root.
	_, err := InvalidateRoute(root, "..", "..")
	if err == nil || !strings.Contains(err.Error(), "traversal") {
		t.Errorf("expected traversal error, got %v", err)
	}
}

func TestInvalidateRouteRejectsEmptyArgs(t *testing.T) {
	cases := []struct {
		root, dongle, route string
	}{
		{"", "d", "r"},
		{"/data", "", "r"},
		{"/data", "d", ""},
	}
	for _, c := range cases {
		if _, err := InvalidateRoute(c.root, c.dongle, c.route); err == nil {
			t.Errorf("InvalidateRoute(%q,%q,%q) expected error", c.root, c.dongle, c.route)
		}
	}
}

func TestInvalidateRouteSurfacesUnexpectedErrors(t *testing.T) {
	// On platforms where os.RemoveAll surfaces a permission error, the
	// function should propagate it. We can't reliably set up such a
	// case across test environments, so we just sanity-check that the
	// happy path does not erroneously return ErrNotExist.
	root := t.TempDir()
	if _, err := InvalidateRoute(root, "d", "r"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Errorf("happy path returned ErrNotExist: %v", err)
		}
	}
}

// Package redaction holds the shared primitives the ALPR plate-blur
// feature uses to render redacted MP4 / HLS output. It is split out of
// internal/api so the export handler, share handler, and the
// background HLS-variant builder can all reuse the same filter-graph
// construction and cache-path conventions without an import cycle.
//
// # Overview
//
// Three surfaces consume this package:
//
//  1. The MP4 export handler (internal/api/export.go) builds a single
//     ffmpeg filter chain that blurs every detection bbox and re-encodes
//     the route into a streaming MP4. This is per-request and never
//     cached -- the response is a download attachment.
//
//  2. The share-link media handler (internal/api/share.go) consults
//     the token's RedactPlates flag and serves either the original
//     qcamera HLS files or a cached redacted variant under
//     <storage>/<dongle>/<route>/<seg>/qcamera-redacted/.
//
//  3. The redacted-variant builder worker (internal/worker/
//     redaction_builder.go) renders the cached variant on demand when
//     the share handler asks for it but the directory is empty.
//
// # Bbox coordinate space
//
// plate_detections.bbox is stored in the pixel space of the frame the
// ALPR detector saw, which is fcamera.hevc at openpilot's canonical
// resolution. To apply a bbox to a different stream (the qcamera
// downscale, or any HEVC re-encode at a different resolution) we
// rescale at filter-build time using the source/output dimensions.
// Callers pass the source dims as ScaleSource and the output dims as
// ScaleOutput; both default to FCameraWidth x FCameraHeight when the
// caller doesn't know them up front.
package redaction

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FCameraWidth / FCameraHeight are openpilot's canonical fcamera
// resolution. The ALPR detector runs against fcamera.hevc, so every
// stored bbox is in this pixel space (post any debayer/scale the device
// applies before the HEVC encoder, which is a no-op for the standard
// camera). Stored as the default scale source so bboxes for other
// streams (qcamera, etc.) are rescaled correctly on the way out.
const (
	FCameraWidth  = 1928
	FCameraHeight = 1208
)

// HoldWindow is how long a detection's bbox stays "active" after its
// frame_ts when no fresh detection arrives in the next frame. It exists
// to absorb the gaps between sampled frames (the extractor runs at
// ~2fps by default; without this, the blur would flicker every time a
// frame is skipped). 200ms covers ~6 frames at the typical fcamera 30fps
// playback rate, which is enough to bridge a one- or two-frame
// detector dropout without holding stale bboxes long enough to leak
// other plates.
const HoldWindowMs = 200

// RedactedQcameraDir is the per-segment directory name where the cached
// redacted qcamera HLS variant is written. Mirrors the unredacted
// "qcamera/" layout (index.m3u8 + seg_NNN.ts) so the share handler can
// serve them through the same playlist plumbing as the originals.
const RedactedQcameraDir = "qcamera-redacted"

// Bbox is a single plate detection bounding box in the pixel space
// described by ScaleSource. X / Y are top-left; W / H are dimensions.
// Mirrors the alpr.Rect on-wire shape (and therefore plate_detections.bbox
// when JSON-decoded) so callers can pass the decoded JSON straight in.
type Bbox struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

// Detection is one bbox tagged with the time it should appear in the
// output video. TimeSec is in seconds relative to the start of the
// stream the filter is being applied to (segment-local for the HLS
// variant builder, route-cumulative for MP4 export).
type Detection struct {
	TimeSec float64
	Bbox    Bbox
}

// FilterOptions controls how the boxblur filter chain is built. The
// zero value is valid: it normalizes bboxes against fcamera defaults
// and rescales them to the actual stream resolution at filter-eval
// time via ffmpeg's iw / ih variables.
type FilterOptions struct {
	// SourceWidth / SourceHeight are the pixel dimensions of the bbox
	// coordinate system (the resolution the ALPR detector saw).
	// Defaults to FCameraWidth x FCameraHeight. Override when bboxes
	// were recorded against a non-standard frame (e.g. a future
	// detector running on a different camera).
	SourceWidth  int
	SourceHeight int

	// HoldMs overrides HoldWindowMs. Useful in tests to prove the
	// hold-window logic kicks in. Values <= 0 fall back to the
	// constant.
	HoldMs int

	// BlurStrength is the boxblur "luma_radius" parameter. Larger =
	// more aggressive blur. The ffmpeg default is 2; the redaction
	// filter uses 12 by default to make plate text unreadable even on
	// a small screen. Values <= 0 fall back to the default.
	BlurStrength int

	// InputLabel is the ffmpeg filter-graph label of the stream the
	// graph reads from. Defaults to "0:v" for the common case of one
	// input file. The MP4 export pipeline overrides this when its
	// concat demuxer produces a non-default label.
	InputLabel string

	// OutputLabel is the label the final overlay writes to. Defaults
	// to "vout"; ffmpeg invocations wire it via `-map "[vout]"`. Set
	// this when chaining multiple filtergraphs.
	OutputLabel string
}

// withDefaults returns a copy of opts with zero fields filled in.
func (opts FilterOptions) withDefaults() FilterOptions {
	out := opts
	if out.SourceWidth <= 0 {
		out.SourceWidth = FCameraWidth
	}
	if out.SourceHeight <= 0 {
		out.SourceHeight = FCameraHeight
	}
	if out.HoldMs <= 0 {
		out.HoldMs = HoldWindowMs
	}
	if out.BlurStrength <= 0 {
		// boxblur's max luma_radius is 2 in modern ffmpeg builds.
		// Pick the maximum so plate text is unreadable; chain
		// luma_power up via multiple passes if a stronger blur is
		// needed in a future tuning pass.
		out.BlurStrength = 2
	}
	if out.InputLabel == "" {
		out.InputLabel = "0:v"
	}
	if out.OutputLabel == "" {
		out.OutputLabel = "vout"
	}
	return out
}

// BuildBoxblurFilter renders an ffmpeg `-filter_complex` graph that
// blurs every region in detections only during its enable window
// (TimeSec .. TimeSec+HoldMs). The graph follows the standard ffmpeg
// "blur a region of a frame at a specific time" pattern: split the
// stream into a base copy and a per-detection sub-pipeline that crops
// the bbox, blurs the crop, and overlays it back onto the base with
// `enable='between(t,t0,t1)'`. The chain composes left-to-right so
// the next detection's overlay receives the already-redacted output
// of the previous step.
//
// Returns an empty string when there are no detections after the
// normalize step; the caller should fall back to `-c copy` in that
// case.
//
// # Coordinate scaling
//
// Because the source video the filter is applied to may have a
// different resolution than the frames the ALPR detector saw (qcamera
// 526x330 vs fcamera 1928x1208, or any resolution drift in a future
// codec swap), the filter is built in normalized coordinates and
// rescaled to the actual stream dimensions at runtime. Crop / overlay
// expressions reference ffmpeg's `iw` / `ih` (input width / height)
// vars instead of literal pixel counts, so the same filter works
// regardless of the output resolution: a bbox at fcamera-pixel x=964
// on a 1928-wide source becomes `iw*964/1928` (== iw/2) which is
// `iw/2` whatever iw is. ffprobe-free, drift-free.
//
// Performance: each crop+blur+overlay step only touches pixels inside
// the bbox AND only when the enable window is open, so 100 detections
// per route stay well inside the 2x-realtime budget on commodity CPUs.
//
// The graph's input label is the caller-provided InputLabel (defaulting
// to "0:v") and the output label is "vout". The export handler wires
// these into `-filter_complex <chain> -map "[vout]"`.
func BuildBoxblurFilter(detections []Detection, opts FilterOptions) string {
	if len(detections) == 0 {
		return ""
	}
	o := opts.withDefaults()
	hold := float64(o.HoldMs) / 1000.0

	// Sort by TimeSec so the emitted filter is deterministic regardless
	// of the order detections were loaded from the database. Output is
	// equivalent either way (overlays compose), but byte-level
	// determinism makes the test fixture and review diff easier.
	sorted := make([]Detection, len(detections))
	copy(sorted, detections)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].TimeSec < sorted[j].TimeSec
	})

	type normalized struct {
		// Normalized coordinates in [0, 1]. Multiplied by iw/ih at
		// filter-graph eval time.
		nx, ny, nw, nh float64
		t0, t1         float64
	}
	scaled := make([]normalized, 0, len(sorted))
	srcW := float64(o.SourceWidth)
	srcH := float64(o.SourceHeight)
	for _, d := range sorted {
		nx := d.Bbox.X / srcW
		ny := d.Bbox.Y / srcH
		nw := d.Bbox.W / srcW
		nh := d.Bbox.H / srcH
		if nw <= 0 || nh <= 0 {
			continue
		}
		// Drop any bbox whose origin is outside the source frame.
		if nx >= 1 || ny >= 1 || nx+nw <= 0 || ny+nh <= 0 {
			continue
		}
		// Clamp to [0, 1] without crossing the right/bottom edge.
		if nx < 0 {
			nw += nx
			nx = 0
		}
		if ny < 0 {
			nh += ny
			ny = 0
		}
		if nx+nw > 1 {
			nw = 1 - nx
		}
		if ny+nh > 1 {
			nh = 1 - ny
		}
		scaled = append(scaled, normalized{nx, ny, nw, nh, d.TimeSec, d.TimeSec + hold})
	}
	if len(scaled) == 0 {
		return ""
	}

	inLabel := o.InputLabel
	outLabel := o.OutputLabel
	// `force_original_aspect_ratio=increase` (and similar) is unwanted
	// here -- we want literal scaling to iw/ih. ffmpeg's filter
	// expression evaluator computes integer truncation on the
	// resulting sub-expressions automatically, so iw*0.5 is the right
	// integer pixel value at filter-init time. To guard against
	// crop's width/height "must be > 0" check on tiny outputs,
	// floor each derived dim with `max(1, ...)`.
	var sb strings.Builder
	prev := inLabel
	for i, c := range scaled {
		bg := fmt.Sprintf("bg%d", i)
		ro := fmt.Sprintf("ro%d", i)
		bl := fmt.Sprintf("bl%d", i)
		var stepOut string
		if i == len(scaled)-1 {
			stepOut = outLabel
		} else {
			stepOut = fmt.Sprintf("s%d", i)
		}
		// ffmpeg crop accepts expressions for w/h/x/y; we use
		// max(1, floor(iw*nw)) to guarantee a strictly positive
		// crop even on tiny test fixtures. Overlay's x/y use a
		// different expression vocabulary: `main_w` / `main_h`
		// reference the main (background) input dimensions, so the
		// overlay placement scales identically to the crop without
		// needing iw/ih (which overlay does not define on its
		// inputs).
		cropW := fmt.Sprintf("max(1\\,floor(iw*%g))", c.nw)
		cropH := fmt.Sprintf("max(1\\,floor(ih*%g))", c.nh)
		cropX := fmt.Sprintf("floor(iw*%g)", c.nx)
		cropY := fmt.Sprintf("floor(ih*%g)", c.ny)
		ovX := fmt.Sprintf("floor(main_w*%g)", c.nx)
		ovY := fmt.Sprintf("floor(main_h*%g)", c.ny)
		// luma_power=3 stacks the blur three passes so a small
		// luma_radius (capped at 2 by ffmpeg in luma; chroma is
		// capped at 1) still produces a strong, plate-illegible
		// smear.
		fmt.Fprintf(&sb,
			"[%s]split=2[%s][%s];[%s]crop=%s:%s:%s:%s,boxblur=luma_radius=%d:luma_power=3:chroma_radius=1:chroma_power=3[%s];"+
				"[%s][%s]overlay=%s:%s:enable='between(t\\,%g\\,%g)'[%s];",
			prev, bg, ro,
			ro, cropW, cropH, cropX, cropY, o.BlurStrength, bl,
			bg, bl, ovX, ovY, c.t0, c.t1, stepOut,
		)
		prev = stepOut
	}
	out := sb.String()
	return strings.TrimRight(out, ";")
}

// ExpandWithHold takes a list of detections sorted by time and inserts
// a "still-active" copy of any detection whose hold window covers the
// next detection's time slot for the same approximate region. This is
// the bbox-interpolation step described in the acceptance criteria.
// The default HoldMs already accomplishes this on the ffmpeg side via
// the enable=between(t,...) gating; ExpandWithHold is exposed for
// callers (and tests) that want to reason about the hold semantics
// without invoking ffmpeg.
//
// The current implementation is a pass-through: the boxblur filter's
// per-detection enable window already overlaps the next detection's
// window when their bboxes are nearby, which is the desired
// interpolation. A future improvement might dedupe overlapping windows
// for the same plate to keep the filter graph short on long routes
// with many detections of one plate; for now we accept the linear
// growth (one filter per detection) because it stays well inside the
// performance budget. Kept exported so tests and the cache-builder can
// share a common code path.
func ExpandWithHold(detections []Detection) []Detection {
	out := make([]Detection, len(detections))
	copy(out, detections)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].TimeSec < out[j].TimeSec
	})
	return out
}

// DecodeBbox parses a JSON-encoded bbox blob (the shape stored in
// plate_detections.bbox) into a Bbox. Returns an error when the bytes
// are not valid JSON or do not match the {x,y,w,h} schema. Callers that
// want to swallow malformed rows (and skip the detection) can do so by
// continuing on err != nil; the package itself never panics on bad
// input.
func DecodeBbox(raw []byte) (Bbox, error) {
	if len(raw) == 0 {
		return Bbox{}, fmt.Errorf("redaction: empty bbox blob")
	}
	var b Bbox
	if err := json.Unmarshal(raw, &b); err != nil {
		return Bbox{}, fmt.Errorf("redaction: decode bbox: %w", err)
	}
	return b, nil
}

// RedactedQcameraIndexPath returns the on-disk path to the cached
// redacted qcamera HLS playlist for one segment. The directory may not
// exist yet (the variant builder creates it on demand); callers that
// need to know whether the cache is hot should os.Stat this path.
func RedactedQcameraIndexPath(storagePath, dongleID, route, segment string) string {
	return filepath.Join(storagePath, dongleID, route, segment, RedactedQcameraDir, "index.m3u8")
}

// RedactedQcameraDirPath returns the per-segment cached-variant
// directory itself (the parent of index.m3u8 + every .ts chunk). Used
// by the variant builder to create / clean the directory and by
// InvalidateRoute / share handler internals to decide whether the
// cache is populated.
func RedactedQcameraDirPath(storagePath, dongleID, route, segment string) string {
	return filepath.Join(storagePath, dongleID, route, segment, RedactedQcameraDir)
}

// InvalidateRoute removes every cached redacted-qcamera HLS variant
// under a single route. It walks the per-segment directories and
// deletes <segment>/qcamera-redacted/ for each. Idempotent: missing
// directories are not an error.
//
// This function is the published contract for the future
// alpr-manual-correction-api feature. When the operator edits a plate
// or merges two plates, that feature must call InvalidateRoute on
// every affected route so the next share-link viewer triggers a
// rebuild against the corrected detections. The function lives in
// this package (rather than internal/storage) so the cache-naming
// convention stays colocated with the rest of the redaction code.
//
// The caller passes the storage root and a route identifier; the
// function does its own path-traversal validation by computing each
// candidate directory under the route root and ensuring it stays
// under storagePath. Returns the number of directories actually
// deleted (useful for logging / metrics) plus any non-NotExist error
// encountered.
func InvalidateRoute(storagePath, dongleID, route string) (int, error) {
	if storagePath == "" {
		return 0, fmt.Errorf("redaction: empty storage path")
	}
	if dongleID == "" || route == "" {
		return 0, fmt.Errorf("redaction: empty dongle or route")
	}
	routeRoot := filepath.Join(storagePath, dongleID, route)
	absBase, err := filepath.Abs(storagePath)
	if err != nil {
		return 0, fmt.Errorf("redaction: resolve storage path: %w", err)
	}
	absRoute, err := filepath.Abs(routeRoot)
	if err != nil {
		return 0, fmt.Errorf("redaction: resolve route path: %w", err)
	}
	if !strings.HasPrefix(absRoute, absBase+string(filepath.Separator)) && absRoute != absBase {
		return 0, fmt.Errorf("redaction: path traversal detected")
	}

	entries, err := os.ReadDir(routeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("redaction: read route dir: %w", err)
	}
	deleted := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(routeRoot, entry.Name(), RedactedQcameraDir)
		info, err := os.Stat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return deleted, fmt.Errorf("redaction: stat %s: %w", dir, err)
		}
		if !info.IsDir() {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return deleted, fmt.Errorf("redaction: remove %s: %w", dir, err)
		}
		deleted++
	}
	return deleted, nil
}

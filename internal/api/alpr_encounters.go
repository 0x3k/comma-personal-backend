package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// ALPREncountersHandler serves the read endpoints the dashboard uses to
// surface plate sightings:
//
//   - GET /v1/routes/:dongle_id/:route_name/plates -- the per-route timeline
//     overlay (which plates we saw on this drive).
//   - GET /v1/plates/:hash_b64 -- cross-route history for a single plate
//     hash, used by the plate-detail page.
//
// Plate text is decrypted only inside the handler, only after auth
// (sessionOrJWT or session-only) has succeeded. Decrypted plate text is
// never logged or labelled; logs use plate_hash_b64 instead.
type ALPREncountersHandler struct {
	queries  alprEncountersQuerier
	keyring  plateKeyring
	envelope alprEnvelope
}

// alprEncountersQuerier names the subset of *db.Queries this handler
// needs. Extracted as an interface so tests can pass a small fake
// without standing up Postgres.
type alprEncountersQuerier interface {
	GetRoute(ctx context.Context, arg db.GetRouteParams) (db.Route, error)
	ListEncountersForRoute(ctx context.Context, arg db.ListEncountersForRouteParams) ([]db.PlateEncounter, error)
	ListEncountersForPlate(ctx context.Context, plateHash []byte) ([]db.PlateEncounter, error)
	ListDetectionsForRoute(ctx context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error)
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
	GetSignature(ctx context.Context, id int64) (db.VehicleSignature, error)
	GetTripByRouteID(ctx context.Context, routeID int32) (db.Trip, error)
}

// plateKeyring is the slice of *alprcrypto.Keyring methods the encounters
// handler needs. Defining an interface here lets the test suite swap in a
// stub keyring without depending on the crypto package's HKDF setup.
type plateKeyring interface {
	Decrypt(ciphertext []byte) (string, error)
	DecryptLabel(ciphertext []byte) (string, error)
}

// alprEnvelope is the small surface of the settings store + crypto
// preconditions the handler needs to gate requests at runtime. Keeping
// it as an interface (rather than passing settings.Store directly) lets
// tests force "alpr_disabled" without building a real settings.Store.
type alprEnvelope interface {
	Enabled(ctx context.Context) (bool, error)
	KeyringConfigured() bool
}

// settingsEnvelope is the production implementation of alprEnvelope: it
// reads alpr_enabled out of the settings table and reports whether the
// keyring was loaded at startup.
type settingsEnvelope struct {
	store   *settings.Store
	hasKeys bool
}

// Enabled returns the runtime alpr_enabled flag. Treats a missing row
// (the default) as off so brand-new deployments do not silently expose
// plate data through these endpoints.
func (e settingsEnvelope) Enabled(ctx context.Context) (bool, error) {
	if e.store == nil {
		return false, nil
	}
	return e.store.BoolOr(ctx, settings.KeyALPREnabled, false)
}

// KeyringConfigured reports whether ALPR_ENCRYPTION_KEY was set and
// successfully loaded at process startup. The detection worker idles
// without it, so encounters cannot grow under that condition; we still
// guard the read path so a stale dataset from a prior keyring is not
// served when the operator re-enabled alpr without restoring the key.
func (e settingsEnvelope) KeyringConfigured() bool { return e.hasKeys }

// NewALPREncountersHandler wires the dependencies. keyring may be nil --
// the handler short-circuits with 503 in that case (the read endpoints
// require decryption to populate `plate`).
//
// Callers passing a typed-nil keyring (e.g. an *alprcrypto.Keyring that
// was never loaded because ALPR_ENCRYPTION_KEY was unset at startup)
// hit a typed-nil-vs-nil-interface trap if we just stored the value;
// we sniff that case explicitly so the requireAlprEnabled gate
// correctly reports "no keyring" via KeyringConfigured.
func NewALPREncountersHandler(queries alprEncountersQuerier, store *settings.Store, keyring plateKeyring) *ALPREncountersHandler {
	hasKeys := keyring != nil
	if hasKeys {
		// Defend against the typed-nil-pointer case: a *Keyring(nil)
		// stuffed into a plateKeyring interface compares != nil but
		// would panic on first method call. reflect.Value.IsNil
		// catches both pointer and channel/map/etc nil-ness.
		v := reflect.ValueOf(keyring)
		switch v.Kind() {
		case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Slice, reflect.Func, reflect.Interface:
			if v.IsNil() {
				hasKeys = false
			}
		}
	}
	if !hasKeys {
		keyring = nil
	}
	h := &ALPREncountersHandler{
		queries: queries,
		keyring: keyring,
	}
	h.envelope = settingsEnvelope{store: store, hasKeys: hasKeys}
	return h
}

// RequireEnabled returns Echo middleware that gates this handler's
// endpoints on the runtime alpr_enabled flag. Exposed publicly so the
// route wiring can apply the gate without re-implementing it.
func (h *ALPREncountersHandler) RequireEnabled() echo.MiddlewareFunc {
	return requireAlprEnabled(h.envelope)
}

// alprDisabledResponse is the JSON envelope returned by the
// requireAlprEnabled middleware (and the keyring-missing fallback) for
// 503 responses. The frontend renders a "feature disabled" banner on
// this shape; the dedicated `error` string lets the UI distinguish it
// from generic 503s without scraping the detail field.
type alprDisabledResponse struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
}

// requireAlprEnabled returns Echo middleware that short-circuits with a
// 503 response when the runtime alpr_enabled flag is off. Each request
// reads the flag fresh so toggling it via PUT /v1/settings/alpr takes
// effect on the next call without a restart.
//
// The middleware also returns 503 when alpr_enabled is on but the
// process keyring is nil -- this is the "operator turned ALPR back on
// without ALPR_ENCRYPTION_KEY in the env" case, and decrypting plate
// text would be impossible. We return the same shape so the frontend
// can render the same disabled banner; the detail string is identical
// because from the user's perspective they need to fix the same thing
// (the Settings page).
func requireAlprEnabled(env alprEnvelope) echo.MiddlewareFunc {
	disabled := alprDisabledResponse{
		Error:  "alpr_disabled",
		Detail: "Enable ALPR in Settings to use this endpoint.",
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if env == nil {
				return c.JSON(http.StatusServiceUnavailable, disabled)
			}
			enabled, err := env.Enabled(c.Request().Context())
			if err != nil {
				return c.JSON(http.StatusInternalServerError, errorResponse{
					Error: "failed to read alpr_enabled flag",
					Code:  http.StatusInternalServerError,
				})
			}
			if !enabled || !env.KeyringConfigured() {
				return c.JSON(http.StatusServiceUnavailable, disabled)
			}
			return next(c)
		}
	}
}

// signatureResponse is the JSON shape for a vehicle_signatures row when
// embedded in encounter responses. Confidence is a pointer so a missing
// confidence (the column is nullable) serialises as null rather than a
// misleading 0.
type signatureResponse struct {
	Make       string   `json:"make,omitempty"`
	Model      string   `json:"model,omitempty"`
	Color      string   `json:"color,omitempty"`
	BodyType   string   `json:"body_type,omitempty"`
	Confidence *float32 `json:"confidence,omitempty"`
}

// bboxResponse is the {x,y,w,h} shape stored in plate_detections.bbox
// and plate_encounters.bbox_first/bbox_last. We round-trip it through a
// typed struct so the wire format is stable independent of how the
// detection worker chose to marshal its bbox JSON.
type bboxResponse struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// routeEncounterResponse is one element of the {encounters: [...]}
// payload returned by GET /v1/routes/:dongle_id/:route_name/plates.
//
// `Plate` is the decrypted plate text; the requester is authenticated
// and viewing their own data. `PlateHashB64` is included so the
// frontend can link to the plate-detail page without re-encoding the
// raw hash bytes. Fields that may be missing (signature, alert state,
// thumbnail) are pointers so they serialize as null rather than zero
// values.
type routeEncounterResponse struct {
	Plate             string             `json:"plate"`
	PlateHashB64      string             `json:"plate_hash_b64"`
	FirstSeenTs       string             `json:"first_seen_ts"`
	LastSeenTs        string             `json:"last_seen_ts"`
	DetectionCount    int32              `json:"detection_count"`
	TurnCount         int32              `json:"turn_count"`
	Signature         *signatureResponse `json:"signature"`
	SeverityIfAlerted *int16             `json:"severity_if_alerted"`
	AckStatus         *string            `json:"ack_status"`
	BboxFirst         *bboxResponse      `json:"bbox_first"`
	SampleThumbURL    *string            `json:"sample_thumb_url"`
}

type routeEncountersResponse struct {
	Encounters []routeEncounterResponse `json:"encounters"`
}

// GetRouteEncounters handles GET /v1/routes/:dongle_id/:route_name/plates.
// Auth: sessionOrJWT, plus checkDongleAccess (a JWT-only caller may only
// query its own dongle). 404 when the route does not exist.
func (h *ALPREncountersHandler) GetRouteEncounters(c echo.Context) error {
	dongleID := c.Param("dongle_id")
	routeName := c.Param("route_name")

	if handled, err := checkDongleAccess(c, dongleID); handled {
		return err
	}

	ctx := c.Request().Context()

	if _, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  dongleID,
		RouteName: routeName,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "route not found",
				Code:  http.StatusNotFound,
			})
		}
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to load route",
			Code:  http.StatusInternalServerError,
		})
	}

	encounters, err := h.queries.ListEncountersForRoute(ctx, db.ListEncountersForRouteParams{
		DongleID: dongleID,
		Route:    routeName,
	})
	if err != nil {
		c.Logger().Errorf("alpr: list encounters for route %s/%s failed: %v", dongleID, routeName, err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list encounters",
			Code:  http.StatusInternalServerError,
		})
	}

	// Build a sample-detection lookup so we can populate sample_thumb_url
	// without one DB round-trip per encounter. The route's detections are
	// chronological; for each plate_hash we keep the first detection that
	// has a thumb_path so the link points at a real file. Detections
	// without thumb_path are silently skipped.
	detRows, err := h.queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
		DongleID: dongleID,
		Route:    routeName,
	})
	if err != nil {
		c.Logger().Errorf("alpr: list detections for route %s/%s failed: %v", dongleID, routeName, err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list detections",
			Code:  http.StatusInternalServerError,
		})
	}
	sampleByHash := buildSampleDetectionsByHash(detRows)

	out := make([]routeEncounterResponse, 0, len(encounters))
	for _, e := range encounters {
		entry, err := h.encounterToRouteResponse(ctx, e, sampleByHash)
		if err != nil {
			// Per-encounter failures (e.g. a single decryption failure
			// from a corrupted ciphertext, or a missing signature row)
			// are logged with the hash and skipped so one bad row does
			// not 500 the whole timeline. Logged with hash, never plate.
			c.Logger().Warnf("alpr: skipping encounter id=%d hash=%s: %v",
				e.ID, hashB64(e.PlateHash), err)
			continue
		}
		out = append(out, entry)
	}

	return c.JSON(http.StatusOK, routeEncountersResponse{Encounters: out})
}

// encounterToRouteResponse converts a single plate_encounters row into
// the wire shape, looking up signature + watchlist state alongside.
func (h *ALPREncountersHandler) encounterToRouteResponse(
	ctx context.Context,
	e db.PlateEncounter,
	sampleByHash map[string]db.ListDetectionsForRouteRow,
) (routeEncounterResponse, error) {
	// Find a detection ciphertext we can decrypt for the plate text.
	// Encounters do not carry plate_ciphertext on their own (the
	// schema only stores plate_hash on encounter rows), so we go
	// through the matching detection in our pre-built map.
	plateText := ""
	sample, hasSample := sampleByHash[string(e.PlateHash)]
	if hasSample && len(sample.PlateCiphertext) > 0 {
		decrypted, err := h.keyring.Decrypt(sample.PlateCiphertext)
		if err != nil {
			return routeEncounterResponse{}, fmt.Errorf("decrypt plate: %w", err)
		}
		plateText = decrypted
	}

	resp := routeEncounterResponse{
		Plate:          plateText,
		PlateHashB64:   hashB64(e.PlateHash),
		FirstSeenTs:    formatTs(e.FirstSeenTs),
		LastSeenTs:     formatTs(e.LastSeenTs),
		DetectionCount: e.DetectionCount,
		TurnCount:      e.TurnCount,
		BboxFirst:      decodeBbox(e.BboxFirst),
	}

	if e.SignatureID.Valid {
		sig, err := h.queries.GetSignature(ctx, e.SignatureID.Int64)
		if err == nil {
			resp.Signature = signatureToResponse(sig)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return routeEncounterResponse{}, fmt.Errorf("get signature: %w", err)
		}
	}

	// Watchlist state -- severity / ack status -- is per-plate, so we
	// look it up via plate_hash. Most encounters have no watchlist row
	// (the common case is "we saw a plate, nobody flagged it") and we
	// just leave the pointer fields nil.
	wl, err := h.queries.GetWatchlistByHash(ctx, e.PlateHash)
	if err == nil && wl.Kind == "alerted" {
		if wl.Severity.Valid {
			sev := wl.Severity.Int16
			resp.SeverityIfAlerted = &sev
		}
		ack := "open"
		if wl.AckedAt.Valid {
			ack = "acked"
		}
		resp.AckStatus = &ack
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return routeEncounterResponse{}, fmt.Errorf("watchlist lookup: %w", err)
	}

	if hasSample && sample.ThumbPath.Valid && sample.ThumbPath.String != "" {
		// The actual /v1/alpr/detections/:id/thumbnail handler is not
		// implemented yet; we still emit the URL so the frontend can
		// pre-wire its image element and the operator can preview it
		// once the handler lands. Until then it 404s, which is a soft
		// failure on the UI side (broken-image icon).
		url := fmt.Sprintf("/v1/alpr/detections/%d/thumbnail", sample.ID)
		resp.SampleThumbURL = &url
	}

	return resp, nil
}

// plateDetailEncounter is one element of the encounters[] slice in the
// per-plate response. Adds dongle_id / route / area_cluster_label
// relative to routeEncounterResponse, and drops plate / plate_hash_b64
// (those live on the parent envelope).
type plateDetailEncounter struct {
	DongleID         string             `json:"dongle_id"`
	Route            string             `json:"route"`
	FirstSeenTs      string             `json:"first_seen_ts"`
	LastSeenTs       string             `json:"last_seen_ts"`
	DetectionCount   int32              `json:"detection_count"`
	TurnCount        int32              `json:"turn_count"`
	Signature        *signatureResponse `json:"signature"`
	AreaClusterLabel string             `json:"area_cluster_label"`
}

// plateWatchlistStatus mirrors the subset of plate_watchlist that the
// plate-detail page surfaces. AckedAt is a pointer so a never-acked
// alert serializes as null.
type plateWatchlistStatus struct {
	Kind     string  `json:"kind"`
	Severity *int16  `json:"severity"`
	Label    string  `json:"label,omitempty"`
	AckedAt  *string `json:"acked_at"`
}

// plateDetailStats is the cross-route aggregate the plate-detail page
// uses for its header summary. Computed in-memory from the encounter
// rows so the schema does not need additional columns or a heavier
// query.
type plateDetailStats struct {
	DistinctRoutes30d int    `json:"distinct_routes_30d"`
	DistinctAreas30d  int    `json:"distinct_areas_30d"`
	TotalDetections   int32  `json:"total_detections"`
	FirstEverSeen     string `json:"first_ever_seen,omitempty"`
	LastEverSeen      string `json:"last_ever_seen,omitempty"`
}

type plateDetailResponse struct {
	Plate           string                 `json:"plate"`
	PlateHashB64    string                 `json:"plate_hash_b64"`
	WatchlistStatus *plateWatchlistStatus  `json:"watchlist_status"`
	Signature       *signatureResponse     `json:"signature"`
	Encounters      []plateDetailEncounter `json:"encounters"`
	Stats           plateDetailStats       `json:"stats"`
}

// platesDefaultLimit / platesMaxLimit bound the encounters[] slice
// returned by GET /v1/plates/:hash_b64. Mirrors the events handler so
// pagination behaviour is consistent across the dashboard.
const (
	platesDefaultLimit = 50
	platesMaxLimit     = 200
)

// GetPlateDetail handles GET /v1/plates/:hash_b64. Session-only -- a
// device-JWT caller has no business querying cross-route plate history.
//
// Pagination: ?limit= (default 50, capped at platesMaxLimit) and
// ?offset= (default 0). The encounters[] slice is paginated; the stats
// envelope is computed across the FULL encounter set so the operator
// always sees the same totals regardless of page.
func (h *ALPREncountersHandler) GetPlateDetail(c echo.Context) error {
	hashB64Param := c.Param("hash_b64")
	plateHash, err := decodePlateHashB64(hashB64Param)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "malformed plate hash (expected base64-url, no padding)",
			Code:  http.StatusBadRequest,
		})
	}

	limit, offset, err := parseLimitOffset(c, platesDefaultLimit, platesMaxLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	all, err := h.queries.ListEncountersForPlate(ctx, plateHash)
	if err != nil {
		c.Logger().Errorf("alpr: list encounters for plate hash=%s failed: %v", hashB64Param, err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list encounters",
			Code:  http.StatusInternalServerError,
		})
	}
	if len(all) == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "no encounters found for plate hash",
			Code:  http.StatusNotFound,
		})
	}

	resp := plateDetailResponse{
		PlateHashB64: hashB64Param,
		Stats:        computePlateStats(all),
	}

	// Decrypt the plate text from the most-recent encounter we can find
	// a sample detection for. Falls back to the next-most-recent if a
	// decryption fails (e.g. a corrupted ciphertext on one row), and
	// finally to "" if nothing decrypts.
	resp.Plate = h.decryptPlateFromEncounters(ctx, all)

	// Watchlist status is plate-wide (not per-encounter), so we look it
	// up once.
	wl, err := h.queries.GetWatchlistByHash(ctx, plateHash)
	if err == nil {
		resp.WatchlistStatus = h.watchlistRowToStatus(wl)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		c.Logger().Errorf("alpr: watchlist lookup for hash=%s failed: %v", hashB64Param, err)
		// Non-fatal: keep going without a watchlist field.
	}

	// Canonical signature: most-frequent signature_id across encounters,
	// resolved into a vehicle_signatures row. If no encounter has a
	// signature_id we leave the field nil.
	if sigID, ok := dominantSignatureID(all); ok {
		if sig, err := h.queries.GetSignature(ctx, sigID); err == nil {
			resp.Signature = signatureToResponse(sig)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			c.Logger().Errorf("alpr: get signature %d for hash=%s failed: %v", sigID, hashB64Param, err)
		}
	}

	// Apply pagination AFTER stats / signature / watchlist computation
	// so those summaries reflect the entire dataset.
	page := paginate(all, offset, limit)
	resp.Encounters = make([]plateDetailEncounter, 0, len(page))
	for _, e := range page {
		entry := plateDetailEncounter{
			DongleID:       e.DongleID,
			Route:          e.Route,
			FirstSeenTs:    formatTs(e.FirstSeenTs),
			LastSeenTs:     formatTs(e.LastSeenTs),
			DetectionCount: e.DetectionCount,
			TurnCount:      e.TurnCount,
		}

		if e.SignatureID.Valid {
			if sig, err := h.queries.GetSignature(ctx, e.SignatureID.Int64); err == nil {
				entry.Signature = signatureToResponse(sig)
			} else if !errors.Is(err, pgx.ErrNoRows) {
				c.Logger().Warnf("alpr: get signature %d for encounter %d failed: %v",
					e.SignatureID.Int64, e.ID, err)
			}
		}

		entry.AreaClusterLabel = h.areaClusterLabel(ctx, e)
		resp.Encounters = append(resp.Encounters, entry)
	}

	return c.JSON(http.StatusOK, resp)
}

// decryptPlateFromEncounters walks the encounter rows in order looking
// for a sample detection whose ciphertext decrypts to a non-empty
// string. Returns "" if no decryption succeeds; the empty value tells
// the frontend "we have a hash but the plain text is unavailable" and
// the page can degrade gracefully.
func (h *ALPREncountersHandler) decryptPlateFromEncounters(
	ctx context.Context,
	encounters []db.PlateEncounter,
) string {
	for _, e := range encounters {
		dets, err := h.queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
			DongleID: e.DongleID,
			Route:    e.Route,
		})
		if err != nil {
			continue
		}
		for _, d := range dets {
			if string(d.PlateHash) != string(e.PlateHash) {
				continue
			}
			if len(d.PlateCiphertext) == 0 {
				continue
			}
			if plaintext, err := h.keyring.Decrypt(d.PlateCiphertext); err == nil {
				return plaintext
			}
		}
	}
	return ""
}

// watchlistRowToStatus converts a plate_watchlist row into the
// plateWatchlistStatus wire shape, decrypting the label opportunistically.
// A label-decryption failure is logged (with hash, never plaintext) and
// produces an empty Label string -- the rest of the row is still useful
// to the operator.
func (h *ALPREncountersHandler) watchlistRowToStatus(w db.GetWatchlistByHashRow) *plateWatchlistStatus {
	out := &plateWatchlistStatus{Kind: w.Kind}
	if w.Severity.Valid {
		s := w.Severity.Int16
		out.Severity = &s
	}
	if w.AckedAt.Valid {
		t := w.AckedAt.Time.UTC().Format(time.RFC3339)
		out.AckedAt = &t
	}
	if len(w.LabelCiphertext) > 0 && h.keyring != nil {
		if label, err := h.keyring.DecryptLabel(w.LabelCiphertext); err == nil {
			out.Label = label
		}
	}
	return out
}

// areaClusterLabel returns the human-friendly representation of the
// route's general area. Tries the trip's reverse-geocoded start
// address first (set by the trip-aggregator-worker); falls back to
// "lat,lng" rounded to 2 decimal places, and finally to "" when
// there's no GPS at all on the route.
func (h *ALPREncountersHandler) areaClusterLabel(ctx context.Context, e db.PlateEncounter) string {
	route, err := h.queries.GetRoute(ctx, db.GetRouteParams{
		DongleID:  e.DongleID,
		RouteName: e.Route,
	})
	if err != nil {
		return ""
	}
	trip, err := h.queries.GetTripByRouteID(ctx, route.ID)
	if err == nil {
		if trip.StartAddress.Valid && trip.StartAddress.String != "" {
			return trip.StartAddress.String
		}
		if trip.StartLat.Valid && trip.StartLng.Valid {
			return fmt.Sprintf("%.2f,%.2f", trip.StartLat.Float64, trip.StartLng.Float64)
		}
	}
	return ""
}

// computePlateStats walks the full encounter set to compute the header
// summary the plate-detail page renders. Done in-memory because the
// numbers are small (a single plate's encounters across all routes is
// O(routes-it-was-seen-in), not O(detections)) and avoiding a custom
// SQL aggregate keeps the migrations footprint zero.
func computePlateStats(encounters []db.PlateEncounter) plateDetailStats {
	stats := plateDetailStats{}

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	routeKeys30d := map[string]struct{}{}
	areaKeys30d := map[string]struct{}{}
	var firstEver, lastEver time.Time

	for _, e := range encounters {
		stats.TotalDetections += e.DetectionCount

		if e.FirstSeenTs.Valid {
			if firstEver.IsZero() || e.FirstSeenTs.Time.Before(firstEver) {
				firstEver = e.FirstSeenTs.Time
			}
		}
		if e.LastSeenTs.Valid {
			if lastEver.IsZero() || e.LastSeenTs.Time.After(lastEver) {
				lastEver = e.LastSeenTs.Time
			}
		}

		// Encounters whose last_seen_ts is inside the 30d window count
		// toward the recurring-routes / recurring-areas tally. Using
		// last_seen_ts (rather than first_seen_ts) catches a plate
		// that was first seen 60 days ago but is still around.
		if e.LastSeenTs.Valid && !e.LastSeenTs.Time.Before(cutoff) {
			routeKeys30d[e.DongleID+"|"+e.Route] = struct{}{}
			// Coarse area key: the (dongle_id, route) pair stands in
			// for "the area we drove that route in". A future
			// improvement is to bucket by 5km lat/lng cell using the
			// trip rows; for now this gives the UI a stable count
			// without requiring extra queries.
			areaKeys30d[e.DongleID+"|"+e.Route] = struct{}{}
		}
	}

	stats.DistinctRoutes30d = len(routeKeys30d)
	stats.DistinctAreas30d = len(areaKeys30d)
	if !firstEver.IsZero() {
		stats.FirstEverSeen = firstEver.UTC().Format(time.RFC3339)
	}
	if !lastEver.IsZero() {
		stats.LastEverSeen = lastEver.UTC().Format(time.RFC3339)
	}
	return stats
}

// dominantSignatureID returns the signature_id that appears on the
// most encounters, ties broken by which appears first. Returns
// (id, true) when at least one encounter has a non-null signature_id;
// (0, false) otherwise.
func dominantSignatureID(encounters []db.PlateEncounter) (int64, bool) {
	counts := map[int64]int{}
	order := []int64{}
	for _, e := range encounters {
		if !e.SignatureID.Valid {
			continue
		}
		id := e.SignatureID.Int64
		if _, seen := counts[id]; !seen {
			order = append(order, id)
		}
		counts[id]++
	}
	if len(order) == 0 {
		return 0, false
	}
	best := order[0]
	for _, id := range order[1:] {
		if counts[id] > counts[best] {
			best = id
		}
	}
	return best, true
}

// paginate slices a result set safely. offset >= len returns an empty
// slice; offset+limit > len truncates to the end.
func paginate[T any](rows []T, offset, limit int) []T {
	if offset >= len(rows) {
		return nil
	}
	end := offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

// parseLimitOffset reads the `limit` and `offset` query params with
// validation. Empty strings fall back to the defaults; non-integers,
// negatives, and over-cap values are rejected with a 400.
func parseLimitOffset(c echo.Context, defaultLimit, maxLimit int) (limit, offset int, err error) {
	limit = defaultLimit
	if raw := c.QueryParam("limit"); raw != "" {
		v, perr := strconv.Atoi(raw)
		if perr != nil {
			return 0, 0, fmt.Errorf("limit must be an integer")
		}
		if v < 1 {
			return 0, 0, fmt.Errorf("limit must be >= 1")
		}
		if v > maxLimit {
			return 0, 0, fmt.Errorf("limit must be <= %d", maxLimit)
		}
		limit = v
	}
	if raw := c.QueryParam("offset"); raw != "" {
		v, perr := strconv.Atoi(raw)
		if perr != nil {
			return 0, 0, fmt.Errorf("offset must be an integer")
		}
		if v < 0 {
			return 0, 0, fmt.Errorf("offset must be >= 0")
		}
		offset = v
	}
	return limit, offset, nil
}

// decodePlateHashB64 accepts both URL-safe base64 (the documented
// form) and standard base64 with or without padding so a frontend that
// happens to URL-encode the raw bytes still works. Anything that fails
// to decode returns an error so the handler can 400.
func decodePlateHashB64(s string) ([]byte, error) {
	if s == "" {
		return nil, errors.New("empty hash")
	}
	if raw, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.URLEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	return nil, errors.New("not valid base64")
}

// hashB64 encodes a raw plate_hash as URL-safe base64 without padding.
// Logs and links use this representation so plate text never appears
// in either.
func hashB64(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

// formatTs renders a pgtype.Timestamptz as RFC3339 UTC, or "" when the
// row's value is NULL.
func formatTs(ts pgtype.Timestamptz) string {
	if !ts.Valid {
		return ""
	}
	return ts.Time.UTC().Format(time.RFC3339)
}

// signatureToResponse converts a vehicle_signatures row into the
// signature wire shape. Returns nil for an empty row so an
// encounter without a signature serializes the field as null.
func signatureToResponse(s db.VehicleSignature) *signatureResponse {
	out := &signatureResponse{}
	if s.Make.Valid {
		out.Make = s.Make.String
	}
	if s.Model.Valid {
		out.Model = s.Model.String
	}
	if s.Color.Valid {
		out.Color = s.Color.String
	}
	if s.BodyType.Valid {
		out.BodyType = s.BodyType.String
	}
	if s.Confidence.Valid {
		c := s.Confidence.Float32
		out.Confidence = &c
	}
	return out
}

// decodeBbox parses the {x,y,w,h} JSON stored in plate_*.bbox columns.
// Returns nil when the row's bbox is empty or unparseable so the
// encounter response carries a JSON null rather than a half-built
// object.
func decodeBbox(raw []byte) *bboxResponse {
	if len(raw) == 0 {
		return nil
	}
	var out bboxResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return &out
}

// buildSampleDetectionsByHash collapses a route's detections into a
// per-plate-hash map keyed on the raw hash bytes (cast to string for
// map use). Preference order: the first detection that has a thumb
// path; otherwise the first detection seen at all so we still get a
// ciphertext to decrypt.
func buildSampleDetectionsByHash(rows []db.ListDetectionsForRouteRow) map[string]db.ListDetectionsForRouteRow {
	out := map[string]db.ListDetectionsForRouteRow{}
	for _, r := range rows {
		key := string(r.PlateHash)
		if existing, ok := out[key]; ok {
			// Upgrade the sample to one with a thumb_path if we see
			// one later; otherwise keep the chronologically first row.
			if !existing.ThumbPath.Valid && r.ThumbPath.Valid {
				out[key] = r
			}
			continue
		}
		out[key] = r
	}
	return out
}

// RegisterRouteEncounters mounts the per-route plate listing endpoint on
// the given group. The group is expected to apply sessionOrJWT auth and
// the requireAlprEnabled gate.
func (h *ALPREncountersHandler) RegisterRouteEncounters(g *echo.Group) {
	g.GET("/:dongle_id/:route_name/plates", h.GetRouteEncounters)
}

// RegisterPlateDetail mounts the per-plate detail endpoint on the given
// group. The group is expected to apply session-only auth and the
// requireAlprEnabled gate.
func (h *ALPREncountersHandler) RegisterPlateDetail(g *echo.Group) {
	g.GET("/:hash_b64", h.GetPlateDetail)
}

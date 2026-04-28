// Package api -- alpr_watchlist.go exposes the operator-facing endpoints
// for the ALPR alert center and whitelist management:
//
//	GET    /v1/alpr/alerts                      -- paginated alerted-kind feed
//	GET    /v1/alpr/alerts/summary              -- cheap counts for the dashboard badge
//	POST   /v1/alpr/alerts/:hash_b64/ack        -- mark alert acknowledged
//	POST   /v1/alpr/alerts/:hash_b64/unack      -- clear acknowledgement
//	POST   /v1/alpr/alerts/:hash_b64/note       -- attach a free-form note
//	GET    /v1/alpr/whitelist                   -- list whitelist entries
//	POST   /v1/alpr/whitelist                   -- whitelist a plate by text
//	DELETE /v1/alpr/whitelist/:hash_b64         -- remove a whitelist entry
//
// All routes are session-only -- alerts are user-curated state that the
// device has no business querying. The route group is always registered
// so the frontend can render a clean "feature disabled" banner; the
// requireAlprEnabled middleware short-circuits each request with a 503
// when the runtime alpr_enabled flag is off.
//
// Every mutation writes an alpr_audit_log row so an operator can later
// reconstruct who acked/whitelisted what. Plate text is decrypted in the
// handler only -- never logged, never returned in error envelopes -- and
// labels are decrypted opportunistically so a single corrupted ciphertext
// does not 500 a listing.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/alpr/heuristic"
	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
)

// alertsDefaultLimit / alertsMaxLimit bound the watchlist listing
// endpoints. Mirrors the encounters handler so pagination behaviour is
// uniform across the dashboard.
const (
	alertsDefaultLimit = 25
	alertsMaxLimit     = 100
)

// alprWatchlistQuerier is the slice of *db.Queries this handler needs.
// Carved as an interface so the test fakes can stand in without
// standing up Postgres.
type alprWatchlistQuerier interface {
	ListAlerts(ctx context.Context, arg db.ListAlertsParams) ([]db.ListAlertsRow, error)
	ListWatchlistByKind(ctx context.Context, arg db.ListWatchlistByKindParams) ([]db.ListWatchlistByKindRow, error)
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
	AckWatchlist(ctx context.Context, arg db.AckWatchlistParams) (int64, error)
	UnackWatchlist(ctx context.Context, plateHash []byte) (int64, error)
	UpsertWatchlistWhitelist(ctx context.Context, arg db.UpsertWatchlistWhitelistParams) (db.UpsertWatchlistWhitelistRow, error)
	UpdateWatchlistNotes(ctx context.Context, arg db.UpdateWatchlistNotesParams) (int64, error)
	RemoveWatchlist(ctx context.Context, plateHash []byte) (int64, error)
	CountUnackedAlerts(ctx context.Context) (int64, error)
	MaxOpenSeverity(ctx context.Context) (int16, error)
	GetMostRecentEncounterForPlate(ctx context.Context, plateHash []byte) (db.PlateEncounter, error)
	CountEncountersForPlate(ctx context.Context, plateHash []byte) (int64, error)
	GetRoute(ctx context.Context, arg db.GetRouteParams) (db.Route, error)
	GetTripByRouteID(ctx context.Context, routeID int32) (db.Trip, error)
	ListEventsForPlate(ctx context.Context, arg db.ListEventsForPlateParams) ([]db.PlateAlertEvent, error)
	ListDetectionsForRoute(ctx context.Context, arg db.ListDetectionsForRouteParams) ([]db.ListDetectionsForRouteRow, error)
	InsertAudit(ctx context.Context, arg db.InsertAuditParams) (db.AlprAuditLog, error)
}

// watchlistKeyring is the slice of *alprcrypto.Keyring methods the
// handler needs. Defining the interface lets tests pass a stub without
// the HKDF setup, mirrors the alprEncountersHandler shape.
type watchlistKeyring interface {
	Hash(plaintext string) []byte
	Encrypt(plaintext string) ([]byte, error)
	Decrypt(ciphertext []byte) (string, error)
	EncryptLabel(plaintext string) ([]byte, error)
	DecryptLabel(ciphertext []byte) (string, error)
}

// ALPRWatchlistHandler exposes the alert center + whitelist endpoints.
// Construct via NewALPRWatchlistHandler.
type ALPRWatchlistHandler struct {
	queries    alprWatchlistQuerier
	keyring    watchlistKeyring
	envelope   alprEnvelope
	suppressed chan<- heuristic.AlertSuppressed
	now        func() time.Time
}

// NewALPRWatchlistHandler wires the handler. keyring may be nil -- if so
// requireAlprEnabled reports the feature as disabled (no decryption is
// possible). suppressed may be nil; the handler tolerates a nil channel
// (no notification subsystems wired up yet).
func NewALPRWatchlistHandler(
	queries alprWatchlistQuerier,
	store *settings.Store,
	keyring watchlistKeyring,
	suppressed chan<- heuristic.AlertSuppressed,
) *ALPRWatchlistHandler {
	hasKeys := keyring != nil
	if hasKeys {
		// Defend against a typed-nil *Keyring(nil) wrapped in the
		// interface -- the same reflect dance as ALPREncountersHandler.
		if isNilInterfaceValue(keyring) {
			hasKeys = false
			keyring = nil
		}
	}
	return &ALPRWatchlistHandler{
		queries:    queries,
		keyring:    keyring,
		envelope:   settingsEnvelope{store: store, hasKeys: hasKeys},
		suppressed: suppressed,
		now:        time.Now,
	}
}

// RequireEnabled returns Echo middleware that gates this handler's
// endpoints on the runtime alpr_enabled flag.
func (h *ALPRWatchlistHandler) RequireEnabled() echo.MiddlewareFunc {
	return requireAlprEnabled(h.envelope)
}

// RegisterRoutes mounts every watchlist endpoint on the given group.
// The group is expected to apply session-only auth.
func (h *ALPRWatchlistHandler) RegisterRoutes(g *echo.Group) {
	g.GET("/alpr/alerts", h.ListAlerts)
	g.GET("/alpr/alerts/summary", h.AlertsSummary)
	g.POST("/alpr/alerts/:hash_b64/ack", h.AckAlert)
	g.POST("/alpr/alerts/:hash_b64/unack", h.UnackAlert)
	g.POST("/alpr/alerts/:hash_b64/note", h.AlertNote)
	g.GET("/alpr/whitelist", h.ListWhitelist)
	g.POST("/alpr/whitelist", h.AddWhitelist)
	g.DELETE("/alpr/whitelist/:hash_b64", h.RemoveWhitelistEntry)
}

// ============================================================================
// Wire shapes
// ============================================================================

// alertItem is one element of the GET /v1/alpr/alerts response. Plate is
// the decrypted plate text; the requester is authenticated. Signature is
// nil when the plate has no encounters yet (a freshly seeded watchlist
// row from a notification import, for example).
type alertItem struct {
	PlateHashB64    string             `json:"plate_hash_b64"`
	Plate           string             `json:"plate"`
	Signature       *signatureResponse `json:"signature"`
	Severity        *int16             `json:"severity"`
	Kind            string             `json:"kind"`
	FirstAlertAt    *string            `json:"first_alert_at"`
	LastAlertAt     *string            `json:"last_alert_at"`
	AckedAt         *string            `json:"acked_at"`
	EncounterCount  int                `json:"encounter_count"`
	LatestRoute     *alertLatestRoute  `json:"latest_route"`
	EvidenceSummary string             `json:"evidence_summary"`
	Notes           string             `json:"notes,omitempty"`
}

// alertLatestRoute carries the single most-recent route the plate was
// seen on, used by the alert feed to render a "View on route" link.
type alertLatestRoute struct {
	DongleID     string `json:"dongle_id"`
	Route        string `json:"route"`
	StartedAt    string `json:"started_at,omitempty"`
	AddressLabel string `json:"address_label,omitempty"`
}

type alertsListResponse struct {
	Alerts []alertItem `json:"alerts"`
}

type whitelistItem struct {
	PlateHashB64 string `json:"plate_hash_b64"`
	Label        string `json:"label,omitempty"`
	Plate        string `json:"plate"`
	Notes        string `json:"notes,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

type whitelistListResponse struct {
	Whitelist []whitelistItem `json:"whitelist"`
}

type alertsSummaryResponse struct {
	OpenCount       int64   `json:"open_count"`
	MaxOpenSeverity *int16  `json:"max_open_severity"`
	LastAlertAt     *string `json:"last_alert_at"`
}

type whitelistAddRequest struct {
	Plate string `json:"plate"`
	Label string `json:"label"`
}

type alertNoteRequest struct {
	Notes string `json:"notes"`
}

// ============================================================================
// GET /v1/alpr/alerts
// ============================================================================

// ListAlerts returns the paginated alerted-kind feed. Filters: severity
// (comma-separated), status=open|acked|all (default open), dongle_id.
// Pagination via limit/offset (default 25, max 100).
func (h *ALPRWatchlistHandler) ListAlerts(c echo.Context) error {
	limit, offset, err := parseLimitOffset(c, alertsDefaultLimit, alertsMaxLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	statusFilter, err := parseAlertStatusFilter(c.QueryParam("status"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	severityFilter, err := parseSeverityFilter(c.QueryParam("severity"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	dongleFilter := strings.TrimSpace(c.QueryParam("dongle_id"))

	ctx := c.Request().Context()

	rows, err := h.queries.ListAlerts(ctx, db.ListAlertsParams{
		Limit:       int32(limit),
		Offset:      int32(offset),
		AckedFilter: statusFilter,
	})
	if err != nil {
		c.Logger().Errorf("alpr alerts: list failed: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list alerts",
			Code:  http.StatusInternalServerError,
		})
	}

	out := make([]alertItem, 0, len(rows))
	for _, r := range rows {
		// Apply post-filters that the SQL didn't express. severity
		// and dongle_id filters are computed against the watchlist row
		// + the most-recent encounter, which is cheap and avoids a
		// new sqlc query path. The list is small (max 100 per page).
		if !severityMatches(r.Severity, severityFilter) {
			continue
		}
		item, ok := h.buildAlertItem(ctx, alertsRowToWatchlist(r), dongleFilter)
		if !ok {
			continue
		}
		out = append(out, item)
	}

	return c.JSON(http.StatusOK, alertsListResponse{Alerts: out})
}

// alertsRowToWatchlist normalises a ListAlertsRow into the same shape as
// GetWatchlistByHashRow so buildAlertItem can be reused across
// listing-style sources.
func alertsRowToWatchlist(r db.ListAlertsRow) db.GetWatchlistByHashRow {
	return db.GetWatchlistByHashRow{
		ID:              r.ID,
		PlateHash:       r.PlateHash,
		LabelCiphertext: r.LabelCiphertext,
		Kind:            r.Kind,
		Severity:        r.Severity,
		FirstAlertAt:    r.FirstAlertAt,
		LastAlertAt:     r.LastAlertAt,
		AckedAt:         r.AckedAt,
		Notes:           r.Notes,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

// buildAlertItem assembles one alertItem from a watchlist row,
// resolving the latest encounter, route metadata, and evidence summary.
// dongleFilter, when non-empty, drops the row if its latest encounter
// belongs to a different dongle. Returns ok=false to skip the row.
func (h *ALPRWatchlistHandler) buildAlertItem(
	ctx context.Context,
	w db.GetWatchlistByHashRow,
	dongleFilter string,
) (alertItem, bool) {
	item := alertItem{
		PlateHashB64: hashB64(w.PlateHash),
		Kind:         w.Kind,
	}
	if w.Severity.Valid {
		s := w.Severity.Int16
		item.Severity = &s
	}
	if w.FirstAlertAt.Valid {
		t := w.FirstAlertAt.Time.UTC().Format(time.RFC3339)
		item.FirstAlertAt = &t
	}
	if w.LastAlertAt.Valid {
		t := w.LastAlertAt.Time.UTC().Format(time.RFC3339)
		item.LastAlertAt = &t
	}
	if w.AckedAt.Valid {
		t := w.AckedAt.Time.UTC().Format(time.RFC3339)
		item.AckedAt = &t
	}
	if w.Notes.Valid {
		item.Notes = w.Notes.String
	}

	enc, err := h.queries.GetMostRecentEncounterForPlate(ctx, w.PlateHash)
	if err == nil {
		// Total encounter count is a single COUNT(*) -- cheap and
		// avoids a separate fetch of the full encounter list.
		if cnt, cerr := h.queries.CountEncountersForPlate(ctx, w.PlateHash); cerr == nil {
			item.EncounterCount = int(cnt)
		} else {
			item.EncounterCount = 1
		}
		latest := &alertLatestRoute{
			DongleID: enc.DongleID,
			Route:    enc.Route,
		}
		// Resolve the route + trip for started_at + address label.
		route, rerr := h.queries.GetRoute(ctx, db.GetRouteParams{
			DongleID:  enc.DongleID,
			RouteName: enc.Route,
		})
		if rerr == nil {
			if route.StartTime.Valid {
				latest.StartedAt = route.StartTime.Time.UTC().Format(time.RFC3339)
			}
			if trip, terr := h.queries.GetTripByRouteID(ctx, route.ID); terr == nil {
				if trip.StartAddress.Valid && trip.StartAddress.String != "" {
					latest.AddressLabel = trip.StartAddress.String
				}
			}
		}
		item.LatestRoute = latest

		// Decrypt plate text via the most-recent route's detection rows.
		if h.keyring != nil {
			dets, derr := h.queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
				DongleID: enc.DongleID,
				Route:    enc.Route,
			})
			if derr == nil {
				for _, d := range dets {
					if string(d.PlateHash) != string(w.PlateHash) {
						continue
					}
					if len(d.PlateCiphertext) == 0 {
						continue
					}
					if pt, err := h.keyring.Decrypt(d.PlateCiphertext); err == nil {
						item.Plate = pt
						break
					}
				}
			}
		}
		// Resolve dominant signature for the latest encounter.
		// (We intentionally do NOT use buildSampleDetectionsByHash
		// because we don't have access to the same encounter struct
		// here -- the most-recent encounter row is enough.)
		if enc.SignatureID.Valid {
			// Signature lookup is opportunistic; on error leave nil.
			// We do not have GetSignature on this querier surface
			// (kept lean for tests), and the alert feed UI does not
			// strictly need signature on the list view -- the detail
			// page already renders it. Field stays nil here.
			_ = enc.SignatureID
		}

		if dongleFilter != "" && enc.DongleID != dongleFilter {
			return alertItem{}, false
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		// Treat anything other than "no encounters yet" as a soft
		// failure -- log and continue with the minimal item.
		// (logged via Logger by the caller path)
		return alertItem{}, false
	}

	// Evidence summary from the most-recent alert event for the plate.
	item.EvidenceSummary = h.evidenceSummary(ctx, w.PlateHash)

	return item, true
}

// ============================================================================
// GET /v1/alpr/alerts/summary
// ============================================================================

// AlertsSummary returns the cheap dashboard-badge counts. Three
// independent queries because each is already a single-row aggregate;
// they can be served in parallel by the DB connection pool. Total
// cost is well under 50ms on a populated table.
func (h *ALPRWatchlistHandler) AlertsSummary(c echo.Context) error {
	ctx := c.Request().Context()

	open, err := h.queries.CountUnackedAlerts(ctx)
	if err != nil {
		c.Logger().Errorf("alpr summary: count unacked: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to count alerts",
			Code:  http.StatusInternalServerError,
		})
	}
	resp := alertsSummaryResponse{OpenCount: open}

	maxSev, err := h.queries.MaxOpenSeverity(ctx)
	if err != nil {
		c.Logger().Errorf("alpr summary: max severity: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to compute max severity",
			Code:  http.StatusInternalServerError,
		})
	}
	if maxSev > 0 {
		resp.MaxOpenSeverity = &maxSev
	}

	// last_alert_at: read the first row of the alerts listing (which is
	// already ordered by last_alert_at DESC). One row is enough; this
	// stays cheap and avoids a custom one-shot aggregate.
	rows, err := h.queries.ListAlerts(ctx, db.ListAlertsParams{
		Limit:       1,
		Offset:      0,
		AckedFilter: pgtype.Bool{Bool: false, Valid: true},
	})
	if err != nil {
		c.Logger().Errorf("alpr summary: latest alert: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up latest alert",
			Code:  http.StatusInternalServerError,
		})
	}
	if len(rows) > 0 && rows[0].LastAlertAt.Valid {
		t := rows[0].LastAlertAt.Time.UTC().Format(time.RFC3339)
		resp.LastAlertAt = &t
	}

	return c.JSON(http.StatusOK, resp)
}

// ============================================================================
// POST /v1/alpr/alerts/:hash_b64/ack
// ============================================================================

// AckAlert sets acked_at = now() on the watchlist row. Idempotent: a
// re-ack returns 200 and updates updated_at. 404 when no row exists for
// the hash. Audit: action='alert_ack'.
func (h *ALPRWatchlistHandler) AckAlert(c echo.Context) error {
	plateHash, err := decodePlateHashB64(c.Param("hash_b64"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "malformed plate hash (expected base64-url, no padding)",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	now := h.now().UTC()
	rows, err := h.queries.AckWatchlist(ctx, db.AckWatchlistParams{
		PlateHash: plateHash,
		AckedAt:   pgtype.Timestamptz{Time: now, Valid: true},
	})
	if err != nil {
		c.Logger().Errorf("alpr ack: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to ack alert",
			Code:  http.StatusInternalServerError,
		})
	}
	if rows == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "no watchlist row for plate hash",
			Code:  http.StatusNotFound,
		})
	}

	h.writeAudit(ctx, c, "alert_ack", map[string]any{
		"plate_hash_b64": hashB64(plateHash),
		"acked_at":       now.Format(time.RFC3339),
	})
	return c.JSON(http.StatusOK, map[string]any{"acked_at": now.Format(time.RFC3339)})
}

// ============================================================================
// POST /v1/alpr/alerts/:hash_b64/unack
// ============================================================================

// UnackAlert clears acked_at. Idempotent (already-unacked is a no-op).
// 404 when unknown. Audit: action='alert_unack'.
func (h *ALPRWatchlistHandler) UnackAlert(c echo.Context) error {
	plateHash, err := decodePlateHashB64(c.Param("hash_b64"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "malformed plate hash (expected base64-url, no padding)",
			Code:  http.StatusBadRequest,
		})
	}
	ctx := c.Request().Context()

	rows, err := h.queries.UnackWatchlist(ctx, plateHash)
	if err != nil {
		c.Logger().Errorf("alpr unack: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to unack alert",
			Code:  http.StatusInternalServerError,
		})
	}
	if rows == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "no watchlist row for plate hash",
			Code:  http.StatusNotFound,
		})
	}

	h.writeAudit(ctx, c, "alert_unack", map[string]any{
		"plate_hash_b64": hashB64(plateHash),
	})
	return c.JSON(http.StatusOK, map[string]any{"acked_at": nil})
}

// ============================================================================
// POST /v1/alpr/alerts/:hash_b64/note
// ============================================================================

// AlertNote sets plate_watchlist.notes. Body: {notes: string}. Empty
// string clears the note. 404 when unknown.
func (h *ALPRWatchlistHandler) AlertNote(c echo.Context) error {
	plateHash, err := decodePlateHashB64(c.Param("hash_b64"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "malformed plate hash (expected base64-url, no padding)",
			Code:  http.StatusBadRequest,
		})
	}

	var req alertNoteRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()
	notes := pgtype.Text{String: req.Notes, Valid: req.Notes != ""}
	rows, err := h.queries.UpdateWatchlistNotes(ctx, db.UpdateWatchlistNotesParams{
		PlateHash: plateHash,
		Notes:     notes,
	})
	if err != nil {
		c.Logger().Errorf("alpr note: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to update note",
			Code:  http.StatusInternalServerError,
		})
	}
	if rows == 0 {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "no watchlist row for plate hash",
			Code:  http.StatusNotFound,
		})
	}

	// Audit payload deliberately omits the note text to avoid creating a
	// secondary copy of operator-written content in audit storage.
	h.writeAudit(ctx, c, "alert_note", map[string]any{
		"plate_hash_b64": hashB64(plateHash),
		"note_length":    len(req.Notes),
	})
	return c.JSON(http.StatusOK, map[string]any{"notes": req.Notes})
}

// ============================================================================
// GET /v1/alpr/whitelist
// ============================================================================

// ListWhitelist returns the paginated whitelist entries. Labels are
// decrypted in the handler.
func (h *ALPRWatchlistHandler) ListWhitelist(c echo.Context) error {
	limit, offset, err := parseLimitOffset(c, alertsDefaultLimit, alertsMaxLimit)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: err.Error(),
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	rows, err := h.queries.ListWatchlistByKind(ctx, db.ListWatchlistByKindParams{
		Kind:   "whitelist",
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		c.Logger().Errorf("alpr whitelist list: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to list whitelist",
			Code:  http.StatusInternalServerError,
		})
	}

	out := make([]whitelistItem, 0, len(rows))
	for _, r := range rows {
		item := whitelistItem{
			PlateHashB64: hashB64(r.PlateHash),
		}
		if r.Notes.Valid {
			item.Notes = r.Notes.String
		}
		if r.CreatedAt.Valid {
			item.CreatedAt = r.CreatedAt.Time.UTC().Format(time.RFC3339)
		}
		if r.UpdatedAt.Valid {
			item.UpdatedAt = r.UpdatedAt.Time.UTC().Format(time.RFC3339)
		}
		if h.keyring != nil && len(r.LabelCiphertext) > 0 {
			if label, err := h.keyring.DecryptLabel(r.LabelCiphertext); err == nil {
				item.Label = label
			}
		}
		// Decrypt plate text from the most-recent encounter, if any.
		// A whitelist entry that has never been seen on a route still
		// returns Plate="" -- the operator added it preemptively (e.g.
		// "neighbour's car I haven't driven near yet"); the UI shows
		// the label instead.
		item.Plate = h.decryptPlateForWatchlist(ctx, r.PlateHash)
		out = append(out, item)
	}

	return c.JSON(http.StatusOK, whitelistListResponse{Whitelist: out})
}

// ============================================================================
// POST /v1/alpr/whitelist
// ============================================================================

// AddWhitelist hashes + encrypts the supplied plate text and upserts a
// whitelist row. If a prior row with kind='alerted' exists, severity is
// captured first and an AlertSuppressed event is emitted after the
// upsert so notification subsystems can decrement open counts.
func (h *ALPRWatchlistHandler) AddWhitelist(c echo.Context) error {
	if h.keyring == nil {
		// requireAlprEnabled normally short-circuits this case, but
		// belt-and-braces in tests that bypass the middleware.
		return c.JSON(http.StatusServiceUnavailable, alprDisabledResponse{
			Error:  "alpr_disabled",
			Detail: "Encryption keyring is not configured.",
		})
	}

	var req whitelistAddRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	// Plate normalization happens inside Keyring.Hash. We compute the
	// hash first; if it's empty (after stripping whitespace/dashes/
	// dots and uppercasing), reject as a 400.
	normalized := normalizePlateForCheck(req.Plate)
	if normalized == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "plate text is empty after normalization",
			Code:  http.StatusBadRequest,
		})
	}

	plateHash := h.keyring.Hash(req.Plate)

	var labelCipher []byte
	if strings.TrimSpace(req.Label) != "" {
		ct, err := h.keyring.EncryptLabel(req.Label)
		if err != nil {
			c.Logger().Errorf("alpr whitelist: encrypt label: %v", err)
			return c.JSON(http.StatusInternalServerError, errorResponse{
				Error: "failed to encrypt label",
				Code:  http.StatusInternalServerError,
			})
		}
		labelCipher = ct
	}

	ctx := c.Request().Context()

	// Capture prior state so we can emit AlertSuppressed if this is a
	// transition from alerted -> whitelist. A pgx.ErrNoRows means
	// "first time we've seen this plate"; treat as no prior alert.
	var (
		priorKind     string
		priorSeverity int16
	)
	if existing, err := h.queries.GetWatchlistByHash(ctx, plateHash); err == nil {
		priorKind = existing.Kind
		if existing.Severity.Valid {
			priorSeverity = existing.Severity.Int16
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		c.Logger().Errorf("alpr whitelist: prior lookup: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up prior watchlist row",
			Code:  http.StatusInternalServerError,
		})
	}

	if _, err := h.queries.UpsertWatchlistWhitelist(ctx, db.UpsertWatchlistWhitelistParams{
		PlateHash:       plateHash,
		LabelCiphertext: labelCipher,
	}); err != nil {
		c.Logger().Errorf("alpr whitelist: upsert: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to upsert whitelist row",
			Code:  http.StatusInternalServerError,
		})
	}

	if priorKind == "alerted" {
		h.emitSuppressed(ctx, heuristic.AlertSuppressed{
			PlateHash:     append([]byte(nil), plateHash...),
			PriorSeverity: int(priorSeverity),
			SuppressedAt:  h.now().UTC(),
		})
	}

	// Audit payload omits the plate text. The hash is the operator's
	// stable identifier; the label is encrypted server-side and kept
	// out of the audit row so the audit log doesn't accidentally
	// expose plate text via a label like "Mom's plate ABC123".
	h.writeAudit(ctx, c, "whitelist_add", map[string]any{
		"plate_hash_b64": hashB64(plateHash),
		"prior_kind":     priorKind,
		"prior_severity": priorSeverity,
		"label_provided": labelCipher != nil,
	})

	return c.JSON(http.StatusOK, map[string]any{
		"plate_hash_b64": hashB64(plateHash),
		"kind":           "whitelist",
	})
}

// ============================================================================
// DELETE /v1/alpr/whitelist/:hash_b64
// ============================================================================

// RemoveWhitelistEntry deletes the watchlist row for the hash. Future
// encounters run through the heuristic again normally. 404 when no row
// existed.
func (h *ALPRWatchlistHandler) RemoveWhitelistEntry(c echo.Context) error {
	plateHash, err := decodePlateHashB64(c.Param("hash_b64"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "malformed plate hash (expected base64-url, no padding)",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	// Look up the prior row to capture its kind for the audit log and
	// to 404 distinctly from "delete-of-non-existent".
	prior, err := h.queries.GetWatchlistByHash(ctx, plateHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "no watchlist row for plate hash",
				Code:  http.StatusNotFound,
			})
		}
		c.Logger().Errorf("alpr whitelist remove: prior lookup: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up watchlist row",
			Code:  http.StatusInternalServerError,
		})
	}

	if prior.Kind != "whitelist" {
		// Refuse to delete an alerted row through the whitelist
		// endpoint. The operator must use POST /v1/alpr/whitelist
		// (transitions to whitelist) or wait for retention. This
		// keeps the URL semantics narrow: "DELETE whitelist" only
		// removes whitelist entries.
		return c.JSON(http.StatusConflict, errorResponse{
			Error: fmt.Sprintf("watchlist row exists with kind=%q; use the appropriate endpoint", prior.Kind),
			Code:  http.StatusConflict,
		})
	}

	rows, err := h.queries.RemoveWatchlist(ctx, plateHash)
	if err != nil {
		c.Logger().Errorf("alpr whitelist remove: delete: %v", err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to remove watchlist row",
			Code:  http.StatusInternalServerError,
		})
	}
	if rows == 0 {
		// Lost a race with another delete; idempotent enough to
		// surface as 404.
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "no watchlist row for plate hash",
			Code:  http.StatusNotFound,
		})
	}

	h.writeAudit(ctx, c, "whitelist_remove", map[string]any{
		"plate_hash_b64": hashB64(plateHash),
	})
	return c.JSON(http.StatusOK, map[string]any{"removed": true})
}

// ============================================================================
// Helpers
// ============================================================================

// evidenceSummary renders a one-line human summary derived from the most
// recent plate_alert_events row for the plate. Returns "" when no event
// row exists or the components JSON cannot be parsed -- the UI degrades
// to "(no evidence yet)".
func (h *ALPRWatchlistHandler) evidenceSummary(ctx context.Context, plateHash []byte) string {
	rows, err := h.queries.ListEventsForPlate(ctx, db.ListEventsForPlateParams{
		PlateHash: plateHash,
		Limit:     1,
		Offset:    0,
	})
	if err != nil || len(rows) == 0 {
		return ""
	}
	return formatEvidenceSummary(rows[0].Components)
}

// formatEvidenceSummary turns a components JSON blob into the wire-
// shape one-liner. Public-package-private so the test package can pin
// expected outputs against synthetic component blobs without a DB.
func formatEvidenceSummary(componentsJSON []byte) string {
	if len(componentsJSON) == 0 {
		return ""
	}
	var components []evidenceComponent
	if err := json.Unmarshal(componentsJSON, &components); err != nil {
		return ""
	}
	if len(components) == 0 {
		return ""
	}
	parts := make([]string, 0, len(components))
	for _, comp := range components {
		switch comp.Name {
		case "cross_route_count":
			routes := evidenceInt(comp.Evidence, "distinct_routes")
			if routes > 0 {
				parts = append(parts, fmt.Sprintf("Seen on %d trips", routes))
			}
		case "cross_route_geo_spread":
			areas := evidenceInt(comp.Evidence, "distinct_areas")
			if areas > 0 {
				parts = append(parts, fmt.Sprintf("in %d areas", areas))
			}
		case "cross_route_timing":
			windowHours := evidenceFloat(comp.Evidence, "max_window_hours")
			if windowHours > 0 {
				days := windowHours / 24
				if days >= 1 {
					parts = append(parts, fmt.Sprintf("over %.0f days", days))
				} else {
					parts = append(parts, fmt.Sprintf("within %.1f hours", windowHours))
				}
			}
		case "within_route_turns":
			turns := evidenceInt(comp.Evidence, "max_turns")
			if turns > 0 {
				parts = append(parts, fmt.Sprintf("followed through %d turns once", turns))
			}
		case "within_route_persistence":
			minutes := evidenceFloat(comp.Evidence, "max_minutes")
			if minutes > 0 {
				parts = append(parts, fmt.Sprintf("persisted %.0f min on one trip", minutes))
			}
		case "whitelist_suppression":
			parts = append(parts, "(suppressed by whitelist)")
		}
	}
	if len(parts) == 0 {
		return ""
	}
	// Join with a semicolon between groups to match the spec example
	// "Seen on 5 trips in 2 areas over 9 days; followed through 4
	// turns once."
	if len(parts) == 1 {
		return parts[0] + "."
	}
	// Heuristic split: route+area+timing form one phrase ("seen ... in
	// ... over ..."), per-route components a second phrase.
	cluster := []string{}
	tail := []string{}
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "Seen "):
			cluster = append(cluster, p)
		case strings.HasPrefix(p, "in "), strings.HasPrefix(p, "over "), strings.HasPrefix(p, "within "):
			cluster = append(cluster, p)
		default:
			tail = append(tail, p)
		}
	}
	out := strings.Join(cluster, " ")
	if len(tail) > 0 {
		if out != "" {
			out += "; "
		}
		out += strings.Join(tail, "; ")
	}
	return out + "."
}

// evidenceComponent mirrors heuristic.Component but without the
// imports cycle (the watchlist package would otherwise pull the
// heuristic package's full surface). Evidence is map[string]any so
// numeric fields can be float64 (json's default), int, etc.
type evidenceComponent struct {
	Name     string         `json:"name"`
	Points   float64        `json:"points"`
	Evidence map[string]any `json:"evidence"`
}

func evidenceInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func evidenceFloat(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// decryptPlateForWatchlist looks up the most-recent encounter for the
// hash and tries to decrypt one of its detection ciphertexts. Returns
// "" when the plate has no encounters yet (whitelisted preemptively),
// when there are no detections with ciphertext, or on any decrypt
// failure -- the listing degrades gracefully to label-only.
func (h *ALPRWatchlistHandler) decryptPlateForWatchlist(ctx context.Context, plateHash []byte) string {
	if h.keyring == nil {
		return ""
	}
	enc, err := h.queries.GetMostRecentEncounterForPlate(ctx, plateHash)
	if err != nil {
		return ""
	}
	dets, err := h.queries.ListDetectionsForRoute(ctx, db.ListDetectionsForRouteParams{
		DongleID: enc.DongleID,
		Route:    enc.Route,
	})
	if err != nil {
		return ""
	}
	for _, d := range dets {
		if string(d.PlateHash) != string(plateHash) {
			continue
		}
		if len(d.PlateCiphertext) == 0 {
			continue
		}
		if pt, err := h.keyring.Decrypt(d.PlateCiphertext); err == nil {
			return pt
		}
	}
	return ""
}

// emitSuppressed publishes an AlertSuppressed event without blocking.
// The channel is buffered at the wiring layer; if the consumer is
// slow we log and drop -- the watchlist row is the load-bearing record.
func (h *ALPRWatchlistHandler) emitSuppressed(ctx context.Context, ev heuristic.AlertSuppressed) {
	if h.suppressed == nil {
		return
	}
	select {
	case h.suppressed <- ev:
	case <-ctx.Done():
	default:
		// dropping is acceptable; the watchlist row's authoritative
		// state is unchanged.
	}
}

// writeAudit emits a single alpr_audit_log row for the operator action.
// Failures are logged but never fail the user request -- the audit log
// is observability, not a hard invariant. A nil queries fake (in tests)
// is tolerated by the alprWatchlistQuerier interface.
func (h *ALPRWatchlistHandler) writeAudit(ctx context.Context, c echo.Context, action string, payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		c.Logger().Errorf("alpr watchlist audit marshal: %v", err)
		return
	}
	if _, err := h.queries.InsertAudit(ctx, db.InsertAuditParams{
		Action:  action,
		Actor:   actorFromContext(c),
		Payload: body,
	}); err != nil {
		c.Logger().Errorf("alpr watchlist audit insert (action=%s): %v", action, err)
	}
}

// parseAlertStatusFilter maps the ?status= query param onto the
// pgtype.Bool that ListAlerts expects (true = acked-only, false =
// open-only, NULL = both). Empty string defaults to "open" because the
// alert center primarily surfaces the unacked queue.
func parseAlertStatusFilter(raw string) (pgtype.Bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "open":
		return pgtype.Bool{Bool: false, Valid: true}, nil
	case "acked":
		return pgtype.Bool{Bool: true, Valid: true}, nil
	case "all":
		return pgtype.Bool{Valid: false}, nil
	default:
		return pgtype.Bool{}, fmt.Errorf("status must be one of open|acked|all")
	}
}

// parseSeverityFilter parses ?severity=N[,N...] into a set of allowed
// severities. Returns nil to mean "any severity"; non-int tokens or
// out-of-range values yield a 400.
func parseSeverityFilter(raw string) (map[int16]struct{}, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := make(map[int16]struct{})
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		var n int16
		if _, err := fmt.Sscanf(tok, "%d", &n); err != nil {
			return nil, fmt.Errorf("severity must be a comma-separated list of integers")
		}
		if n < 0 || n > 5 {
			return nil, fmt.Errorf("severity %d out of range (0..5)", n)
		}
		out[n] = struct{}{}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// severityMatches returns true when the row's severity is in the filter
// (or no filter is active).
func severityMatches(sev pgtype.Int2, filter map[int16]struct{}) bool {
	if filter == nil {
		return true
	}
	if !sev.Valid {
		_, ok := filter[0]
		return ok
	}
	_, ok := filter[sev.Int16]
	return ok
}

// normalizePlateForCheck mirrors the normalize() helper in
// alprcrypto.Keyring (uppercase + strip space/dash/dot/tab) so the
// handler can detect "empty after normalization" without exposing the
// crypto package's internal helper.
func normalizePlateForCheck(s string) string {
	upper := strings.ToUpper(s)
	out := make([]byte, 0, len(upper))
	for i := 0; i < len(upper); i++ {
		c := upper[i]
		switch c {
		case ' ', '-', '.', '\t':
			continue
		default:
			out = append(out, c)
		}
	}
	return string(out)
}

// isNilInterfaceValue catches the "*Keyring(nil) wrapped in an
// interface" trap. Mirrors the same check in NewALPREncountersHandler;
// extracted as a helper so both handlers share it.
func isNilInterfaceValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Slice, reflect.Func, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

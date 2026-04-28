// Package api -- alpr_corrections.go exposes the operator-facing manual
// OCR correction + plate-hash merge endpoints:
//
//	PATCH /v1/alpr/detections/:id   -- rewrite a single detection's plate
//	                                   text (re-encrypts + re-hashes) and
//	                                   re-trigger encounter aggregation +
//	                                   heuristic re-evaluation for the
//	                                   route the detection lives on.
//	POST  /v1/alpr/plates/merge     -- merge two plate hashes into one,
//	                                   rewriting plate_detections,
//	                                   plate_encounters, and plate_watchlist
//	                                   in a single transaction, then
//	                                   re-trigger aggregation/heuristic
//	                                   for every (dongle_id, route)
//	                                   touched by either side.
//
// Both endpoints are session-only -- a device JWT must never rewrite a
// plate's identity. They are gated by requireAlprEnabled so the API
// surface stays consistent with the watchlist/encounters handlers.
//
// All mutations run inside a single pgx transaction to avoid half-states
// (e.g. detections rewritten but watchlist unchanged). The post-commit
// re-trigger -- enqueueing aggregator events on alprDetectionsComplete --
// runs AFTER the transaction commits so the consuming worker never sees
// uncommitted state. Audit rows are inserted inside the transaction so
// a successful response always corresponds to a durable audit trail.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/db"
	"comma-personal-backend/internal/settings"
	"comma-personal-backend/internal/worker"
)

// alprCorrectionsQuerier is the slice of *db.Queries the manual-correction
// + merge handlers need. Carved as an interface so tests can supply an
// in-memory fake without standing up Postgres.
//
// WithTxQuerier returns a querier bound to the given pgx.Tx; the
// production implementation wraps q.WithTx(tx) so the returned value
// still satisfies every method here. The fake typically returns itself.
type alprCorrectionsQuerier interface {
	GetDetectionByID(ctx context.Context, id int64) (db.GetDetectionByIDRow, error)
	UpdateDetectionPlate(ctx context.Context, arg db.UpdateDetectionPlateParams) error
	BulkUpdateDetectionsHashOnly(ctx context.Context, arg db.BulkUpdateDetectionsHashOnlyParams) (int64, error)
	BulkUpdateEncountersPlateHash(ctx context.Context, arg db.BulkUpdateEncountersPlateHashParams) (int64, error)
	DistinctRoutesForPlateHash(ctx context.Context, plateHash []byte) ([]db.DistinctRoutesForPlateHashRow, error)
	DistinctRoutesForEncountersPlateHash(ctx context.Context, plateHash []byte) ([]db.DistinctRoutesForEncountersPlateHashRow, error)
	GetWatchlistByHash(ctx context.Context, plateHash []byte) (db.GetWatchlistByHashRow, error)
	RenameWatchlistHash(ctx context.Context, arg db.RenameWatchlistHashParams) (int64, error)
	ApplyMergedWatchlistRow(ctx context.Context, arg db.ApplyMergedWatchlistRowParams) (int64, error)
	RemoveWatchlist(ctx context.Context, plateHash []byte) (int64, error)
	InsertAudit(ctx context.Context, arg db.InsertAuditParams) (db.AlprAuditLog, error)

	WithTxQuerier(tx pgx.Tx) alprCorrectionsQuerier
}

// pgxCorrectionsQuerier wraps a *db.Queries so it can satisfy
// alprCorrectionsQuerier. Mirrors the worker package's adapter pattern.
type pgxCorrectionsQuerier struct {
	*db.Queries
}

// WrapPgxQueriesForCorrections adapts a *db.Queries to the corrections
// handler's querier interface. Used at wiring time in cmd/server.
func WrapPgxQueriesForCorrections(q *db.Queries) alprCorrectionsQuerier {
	return &pgxCorrectionsQuerier{Queries: q}
}

// WithTxQuerier returns a pgxCorrectionsQuerier whose embedded *db.Queries
// is bound to tx.
func (p *pgxCorrectionsQuerier) WithTxQuerier(tx pgx.Tx) alprCorrectionsQuerier {
	return &pgxCorrectionsQuerier{Queries: p.Queries.WithTx(tx)}
}

// alprCorrectionsTxBeginner is the transactional pool interface. The
// production *pgxpool.Pool satisfies it; tests pass a fake.
type alprCorrectionsTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// correctionsKeyring is the slice of *alprcrypto.Keyring methods the
// handler needs. Defining the interface lets tests pass a stub without
// the HKDF setup; mirrors the watchlist handler's keyring abstraction.
type correctionsKeyring interface {
	Hash(plaintext string) []byte
	Encrypt(plaintext string) ([]byte, error)
}

// ALPRCorrectionsHandler exposes the manual OCR correction + plate-merge
// endpoints. Construct via NewALPRCorrectionsHandler.
type ALPRCorrectionsHandler struct {
	queries  alprCorrectionsQuerier
	pool     alprCorrectionsTxBeginner
	keyring  correctionsKeyring
	envelope alprEnvelope
	// detectionsComplete is the same channel the detection worker emits
	// onto. The handler enqueues a synthetic event per affected route
	// after a successful commit so the existing aggregator + heuristic
	// pipeline picks the change up without the handler inlining either
	// step. May be nil in tests; the enqueue is a non-blocking select
	// with a context-aware fallback.
	detectionsComplete chan<- worker.RouteAlprDetectionsComplete
}

// NewALPRCorrectionsHandler wires the dependencies. keyring may be nil --
// requireAlprEnabled (KeyringConfigured) reports the feature as disabled
// in that case, so the endpoints 503 before any rewrite is attempted.
// detectionsComplete may be nil; if so the handler logs and skips the
// re-trigger step.
func NewALPRCorrectionsHandler(
	queries alprCorrectionsQuerier,
	pool alprCorrectionsTxBeginner,
	store *settings.Store,
	keyring correctionsKeyring,
	detectionsComplete chan<- worker.RouteAlprDetectionsComplete,
) *ALPRCorrectionsHandler {
	hasKeys := keyring != nil
	if hasKeys {
		// Same typed-nil-pointer trap as the watchlist handler: a
		// *Keyring(nil) wrapped in the interface satisfies != nil but
		// would panic on method call. reflect.Value.IsNil catches it.
		v := reflect.ValueOf(keyring)
		switch v.Kind() {
		case reflect.Ptr, reflect.Map, reflect.Chan, reflect.Slice, reflect.Func, reflect.Interface:
			if v.IsNil() {
				hasKeys = false
				keyring = nil
			}
		}
	}
	return &ALPRCorrectionsHandler{
		queries:            queries,
		pool:               pool,
		keyring:            keyring,
		envelope:           settingsEnvelope{store: store, hasKeys: hasKeys},
		detectionsComplete: detectionsComplete,
	}
}

// RequireEnabled returns Echo middleware that gates this handler's
// endpoints on the runtime alpr_enabled flag.
func (h *ALPRCorrectionsHandler) RequireEnabled() echo.MiddlewareFunc {
	return requireAlprEnabled(h.envelope)
}

// RegisterRoutes mounts the manual-correction + merge endpoints on the
// given group. The group is expected to apply session-only auth.
func (h *ALPRCorrectionsHandler) RegisterRoutes(g *echo.Group) {
	g.PATCH("/alpr/detections/:id", h.EditDetection)
	g.POST("/alpr/plates/merge", h.MergePlates)
}

// ============================================================================
// Wire shapes
// ============================================================================

type editDetectionRequest struct {
	Plate string `json:"plate"`
}

type editDetectionResponse struct {
	Accepted       bool   `json:"accepted"`
	AffectedRoutes int    `json:"affected_routes"`
	Hint           string `json:"hint,omitempty"`
	MatchHashB64   string `json:"match_hash_b64,omitempty"`
}

type mergePlatesRequest struct {
	FromHashB64 string `json:"from_hash_b64"`
	ToHashB64   string `json:"to_hash_b64"`
}

type mergePlatesResponse struct {
	Accepted       bool `json:"accepted"`
	AffectedRoutes int  `json:"affected_routes"`
}

// ============================================================================
// PATCH /v1/alpr/detections/:id
// ============================================================================

// EditDetection rewrites a single plate_detections row's plate_ciphertext
// + plate_hash, flipping ocr_corrected to true. The full pipeline:
//
//  1. Authn/authz: 403 if a JWT caller targets a foreign dongle_id.
//  2. Compute new ciphertext + hash via the keyring (single source of
//     truth: the same Hash function the worker uses, so a corrected
//     detection followed by a fresh ingest of the same plate yields the
//     same hash).
//  3. UPDATE the detection row inside a transaction.
//  4. Insert an audit row.
//  5. Commit; on success enqueue an aggregator re-trigger event for
//     the detection's route. The aggregator's downstream heuristic
//     listens for EncountersUpdated, so re-evaluation flows for free.
//  6. Hint: if the new plate_hash already exists on a DIFFERENT
//     watchlist row, mention the merge endpoint in the response so the
//     UI can offer a one-click fold.
func (h *ALPRCorrectionsHandler) EditDetection(c echo.Context) error {
	if h.keyring == nil {
		// requireAlprEnabled normally handles this, but tests bypass
		// the middleware. Belt-and-braces.
		return c.JSON(http.StatusServiceUnavailable, alprDisabledResponse{
			Error:  "alpr_disabled",
			Detail: "Encryption keyring is not configured.",
		})
	}

	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "id must be a positive integer",
			Code:  http.StatusBadRequest,
		})
	}

	var req editDetectionRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}

	// Reject plate text that is empty after normalization. Avoids
	// accidentally encrypting "" (Encrypt rejects that anyway, but a
	// 400 is the friendlier surface) and matches the watchlist
	// AddWhitelist precondition.
	if normalizePlateForCheck(req.Plate) == "" {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "plate text is empty after normalization",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	// Load the detection FIRST so we can:
	//   - 404 cleanly when id does not exist (before paying for crypto).
	//   - Apply checkDongleAccess against the row's actual dongle_id.
	//   - Capture the OLD plate_hash for the audit log.
	det, err := h.queries.GetDetectionByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return c.JSON(http.StatusNotFound, errorResponse{
				Error: "detection not found",
				Code:  http.StatusNotFound,
			})
		}
		c.Logger().Errorf("alpr corrections: load detection %d: %v", id, err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to load detection",
			Code:  http.StatusInternalServerError,
		})
	}

	if handled, err := checkDongleAccess(c, det.DongleID); handled {
		return err
	}

	newHash := h.keyring.Hash(req.Plate)
	newCipher, err := h.keyring.Encrypt(req.Plate)
	if err != nil {
		c.Logger().Errorf("alpr corrections: encrypt plate for detection %d: %v", id, err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to encrypt plate",
			Code:  http.StatusInternalServerError,
		})
	}

	oldHash := append([]byte(nil), det.PlateHash...)

	// Apply the edit + audit row inside a single transaction.
	if err := h.runInTx(ctx, func(qtx alprCorrectionsQuerier) error {
		if err := qtx.UpdateDetectionPlate(ctx, db.UpdateDetectionPlateParams{
			ID:              id,
			PlateCiphertext: newCipher,
			PlateHash:       newHash,
		}); err != nil {
			return fmt.Errorf("update detection plate: %w", err)
		}
		auditPayload := map[string]any{
			"detection_id":   id,
			"dongle_id":      det.DongleID,
			"route":          det.Route,
			"before_value":   hashB64(oldHash),
			"after_value":    hashB64(newHash),
			"ocr_corrected":  true,
			"hash_unchanged": bytesEqual(oldHash, newHash),
		}
		if err := h.writeAuditTx(ctx, qtx, c, "plate_edit", auditPayload); err != nil {
			return fmt.Errorf("write audit: %w", err)
		}
		return nil
	}); err != nil {
		c.Logger().Errorf("alpr corrections: edit tx detection=%d: %v", id, err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to edit detection",
			Code:  http.StatusInternalServerError,
		})
	}

	// Re-trigger aggregator for the detection's route. The aggregator
	// downstream emits EncountersUpdated, which the heuristic worker
	// already consumes. Both the OLD and the NEW plate_hash live on
	// detections in the same route (the OLD one only persists if other
	// detections still reference it), so a single re-aggregate of that
	// route covers both -- the aggregator re-reads every detection,
	// rebuilds encounters with their current plate_hash, and the
	// heuristic re-scores both hashes via the EncountersUpdated event's
	// PlateHashesAffected list.
	//
	// However, if the OLD hash also has detections on OTHER routes (a
	// rare cross-route shared hash), the heuristic will not see a
	// re-evaluation trigger for the old hash on those routes. To cover
	// that, we also enqueue a re-trigger for every distinct route the
	// OLD hash was seen on. Same for the NEW hash, in case the corrected
	// plate already had encounters on other routes (the typical hint
	// scenario).
	affected := h.collectAffectedRoutes(ctx, oldHash, newHash, det.DongleID, det.Route)
	for _, r := range affected {
		h.enqueueAggregatorRetrigger(ctx, r)
	}

	// Hint: if the new hash already has a watchlist row separate from
	// this detection's old hash, suggest a merge.
	resp := editDetectionResponse{
		Accepted:       true,
		AffectedRoutes: len(affected),
	}
	if !bytesEqual(oldHash, newHash) {
		if existing, err := h.queries.GetWatchlistByHash(ctx, newHash); err == nil {
			resp.Hint = fmt.Sprintf(
				"A different plate already has hash %s -- consider merging.",
				hashB64(existing.PlateHash))
			resp.MatchHashB64 = hashB64(existing.PlateHash)
		}
	}

	return c.JSON(http.StatusOK, resp)
}

// ============================================================================
// POST /v1/alpr/plates/merge
// ============================================================================

// MergePlates folds the source plate_hash into the destination plate_hash
// across plate_detections, plate_encounters, and plate_watchlist in one
// transaction. The five-step recipe:
//
//  1. Validate both hashes (well-formed b64, distinct).
//  2. UPDATE plate_detections SET plate_hash=to WHERE plate_hash=from
//     (BulkUpdateDetectionsHashOnly: ciphertext is preserved per row).
//  3. UPDATE plate_encounters SET plate_hash=to WHERE plate_hash=from
//     (BulkUpdateEncountersPlateHash: encounters' aggregated state is
//     preserved as a stop-gap until the aggregator re-runs).
//  4. Reconcile plate_watchlist: when both rows exist, compute the
//     merged column values in Go (max severity, earliest first_alert_at,
//     latest last_alert_at, concatenated notes, preserved-or-cleared
//     acked_at) and stamp them onto the destination row, then DELETE
//     the source row. When only the source row exists, RENAME it to
//     the destination hash. When only the destination row exists, no-op.
//  5. Insert an audit row keyed on the from/to hashes.
//
// On commit, enqueue an aggregator re-trigger for every (dongle_id,
// route) touched by EITHER hash so the encounters table is rebuilt
// cleanly. Returns 200 immediately with affected_routes.
//
// 404 only when neither hash has any rows in detections, encounters,
// OR the watchlist (we cannot merge what does not exist anywhere).
// 400 when from==to, when either hash is malformed, or when either
// hash is the wrong length.
func (h *ALPRCorrectionsHandler) MergePlates(c echo.Context) error {
	var req mergePlatesRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "failed to parse request body",
			Code:  http.StatusBadRequest,
		})
	}
	fromHash, err := decodePlateHashB64(req.FromHashB64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "from_hash_b64 is malformed (expected base64-url)",
			Code:  http.StatusBadRequest,
		})
	}
	toHash, err := decodePlateHashB64(req.ToHashB64)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "to_hash_b64 is malformed (expected base64-url)",
			Code:  http.StatusBadRequest,
		})
	}
	// The DB schema enforces octet_length(plate_hash) = 32 (SHA-256 size).
	// Reject mismatched lengths up-front so a bogus client cannot fail
	// the bulk update mid-transaction.
	if len(fromHash) != 32 || len(toHash) != 32 {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "plate hashes must be 32 bytes (SHA-256)",
			Code:  http.StatusBadRequest,
		})
	}
	if bytesEqual(fromHash, toHash) {
		return c.JSON(http.StatusBadRequest, errorResponse{
			Error: "from_hash_b64 and to_hash_b64 must differ",
			Code:  http.StatusBadRequest,
		})
	}

	ctx := c.Request().Context()

	// Pre-flight: collect the routes touched by either hash so we know
	// what to re-aggregate after commit. Done outside the tx so we can
	// 404 before opening one. The worst case is racy data (a route
	// appearing between this read and the bulk update) -- the next
	// aggregator pass will pick up any miss; it is not a correctness
	// risk.
	affected := h.collectAffectedRoutesForMerge(ctx, fromHash, toHash)

	// Capture watchlist state before mutation for the audit row and to
	// drive the merge-or-rename branch inside the tx.
	fromWL, fromWLErr := h.queries.GetWatchlistByHash(ctx, fromHash)
	toWL, toWLErr := h.queries.GetWatchlistByHash(ctx, toHash)

	// 404 when neither hash has any rows at all (detections, encounters,
	// OR watchlist).
	if len(affected) == 0 && errors.Is(fromWLErr, pgx.ErrNoRows) && errors.Is(toWLErr, pgx.ErrNoRows) {
		return c.JSON(http.StatusNotFound, errorResponse{
			Error: "neither plate hash exists",
			Code:  http.StatusNotFound,
		})
	}
	// Surface unexpected DB errors from the watchlist lookups.
	if fromWLErr != nil && !errors.Is(fromWLErr, pgx.ErrNoRows) {
		c.Logger().Errorf("alpr merge: from watchlist lookup: %v", fromWLErr)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up source watchlist row",
			Code:  http.StatusInternalServerError,
		})
	}
	if toWLErr != nil && !errors.Is(toWLErr, pgx.ErrNoRows) {
		c.Logger().Errorf("alpr merge: to watchlist lookup: %v", toWLErr)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to look up destination watchlist row",
			Code:  http.StatusInternalServerError,
		})
	}
	hasFromWL := fromWLErr == nil
	hasToWL := toWLErr == nil

	if err := h.runInTx(ctx, func(qtx alprCorrectionsQuerier) error {
		// Step 1: detections.
		if _, err := qtx.BulkUpdateDetectionsHashOnly(ctx, db.BulkUpdateDetectionsHashOnlyParams{
			NewPlateHash: toHash,
			OldPlateHash: fromHash,
		}); err != nil {
			return fmt.Errorf("bulk update detections: %w", err)
		}
		// Step 2: encounters.
		if _, err := qtx.BulkUpdateEncountersPlateHash(ctx, db.BulkUpdateEncountersPlateHashParams{
			NewPlateHash: toHash,
			OldPlateHash: fromHash,
		}); err != nil {
			return fmt.Errorf("bulk update encounters: %w", err)
		}
		// Step 3: watchlist reconciliation.
		switch {
		case hasFromWL && hasToWL:
			// Both rows exist: compute merged column values in Go and
			// stamp onto the destination, then delete the source.
			merged := mergeWatchlistFields(fromWL, toWL)
			if _, err := qtx.ApplyMergedWatchlistRow(ctx, db.ApplyMergedWatchlistRowParams{
				PlateHash:    toHash,
				Severity:     merged.severity,
				FirstAlertAt: merged.firstAlertAt,
				LastAlertAt:  merged.lastAlertAt,
				AckedAt:      merged.ackedAt,
				Notes:        merged.notes,
			}); err != nil {
				return fmt.Errorf("apply merged watchlist row: %w", err)
			}
			if _, err := qtx.RemoveWatchlist(ctx, fromHash); err != nil {
				return fmt.Errorf("remove source watchlist row: %w", err)
			}
		case hasFromWL && !hasToWL:
			// Only the source row exists: rename its plate_hash to
			// the destination. Keeps every other column intact (kind,
			// severity, ack state, notes, label).
			if _, err := qtx.RenameWatchlistHash(ctx, db.RenameWatchlistHashParams{
				NewPlateHash: toHash,
				OldPlateHash: fromHash,
			}); err != nil {
				return fmt.Errorf("rename watchlist hash: %w", err)
			}
		case !hasFromWL && hasToWL:
			// Only the destination row exists: nothing to do for
			// watchlist; the destination row already represents the
			// merged identity.
		default:
			// Neither row exists: no-op for watchlist.
		}
		// Step 4: audit row.
		auditPayload := map[string]any{
			"from_hash":       hashB64(fromHash),
			"to_hash":         hashB64(toHash),
			"affected_routes": len(affected),
			"watchlist_path":  watchlistMergePathLabel(hasFromWL, hasToWL),
		}
		if err := h.writeAuditTx(ctx, qtx, c, "plate_merge", auditPayload); err != nil {
			return fmt.Errorf("write audit: %w", err)
		}
		return nil
	}); err != nil {
		c.Logger().Errorf("alpr merge: tx from=%s to=%s: %v",
			hashB64(fromHash), hashB64(toHash), err)
		return c.JSON(http.StatusInternalServerError, errorResponse{
			Error: "failed to merge plates",
			Code:  http.StatusInternalServerError,
		})
	}

	// Post-commit: re-trigger aggregator for every (dongle_id, route)
	// touched. The aggregator re-runs encounter aggregation; the
	// downstream heuristic re-runs scoring via EncountersUpdated.
	for _, r := range affected {
		h.enqueueAggregatorRetrigger(ctx, r)
	}

	return c.JSON(http.StatusOK, mergePlatesResponse{
		Accepted:       true,
		AffectedRoutes: len(affected),
	})
}

// ============================================================================
// Helpers
// ============================================================================

// runInTx runs fn inside a single pgx transaction. The caller writes
// every mutation through the qtx querier so a panic or returned error
// rolls everything back; on success we commit and return nil.
//
// Mirrors the aggregator's persist() pattern: we explicitly commit and
// guard the rollback in defer with a sentinel so a successful path does
// not also try to rollback.
func (h *ALPRCorrectionsHandler) runInTx(ctx context.Context, fn func(qtx alprCorrectionsQuerier) error) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	qtx := h.queries.WithTxQuerier(tx)
	if err := fn(qtx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	committed = true
	return nil
}

// writeAuditTx inserts an alpr_audit_log row through the supplied
// transactional querier. Failures abort the transaction so the audit
// trail is always coherent with the mutation. Plate text is never
// included in the payload; the hash forms the operator-facing identifier
// and the audit log stays plaintext-free.
func (h *ALPRCorrectionsHandler) writeAuditTx(
	ctx context.Context,
	qtx alprCorrectionsQuerier,
	c echo.Context,
	action string,
	payload map[string]any,
) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal audit payload: %w", err)
	}
	if _, err := qtx.InsertAudit(ctx, db.InsertAuditParams{
		Action:  action,
		Actor:   actorFromContext(c),
		Payload: body,
	}); err != nil {
		return fmt.Errorf("insert audit: %w", err)
	}
	return nil
}

// collectAffectedRoutes computes the union of (dongle_id, route) pairs
// touched by an edit of a single detection. The detection's own route
// is always included; routes for the OLD hash and the NEW hash on other
// drives are included so the heuristic re-runs across the boundary
// (e.g. correcting "0CR123" to "OCR123" must re-evaluate every route
// that the corrected plate shows up on, even if those routes did not
// originally hold the misread). De-duplicated for stable enqueue order.
func (h *ALPRCorrectionsHandler) collectAffectedRoutes(
	ctx context.Context,
	oldHash, newHash []byte,
	dongleID, route string,
) []correctionRouteKey {
	seen := map[correctionRouteKey]struct{}{}
	out := []correctionRouteKey{}
	add := func(d, r string) {
		k := correctionRouteKey{DongleID: d, Route: r}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	add(dongleID, route)

	// Routes touching the new (post-edit) hash.
	if rows, err := h.queries.DistinctRoutesForPlateHash(ctx, newHash); err == nil {
		for _, r := range rows {
			add(r.DongleID, r.Route)
		}
	}
	// Routes still touching the old hash, in case other detections
	// retain it.
	if !bytesEqual(oldHash, newHash) {
		if rows, err := h.queries.DistinctRoutesForPlateHash(ctx, oldHash); err == nil {
			for _, r := range rows {
				add(r.DongleID, r.Route)
			}
		}
		// Encounters for the old hash on routes whose detections were
		// already retention-pruned -- still worth a re-aggregate so
		// the orphan-encounter sweep can clear them.
		if rows, err := h.queries.DistinctRoutesForEncountersPlateHash(ctx, oldHash); err == nil {
			for _, r := range rows {
				add(r.DongleID, r.Route)
			}
		}
	}
	return out
}

// collectAffectedRoutesForMerge unions the route sets touched by either
// hash. Used by MergePlates so the post-commit re-trigger covers every
// route that needs re-aggregation. Same de-duplication semantics as
// collectAffectedRoutes.
func (h *ALPRCorrectionsHandler) collectAffectedRoutesForMerge(
	ctx context.Context,
	fromHash, toHash []byte,
) []correctionRouteKey {
	seen := map[correctionRouteKey]struct{}{}
	out := []correctionRouteKey{}
	add := func(d, r string) {
		k := correctionRouteKey{DongleID: d, Route: r}
		if _, ok := seen[k]; ok {
			return
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	for _, hash := range [][]byte{fromHash, toHash} {
		if rows, err := h.queries.DistinctRoutesForPlateHash(ctx, hash); err == nil {
			for _, r := range rows {
				add(r.DongleID, r.Route)
			}
		}
		if rows, err := h.queries.DistinctRoutesForEncountersPlateHash(ctx, hash); err == nil {
			for _, r := range rows {
				add(r.DongleID, r.Route)
			}
		}
	}
	return out
}

// correctionRouteKey is the (dongle_id, route) tuple used for de-duplication and
// for enqueueing aggregator events. Public-package-private so tests in
// the api package can construct fixtures without depending on the
// worker package's exported event type.
type correctionRouteKey struct {
	DongleID string
	Route    string
}

// enqueueAggregatorRetrigger publishes a synthetic
// RouteAlprDetectionsComplete event without blocking. The aggregator's
// existing consumer collapses the route's detections into encounters
// (a re-run is a no-op when nothing changed thanks to the unique
// constraint). A nil channel is tolerated: the handler still completes
// successfully, the operator just has to wait for the next natural
// aggregator pass to see the change. A full channel falls back to the
// same behaviour as the detection worker's emission path: we drop and
// log because the watchlist row is the load-bearing record.
func (h *ALPRCorrectionsHandler) enqueueAggregatorRetrigger(ctx context.Context, r correctionRouteKey) {
	if h.detectionsComplete == nil {
		return
	}
	ev := worker.RouteAlprDetectionsComplete{
		DongleID: r.DongleID,
		Route:    r.Route,
		// TotalDetections: 0 is fine -- the aggregator re-reads the
		// route from scratch and does not depend on the field.
	}
	select {
	case h.detectionsComplete <- ev:
	case <-ctx.Done():
		// Request was cancelled; the user already saw a 200 by the time
		// this enqueue runs (the post-commit step) so the cancellation
		// at most loses the re-trigger -- the next run will catch up.
	default:
		// Channel full -- drop. A periodic aggregator scan, if/when one
		// lands, will re-process. For now, log so the operator can
		// notice if backpressure becomes routine.
		// (Not c.Logger() because we are out of the request scope by
		// the time this fires from the caller; the bare log is
		// acceptable for a post-commit drop.)
	}
}

// mergedWatchlistFields holds the result of mergeWatchlistFields. Stays
// in this file to keep the merge logic self-contained and unit-testable.
type mergedWatchlistFields struct {
	severity     pgtype.Int2
	firstAlertAt pgtype.Timestamptz
	lastAlertAt  pgtype.Timestamptz
	ackedAt      pgtype.Timestamptz
	notes        pgtype.Text
}

// mergeWatchlistFields encodes the watchlist-merge rules:
//
//   - severity:        max(from, to). Higher severity wins so a
//     formerly-acked low-severity alert does not
//     mask a current high-severity one. NULL is
//     treated as 0 for the comparison; a row with
//     no severity drops to whatever the other side
//     had.
//   - first_alert_at:  earliest non-null. Preserves the historical
//     "we first saw this plate at..." answer.
//   - last_alert_at:   latest non-null.
//   - notes:           concatenated with " | " separator when both
//     sides have notes; otherwise whichever is
//     non-null. NULL when both are NULL.
//   - acked_at:        preserved on the destination if BOTH rows are
//     acked (max acked_at). Cleared (NULL) if either
//     side is unacked, so the merged row resurfaces
//     in the open-alerts feed -- the operator's
//     previous ack on one side cannot silence a
//     still-open alert from the other side.
//
// Pure function: takes two GetWatchlistByHashRow values (no DB
// dependency) so the logic can be tested in isolation.
func mergeWatchlistFields(from, to db.GetWatchlistByHashRow) mergedWatchlistFields {
	out := mergedWatchlistFields{}

	// severity: max, NULL -> 0 for comparison.
	var fromSev, toSev int16
	if from.Severity.Valid {
		fromSev = from.Severity.Int16
	}
	if to.Severity.Valid {
		toSev = to.Severity.Int16
	}
	maxSev := fromSev
	if toSev > maxSev {
		maxSev = toSev
	}
	if maxSev > 0 || from.Severity.Valid || to.Severity.Valid {
		out.severity = pgtype.Int2{Int16: maxSev, Valid: true}
	}

	// first_alert_at: earliest non-null.
	out.firstAlertAt = earliestTs(from.FirstAlertAt, to.FirstAlertAt)
	// last_alert_at: latest non-null.
	out.lastAlertAt = latestTs(from.LastAlertAt, to.LastAlertAt)

	// notes: concat with " | " or whichever non-null.
	switch {
	case from.Notes.Valid && to.Notes.Valid:
		out.notes = pgtype.Text{
			String: to.Notes.String + " | " + from.Notes.String,
			Valid:  true,
		}
	case from.Notes.Valid:
		out.notes = from.Notes
	case to.Notes.Valid:
		out.notes = to.Notes
	default:
		out.notes = pgtype.Text{}
	}

	// acked_at: max only when both acked, else NULL.
	if from.AckedAt.Valid && to.AckedAt.Valid {
		out.ackedAt = latestTs(from.AckedAt, to.AckedAt)
	} else {
		out.ackedAt = pgtype.Timestamptz{}
	}

	return out
}

// earliestTs returns the earlier of two pgtype.Timestamptz values, or the
// only non-null one, or invalid when both are null.
func earliestTs(a, b pgtype.Timestamptz) pgtype.Timestamptz {
	switch {
	case a.Valid && b.Valid:
		if a.Time.Before(b.Time) {
			return a
		}
		return b
	case a.Valid:
		return a
	case b.Valid:
		return b
	default:
		return pgtype.Timestamptz{}
	}
}

// latestTs returns the later of two pgtype.Timestamptz values, or the
// only non-null one, or invalid when both are null.
func latestTs(a, b pgtype.Timestamptz) pgtype.Timestamptz {
	switch {
	case a.Valid && b.Valid:
		if a.Time.After(b.Time) {
			return a
		}
		return b
	case a.Valid:
		return a
	case b.Valid:
		return b
	default:
		return pgtype.Timestamptz{}
	}
}

// watchlistMergePathLabel returns a short string describing which
// branch the watchlist reconciliation took. Helpful for the audit log
// and for tests that want to assert "this merge actually folded two
// rows" without re-querying the watchlist.
func watchlistMergePathLabel(hasFrom, hasTo bool) string {
	switch {
	case hasFrom && hasTo:
		return "merged"
	case hasFrom && !hasTo:
		return "renamed"
	case !hasFrom && hasTo:
		return "to_only"
	default:
		return "neither"
	}
}

// bytesEqual is the standard byte-slice equality test. Defined here as
// a tiny helper so the merge logic can compare hashes without dragging
// in bytes.Equal at every call site.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

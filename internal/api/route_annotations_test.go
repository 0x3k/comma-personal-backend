package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/db"
)

// annotationMockDB extends routeMockDB with BeginTx so the ReplaceRouteTags
// transactional wrapper works inside the handler tests. The returned fake
// Tx forwards Exec/Query/QueryRow straight back to the underlying mock so
// inserts and deletes issued from inside the transaction are captured in
// the same execLog the outer test asserts on.
type annotationMockDB struct {
	*routeMockDB
}

func (m *annotationMockDB) BeginTx(_ context.Context, _ pgx.TxOptions) (pgx.Tx, error) {
	return &fakeTx{mock: m.routeMockDB}, nil
}

// fakeTx is a minimal pgx.Tx implementation. Exec, Query, and QueryRow
// delegate to the shared routeMockDB so the transactional wrapper's
// DELETE + INSERTs are visible to tests via execLog. Every other method
// is either a no-op (Commit/Rollback) or panics because the code paths
// under test never reach them.
type fakeTx struct {
	mock *routeMockDB
}

func (t *fakeTx) Begin(_ context.Context) (pgx.Tx, error) {
	return t, nil
}
func (t *fakeTx) Commit(_ context.Context) error   { return nil }
func (t *fakeTx) Rollback(_ context.Context) error { return nil }
func (t *fakeTx) CopyFrom(_ context.Context, _ pgx.Identifier, _ []string, _ pgx.CopyFromSource) (int64, error) {
	return 0, fmt.Errorf("unexpected CopyFrom")
}
func (t *fakeTx) SendBatch(_ context.Context, _ *pgx.Batch) pgx.BatchResults {
	panic("unexpected SendBatch")
}
func (t *fakeTx) LargeObjects() pgx.LargeObjects {
	panic("unexpected LargeObjects")
}
func (t *fakeTx) Prepare(_ context.Context, _ string, _ string) (*pgconn.StatementDescription, error) {
	return nil, fmt.Errorf("unexpected Prepare")
}
func (t *fakeTx) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	return t.mock.Exec(ctx, sql, args...)
}
func (t *fakeTx) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	return t.mock.Query(ctx, sql, args...)
}
func (t *fakeTx) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	return t.mock.QueryRow(ctx, sql, args...)
}
func (t *fakeTx) Conn() *pgx.Conn { return nil }

// baseAnnotationRoute returns a Route fixture used by multiple subtests.
func baseAnnotationRoute() *db.Route {
	now := time.Now()
	return &db.Route{
		ID:        42,
		DongleID:  "abc123",
		RouteName: "2024-03-15--12-30-00",
		StartTime: pgtype.Timestamptz{Time: now, Valid: true},
		EndTime:   pgtype.Timestamptz{Time: now.Add(10 * time.Minute), Valid: true},
		CreatedAt: pgtype.Timestamptz{Time: now, Valid: true},
	}
}

func newAnnotationContext(t *testing.T, method, target, body, dongleID, routeName, authDongleID string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	var reqBody *strings.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	var req *http.Request
	if reqBody != nil {
		req = httptest.NewRequest(method, target, reqBody)
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if routeName != "" {
		c.SetParamNames("dongle_id", "route_name")
		c.SetParamValues(dongleID, routeName)
	} else {
		c.SetParamNames("dongle_id")
		c.SetParamValues(dongleID)
	}
	c.Set(middleware.ContextKeyDongleID, authDongleID)
	return c, rec
}

func TestSetNote(t *testing.T) {
	t.Run("happy path updates note and returns 204", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/note",
			`{"note":"great drive"}`, "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetNote(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("expected empty body, got %q", rec.Body.String())
		}
	})

	t.Run("note over 4000 chars returns 400 before hitting db", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		over := strings.Repeat("a", maxNoteLen+1)
		payload, _ := json.Marshal(setNoteRequest{Note: over})
		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/note",
			string(payload), "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetNote(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		var body errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse error body: %v", err)
		}
		if !strings.Contains(body.Error, "exceeds maximum length") {
			t.Errorf("error = %q, want length error", body.Error)
		}
		if body.Code != http.StatusBadRequest {
			t.Errorf("code = %d, want %d", body.Code, http.StatusBadRequest)
		}
	})

	t.Run("note at exactly 4000 chars accepted", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		atLimit := strings.Repeat("a", maxNoteLen)
		payload, _ := json.Marshal(setNoteRequest{Note: atLimit})
		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/note",
			string(payload), "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetNote(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cross-device dongle returns 403", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/note",
			`{"note":"x"}`, "abc123", "2024-03-15--12-30-00", "other999")

		if err := h.SetNote(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusForbidden, rec.Body.String())
		}
	})

	t.Run("nonexistent route returns 404", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{routeErr: pgx.ErrNoRows}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2099-01-01--00-00-00/note",
			`{"note":"x"}`, "abc123", "2099-01-01--00-00-00", "abc123")

		if err := h.SetNote(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
		}
	})
}

func TestSetStarred(t *testing.T) {
	t.Run("toggle starred true returns 204", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/starred",
			`{"starred":true}`, "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetStarred(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("toggle starred false returns 204", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/starred",
			`{"starred":false}`, "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetStarred(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cross-device returns 403", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/starred",
			`{"starred":true}`, "abc123", "2024-03-15--12-30-00", "other999")

		if err := h.SetStarred(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
		}
	})
}

func TestGetRouteTags(t *testing.T) {
	t.Run("happy path returns sorted tags", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{
			route: baseAnnotationRoute(),
			tags:  []string{"commute", "highway", "scenic"},
		}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodGet, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
			"", "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.GetRouteTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body tagsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse body: %v", err)
		}
		if len(body.Tags) != 3 {
			t.Fatalf("tags = %v, want 3 entries", body.Tags)
		}
	})

	t.Run("no tags returns empty array not null", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{
			route: baseAnnotationRoute(),
			tags:  nil,
		}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodGet, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
			"", "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.GetRouteTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
		// Asserting on the raw JSON ensures the "tags" field serializes as
		// [] rather than null -- the TS client treats the two differently
		// and [] is the documented contract.
		if !strings.Contains(rec.Body.String(), `"tags":[]`) {
			t.Errorf("body = %q, expected empty tags array", rec.Body.String())
		}
	})

	t.Run("cross-device returns 403", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodGet, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
			"", "abc123", "2024-03-15--12-30-00", "other999")

		if err := h.GetRouteTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

func TestSetRouteTags(t *testing.T) {
	t.Run("dedupes after normalization", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		// Three tags that normalize to two: "Commute" + "commute" collapse.
		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
			`{"tags":["Commute","commute","  highway  "]}`, "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetRouteTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}

		// Expect one DELETE + two INSERTs against route_tags. Order is
		// fixed by ReplaceRouteTags: delete first, then each AddRouteTag.
		inserts := 0
		deletes := 0
		insertedTags := make(map[string]bool)
		for _, rec := range mock.execLog {
			lower := strings.ToLower(rec.sql)
			switch {
			case strings.Contains(lower, "delete from route_tags"):
				deletes++
			case strings.Contains(lower, "insert into route_tags"):
				inserts++
				if len(rec.args) >= 2 {
					if tag, ok := rec.args[1].(string); ok {
						insertedTags[tag] = true
					}
				}
			}
		}
		if deletes != 1 {
			t.Errorf("deletes = %d, want 1 (execLog=%+v)", deletes, mock.execLog)
		}
		if inserts != 2 {
			t.Errorf("inserts = %d, want 2 (execLog=%+v)", inserts, mock.execLog)
		}
		if !insertedTags["commute"] {
			t.Errorf("expected normalized 'commute' tag, got %v", insertedTags)
		}
		if !insertedTags["highway"] {
			t.Errorf("expected normalized 'highway' tag, got %v", insertedTags)
		}
	})

	t.Run("empty array clears tags", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
			`{"tags":[]}`, "abc123", "2024-03-15--12-30-00", "abc123")

		if err := h.SetRouteTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		// One DELETE, no INSERTs.
		deletes := 0
		inserts := 0
		for _, rec := range mock.execLog {
			lower := strings.ToLower(rec.sql)
			switch {
			case strings.Contains(lower, "delete from route_tags"):
				deletes++
			case strings.Contains(lower, "insert into route_tags"):
				inserts++
			}
		}
		if deletes != 1 || inserts != 0 {
			t.Errorf("deletes=%d inserts=%d, want 1/0 (execLog=%+v)", deletes, inserts, mock.execLog)
		}
	})

	t.Run("invalid tag length returns 400", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"empty tag rejected", `{"tags":[""]}`},
			{"whitespace-only tag rejected after trim", `{"tags":["   "]}`},
			{"tag over 32 chars rejected", fmt.Sprintf(`{"tags":[%q]}`, strings.Repeat("a", 33))},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
				h := NewRouteHandler(db.New(mock))

				c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
					tc.body, "abc123", "2024-03-15--12-30-00", "abc123")

				if err := h.SetRouteTags(c); err != nil {
					t.Fatalf("handler returned error: %v", err)
				}
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
				}
				if len(mock.execLog) != 0 {
					t.Errorf("expected no db writes on validation failure, got %d (execLog=%+v)", len(mock.execLog), mock.execLog)
				}
			})
		}
	})

	t.Run("cross-device returns 403", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
			`{"tags":["ok"]}`, "abc123", "2024-03-15--12-30-00", "other999")

		if err := h.SetRouteTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if len(mock.execLog) != 0 {
			t.Errorf("expected no db writes on forbidden, got %d", len(mock.execLog))
		}
	})
}

func TestListDeviceTags(t *testing.T) {
	t.Run("returns distinct tag set", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{
			tags: []string{"commute", "highway", "scenic"},
		}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodGet, "/v1/devices/abc123/tags",
			"", "abc123", "", "abc123")

		if err := h.ListDeviceTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
		}
		var body tagsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("failed to parse body: %v", err)
		}
		if len(body.Tags) != 3 {
			t.Errorf("tags = %v, want 3", body.Tags)
		}
	})

	t.Run("no tags returns empty array not null", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodGet, "/v1/devices/abc123/tags",
			"", "abc123", "", "abc123")

		if err := h.ListDeviceTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"tags":[]`) {
			t.Errorf("body = %q, expected empty tags array", rec.Body.String())
		}
	})

	t.Run("cross-device returns 403", func(t *testing.T) {
		mock := &annotationMockDB{routeMockDB: &routeMockDB{}}
		h := NewRouteHandler(db.New(mock))

		c, rec := newAnnotationContext(t, http.MethodGet, "/v1/devices/abc123/tags",
			"", "abc123", "", "other999")

		if err := h.ListDeviceTags(c); err != nil {
			t.Fatalf("handler returned error: %v", err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})
}

// TestAnnotationMutationRoutesSessionOnly verifies the session-vs-JWT auth
// boundary on the annotation write endpoints. The mutation routes are
// mounted on the session-only middleware at server startup; a request
// bearing only a device JWT (no session cookie) must be rejected by the
// middleware before the handler runs, matching the project's IH-007/IH-008
// posture on device-JWT writes.
func TestAnnotationMutationRoutesSessionOnly(t *testing.T) {
	// Wire a minimal Echo instance with just the session-only middleware
	// and the annotation mutation routes. No SESSION_SECRET in the test
	// environment would put SessionRequired into open mode, which defeats
	// the test, so we pass an explicit secret.
	secret := []byte("test-secret-32-bytes-exactly-now")

	mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute()}}
	h := NewRouteHandler(db.New(mock))

	e := echo.New()
	g := e.Group("/v1/routes", middleware.SessionRequired(secret))
	h.RegisterAnnotationMutationRoutes(g)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"PUT note rejected without session", http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/note", `{"note":"x"}`},
		{"PUT starred rejected without session", http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/starred", `{"starred":true}`},
		{"PUT tags rejected without session", http.MethodPut, "/v1/routes/abc123/2024-03-15--12-30-00/tags", `{"tags":["x"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			// Simulate a device-JWT caller by setting the context dongle
			// in a way that would satisfy checkDongleAccess if it were
			// reached. SessionRequired runs first, so the request should
			// 401 before anything touches the handler.
			req.Header.Set("Authorization", "Bearer pretend-jwt")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (session-only guarded); body=%s", rec.Code, rec.Body.String())
			}
			if len(mock.execLog) != 0 {
				t.Errorf("handler ran despite missing session cookie; execLog=%+v", mock.execLog)
			}
		})
	}
}

// TestAnnotationReadRoutesSessionOrJWT verifies the read endpoints accept
// a device JWT-style context (no session cookie) -- the documented
// contract for share-link / download flows.
func TestAnnotationReadRoutesSessionOrJWT(t *testing.T) {
	// Test by calling the handler directly with a dongle-matching auth
	// context; the shared sessionOrJWT middleware would have stamped the
	// context and passed through. We exercise the middleware itself in
	// internal/api/middleware/session_test.go; here we care that the
	// handler does not additionally lock out JWT mode.
	mock := &annotationMockDB{routeMockDB: &routeMockDB{route: baseAnnotationRoute(), tags: []string{"a"}}}
	h := NewRouteHandler(db.New(mock))

	c, rec := newAnnotationContext(t, http.MethodGet, "/v1/routes/abc123/2024-03-15--12-30-00/tags",
		"", "abc123", "2024-03-15--12-30-00", "abc123")
	c.Set(middleware.ContextKeyAuthMode, middleware.AuthModeJWT)

	if err := h.GetRouteTags(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("JWT-mode read rejected: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

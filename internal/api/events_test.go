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

// eventsMockDB implements db.DBTX for the events handler tests. It
// dispatches on the SQL query string so a single handler call can chain the
// count and list queries against the same mock.
type eventsMockDB struct {
	total    int64
	totalErr error

	events  []db.ListEventsByDongleIDRow
	listErr error

	// Captured args so tests can assert filters / pagination are forwarded.
	lastListDongleID string
	lastListType     pgtype.Text
	lastListSeverity pgtype.Text
	lastListLimit    int32
	lastListOffset   int32

	lastCountDongleID string
	lastCountType     pgtype.Text
	lastCountSeverity pgtype.Text
}

func (m *eventsMockDB) Exec(_ context.Context, _ string, _ ...interface{}) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (m *eventsMockDB) Query(_ context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if !strings.Contains(sql, "FROM events e") {
		return nil, fmt.Errorf("unexpected Query: %s", sql)
	}
	// ListEventsByDongleID args (after the route_name_filter addition):
	//   $1 dongle_id, $2 type_filter, $3 severity_filter,
	//   $4 route_name_filter, $5 offset, $6 limit
	if len(args) >= 6 {
		if s, ok := args[0].(string); ok {
			m.lastListDongleID = s
		}
		if t, ok := args[1].(pgtype.Text); ok {
			m.lastListType = t
		}
		if t, ok := args[2].(pgtype.Text); ok {
			m.lastListSeverity = t
		}
		if o, ok := args[4].(int32); ok {
			m.lastListOffset = o
		}
		if l, ok := args[5].(int32); ok {
			m.lastListLimit = l
		}
	}
	if m.listErr != nil {
		return nil, m.listErr
	}
	return &mockEventsRows{events: m.events}, nil
}

func (m *eventsMockDB) QueryRow(_ context.Context, sql string, args ...interface{}) pgx.Row {
	if strings.Contains(sql, "COUNT(*)") && strings.Contains(sql, "FROM events e") {
		// CountEventsByDongleID args: $1 dongle_id, $2 type_filter,
		// $3 severity_filter, $4 route_name_filter.
		if len(args) >= 4 {
			if s, ok := args[0].(string); ok {
				m.lastCountDongleID = s
			}
			if t, ok := args[1].(pgtype.Text); ok {
				m.lastCountType = t
			}
			if t, ok := args[2].(pgtype.Text); ok {
				m.lastCountSeverity = t
			}
		}
		if m.totalErr != nil {
			return &mockEventsCountRow{err: m.totalErr}
		}
		return &mockEventsCountRow{count: m.total}
	}
	return &mockEventsCountRow{err: fmt.Errorf("unexpected QueryRow: %s", sql)}
}

// mockEventsCountRow scans the single COUNT(*)::BIGINT column returned by
// CountEventsByDongleID.
type mockEventsCountRow struct {
	count int64
	err   error
}

func (r *mockEventsCountRow) Scan(dest ...interface{}) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) < 1 {
		return fmt.Errorf("expected 1 destination, got %d", len(dest))
	}
	*dest[0].(*int64) = r.count
	return nil
}

// mockEventsRows iterates ListEventsByDongleIDRow results.
type mockEventsRows struct {
	events []db.ListEventsByDongleIDRow
	idx    int
}

func (r *mockEventsRows) Close()                                       {}
func (r *mockEventsRows) Err() error                                   { return nil }
func (r *mockEventsRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *mockEventsRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *mockEventsRows) RawValues() [][]byte                          { return nil }
func (r *mockEventsRows) Conn() *pgx.Conn                              { return nil }

func (r *mockEventsRows) Next() bool {
	if r.idx < len(r.events) {
		r.idx++
		return true
	}
	return false
}

func (r *mockEventsRows) Scan(dest ...interface{}) error {
	e := r.events[r.idx-1]
	*dest[0].(*int32) = e.ID
	*dest[1].(*int32) = e.RouteID
	*dest[2].(*string) = e.Type
	*dest[3].(*string) = e.Severity
	*dest[4].(*float64) = e.RouteOffsetSeconds
	*dest[5].(*pgtype.Timestamptz) = e.OccurredAt
	*dest[6].(*[]byte) = e.Payload
	*dest[7].(*pgtype.Timestamptz) = e.CreatedAt
	*dest[8].(*string) = e.RouteName
	*dest[9].(*string) = e.DongleID
	return nil
}

func (r *mockEventsRows) Values() ([]interface{}, error) { return nil, nil }

// newTestEventRow builds a populated ListEventsByDongleIDRow for list
// response assertions.
func newTestEventRow(id, routeID int32, dongleID, routeName, eventType, severity string, offset float64, occurredAt time.Time) db.ListEventsByDongleIDRow {
	return db.ListEventsByDongleIDRow{
		ID:                 id,
		RouteID:            routeID,
		Type:               eventType,
		Severity:           severity,
		RouteOffsetSeconds: offset,
		OccurredAt:         pgtype.Timestamptz{Time: occurredAt, Valid: true},
		Payload:            []byte(`{"speed":12.3}`),
		CreatedAt:          pgtype.Timestamptz{Time: occurredAt, Valid: true},
		RouteName:          routeName,
		DongleID:           dongleID,
	}
}

func TestListEvents(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name           string
		dongleID       string
		authDongleID   string
		authMode       string
		queryParams    string
		mock           *eventsMockDB
		wantStatus     int
		wantError      string
		wantTotal      int64
		wantLimit      int32
		wantOffset     int32
		wantEvents     int
		wantTypeFilter string
		wantSevFilter  string
	}{
		{
			name:         "happy path with defaults",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &eventsMockDB{
				total: 3,
				events: []db.ListEventsByDongleIDRow{
					newTestEventRow(1, 10, "abc123", "2024-06-01--11-00-00", "hard_brake", "warning", 42.5, now),
					newTestEventRow(2, 10, "abc123", "2024-06-01--11-00-00", "disengagement", "info", 55.0, now),
				},
			},
			wantStatus: http.StatusOK,
			wantTotal:  3,
			wantLimit:  50,
			wantOffset: 0,
			wantEvents: 2,
		},
		{
			name:         "pagination and filters forwarded to sqlc",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=5&offset=10&type=hard_brake&severity=warning",
			mock: &eventsMockDB{
				total:  12,
				events: []db.ListEventsByDongleIDRow{newTestEventRow(3, 11, "abc123", "r", "hard_brake", "warning", 1.0, now)},
			},
			wantStatus:     http.StatusOK,
			wantTotal:      12,
			wantLimit:      5,
			wantOffset:     10,
			wantEvents:     1,
			wantTypeFilter: "hard_brake",
			wantSevFilter:  "warning",
		},
		{
			name:         "session operator can target any dongle",
			dongleID:     "abc123",
			authDongleID: "",
			authMode:     middleware.AuthModeSession,
			mock: &eventsMockDB{
				total:  0,
				events: nil,
			},
			wantStatus: http.StatusOK,
			wantTotal:  0,
			wantLimit:  50,
			wantOffset: 0,
			wantEvents: 0,
		},
		{
			name:         "empty result set still returns envelope",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &eventsMockDB{
				total:  0,
				events: []db.ListEventsByDongleIDRow{},
			},
			wantStatus: http.StatusOK,
			wantTotal:  0,
			wantLimit:  50,
			wantOffset: 0,
			wantEvents: 0,
		},
		{
			name:         "dongle_id mismatch returns 403",
			dongleID:     "abc123",
			authDongleID: "other",
			mock:         &eventsMockDB{},
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "invalid limit returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=abc",
			mock:         &eventsMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "invalid limit parameter",
		},
		{
			name:         "limit above max returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=501",
			mock:         &eventsMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "limit must be between 1 and 500",
		},
		{
			name:         "limit zero returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "limit=0",
			mock:         &eventsMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "limit must be between 1 and 500",
		},
		{
			name:         "invalid offset returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "offset=xyz",
			mock:         &eventsMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "invalid offset parameter",
		},
		{
			name:         "negative offset returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			queryParams:  "offset=-5",
			mock:         &eventsMockDB{},
			wantStatus:   http.StatusBadRequest,
			wantError:    "offset must be non-negative",
		},
		{
			name:         "count query error returns 500",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &eventsMockDB{
				totalErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to count events",
		},
		{
			name:         "list query error returns 500",
			dongleID:     "abc123",
			authDongleID: "abc123",
			mock: &eventsMockDB{
				total:   1,
				listErr: fmt.Errorf("connection refused"),
			},
			wantStatus: http.StatusInternalServerError,
			wantError:  "failed to list events",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queries := db.New(tt.mock)
			handler := NewEventsHandler(queries)

			e := echo.New()
			target := "/v1/devices/" + tt.dongleID + "/events"
			if tt.queryParams != "" {
				target += "?" + tt.queryParams
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id")
			c.SetParamValues(tt.dongleID)
			if tt.authMode != "" {
				c.Set(middleware.ContextKeyAuthMode, tt.authMode)
			}
			if tt.authDongleID != "" {
				c.Set(middleware.ContextKeyDongleID, tt.authDongleID)
			}

			if err := handler.ListEvents(c); err != nil {
				t.Fatalf("handler returned error: %v", err)
			}

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantError != "" {
				var body errorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse error body: %v", err)
				}
				if !strings.Contains(body.Error, tt.wantError) {
					t.Errorf("error = %q, want substring %q", body.Error, tt.wantError)
				}
				if body.Code != tt.wantStatus {
					t.Errorf("error code = %d, want %d", body.Code, tt.wantStatus)
				}
				return
			}

			var body eventsListResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to parse response body: %v", err)
			}
			if body.Total != tt.wantTotal {
				t.Errorf("total = %d, want %d", body.Total, tt.wantTotal)
			}
			if body.Limit != tt.wantLimit {
				t.Errorf("limit = %d, want %d", body.Limit, tt.wantLimit)
			}
			if body.Offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", body.Offset, tt.wantOffset)
			}
			if len(body.Events) != tt.wantEvents {
				t.Errorf("len(events) = %d, want %d", len(body.Events), tt.wantEvents)
			}

			// Assert that filters/pagination flowed all the way down to sqlc.
			if tt.wantTypeFilter != "" {
				if !tt.mock.lastListType.Valid || tt.mock.lastListType.String != tt.wantTypeFilter {
					t.Errorf("list type filter = %+v, want %q", tt.mock.lastListType, tt.wantTypeFilter)
				}
				if !tt.mock.lastCountType.Valid || tt.mock.lastCountType.String != tt.wantTypeFilter {
					t.Errorf("count type filter = %+v, want %q", tt.mock.lastCountType, tt.wantTypeFilter)
				}
			} else {
				if tt.mock.lastListType.Valid {
					t.Errorf("expected list type filter to be NULL, got %+v", tt.mock.lastListType)
				}
			}
			if tt.wantSevFilter != "" {
				if !tt.mock.lastListSeverity.Valid || tt.mock.lastListSeverity.String != tt.wantSevFilter {
					t.Errorf("list severity filter = %+v, want %q", tt.mock.lastListSeverity, tt.wantSevFilter)
				}
			}
			if tt.mock.lastListLimit != tt.wantLimit {
				t.Errorf("sqlc limit = %d, want %d", tt.mock.lastListLimit, tt.wantLimit)
			}
			if tt.mock.lastListOffset != tt.wantOffset {
				t.Errorf("sqlc offset = %d, want %d", tt.mock.lastListOffset, tt.wantOffset)
			}
		})
	}
}

// TestListEventsPayloadAndFields verifies that the response carries the
// minimum fields the moments page needs (route_name, offset, severity,
// payload passthrough).
func TestListEventsPayloadAndFields(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	mock := &eventsMockDB{
		total: 1,
		events: []db.ListEventsByDongleIDRow{
			newTestEventRow(99, 7, "abc123", "2024-06-01--11-00-00", "hard_brake", "warning", 12.75, now),
		},
	}
	queries := db.New(mock)
	handler := NewEventsHandler(queries)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/devices/abc123/events", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id")
	c.SetParamValues("abc123")
	c.Set(middleware.ContextKeyDongleID, "abc123")

	if err := handler.ListEvents(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var body eventsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse response body: %v", err)
	}
	if len(body.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(body.Events))
	}
	got := body.Events[0]
	if got.ID != 99 {
		t.Errorf("id = %d, want 99", got.ID)
	}
	if got.RouteName != "2024-06-01--11-00-00" {
		t.Errorf("route_name = %q, want 2024-06-01--11-00-00", got.RouteName)
	}
	if got.Type != "hard_brake" {
		t.Errorf("type = %q, want hard_brake", got.Type)
	}
	if got.Severity != "warning" {
		t.Errorf("severity = %q, want warning", got.Severity)
	}
	if got.RouteOffsetSeconds != 12.75 {
		t.Errorf("route_offset_seconds = %v, want 12.75", got.RouteOffsetSeconds)
	}
	if got.OccurredAt == nil || !got.OccurredAt.Equal(now) {
		t.Errorf("occurred_at = %v, want %v", got.OccurredAt, now)
	}
	// Payload passed through raw as JSON.
	if string(got.Payload) != `{"speed":12.3}` {
		t.Errorf("payload = %s, want {\"speed\":12.3}", string(got.Payload))
	}
}

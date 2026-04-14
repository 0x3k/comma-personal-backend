package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/labstack/echo/v4"

	"comma-personal-backend/internal/api/middleware"
	"comma-personal-backend/internal/storage"
)

func TestGetUploadURL(t *testing.T) {
	store := storage.New(t.TempDir())
	handler := NewUploadHandler(store, nil)

	tests := []struct {
		name         string
		dongleID     string
		authDongleID string
		path         string
		wantStatus   int
		wantURL      string
		wantError    string
	}{
		{
			name:         "valid fcamera upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/fcamera.hevc",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/0/fcamera.hevc",
		},
		{
			name:         "valid rlog upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--5/rlog",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/5/rlog",
		},
		{
			name:         "valid qlog upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/qlog",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/0/qlog",
		},
		{
			name:         "valid ecamera upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/ecamera.hevc",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/0/ecamera.hevc",
		},
		{
			name:         "valid dcamera upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/dcamera.hevc",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/0/dcamera.hevc",
		},
		{
			name:         "valid qcamera.ts upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/qcamera.ts",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/0/qcamera.ts",
		},
		{
			name:         "valid rlog.bz2 upload URL",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/rlog.bz2",
			wantStatus:   http.StatusOK,
			wantURL:      "/upload/abc123/2024-03-15--12-30-00/0/rlog.bz2",
		},
		{
			name:         "dongle_id mismatch returns 403",
			dongleID:     "abc123",
			authDongleID: "other999",
			path:         "2024-03-15--12-30-00--0/fcamera.hevc",
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "missing path returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "",
			wantStatus:   http.StatusBadRequest,
			wantError:    "missing required query parameter: path",
		},
		{
			name:         "invalid path format returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "noslashes",
			wantStatus:   http.StatusBadRequest,
			wantError:    "failed to parse path",
		},
		{
			name:         "unsupported file type returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			path:         "2024-03-15--12-30-00--0/unknown.bin",
			wantStatus:   http.StatusBadRequest,
			wantError:    "unsupported file type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			target := "/v1.4/" + tt.dongleID + "/upload_url/"
			if tt.path != "" {
				target += "?path=" + tt.path
			}
			req := httptest.NewRequest(http.MethodGet, target, nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id")
			c.SetParamValues(tt.dongleID)
			c.Set(middleware.ContextKeyDongleID, tt.authDongleID)

			err := handler.GetUploadURL(c)
			if err != nil {
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
			}

			if tt.wantURL != "" {
				var body uploadURLResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("failed to parse response body: %v", err)
				}
				if !strings.HasSuffix(body.URL, tt.wantURL) {
					t.Errorf("url = %q, want suffix %q", body.URL, tt.wantURL)
				}
			}
		})
	}
}

func TestUploadFile(t *testing.T) {
	tests := []struct {
		name         string
		dongleID     string
		authDongleID string
		filePath     string
		body         string
		wantStatus   int
		wantError    string
		wantStored   bool
	}{
		{
			name:         "successful file upload",
			dongleID:     "abc123",
			authDongleID: "abc123",
			filePath:     "2024-03-15--12-30-00/0/fcamera.hevc",
			body:         "fake video data",
			wantStatus:   http.StatusOK,
			wantStored:   true,
		},
		{
			name:         "successful rlog upload",
			dongleID:     "abc123",
			authDongleID: "abc123",
			filePath:     "2024-03-15--12-30-00/5/rlog",
			body:         "fake log data",
			wantStatus:   http.StatusOK,
			wantStored:   true,
		},
		{
			name:         "dongle_id mismatch returns 403",
			dongleID:     "abc123",
			authDongleID: "other999",
			filePath:     "2024-03-15--12-30-00/0/fcamera.hevc",
			body:         "data",
			wantStatus:   http.StatusForbidden,
			wantError:    "dongle_id does not match authenticated device",
		},
		{
			name:         "missing file path returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			filePath:     "",
			body:         "data",
			wantStatus:   http.StatusBadRequest,
			wantError:    "missing file path",
		},
		{
			name:         "incomplete path returns 400",
			dongleID:     "abc123",
			authDongleID: "abc123",
			filePath:     "onlyone",
			body:         "data",
			wantStatus:   http.StatusBadRequest,
			wantError:    "failed to parse upload path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			store := storage.New(tmpDir)
			handler := NewUploadHandler(store, nil)

			e := echo.New()
			target := "/upload/" + tt.dongleID + "/" + tt.filePath
			req := httptest.NewRequest(http.MethodPut, target, strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.SetParamNames("dongle_id", "*")
			c.SetParamValues(tt.dongleID, tt.filePath)
			c.Set(middleware.ContextKeyDongleID, tt.authDongleID)

			err := handler.UploadFile(c)
			if err != nil {
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
			}

			if tt.wantStored {
				route, segment, filename, parseErr := parseUploadPath(tt.filePath)
				if parseErr != nil {
					t.Fatalf("failed to parse test file path: %v", parseErr)
				}
				storedPath := filepath.Join(tmpDir, tt.dongleID, route, segment, filename)
				data, readErr := os.ReadFile(storedPath)
				if readErr != nil {
					t.Fatalf("expected file at %s but got error: %v", storedPath, readErr)
				}
				if string(data) != tt.body {
					t.Errorf("stored data = %q, want %q", string(data), tt.body)
				}
			}
		})
	}
}

func TestParsePath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantRoute   string
		wantSegment string
		wantFile    string
		wantErr     bool
	}{
		{
			name:        "standard path with segment 0",
			path:        "2024-03-15--12-30-00--0/fcamera.hevc",
			wantRoute:   "2024-03-15--12-30-00",
			wantSegment: "0",
			wantFile:    "fcamera.hevc",
		},
		{
			name:        "path with multi-digit segment",
			path:        "2024-03-15--12-30-00--15/rlog",
			wantRoute:   "2024-03-15--12-30-00",
			wantSegment: "15",
			wantFile:    "rlog",
		},
		{
			name:    "no slash separator",
			path:    "noslash",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "no segment separator",
			path:    "routeonly/fcamera.hevc",
			wantErr: true,
		},
		{
			name:    "empty filename after slash",
			path:    "2024-03-15--12-30-00--0/",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, segment, filename, err := parsePath(tt.path)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got route=%q segment=%q filename=%q", route, segment, filename)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if route != tt.wantRoute {
				t.Errorf("route = %q, want %q", route, tt.wantRoute)
			}
			if segment != tt.wantSegment {
				t.Errorf("segment = %q, want %q", segment, tt.wantSegment)
			}
			if filename != tt.wantFile {
				t.Errorf("filename = %q, want %q", filename, tt.wantFile)
			}
		})
	}
}

func TestParseUploadPath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantRoute   string
		wantSegment string
		wantFile    string
		wantErr     bool
	}{
		{
			name:        "valid three-part path",
			path:        "2024-03-15--12-30-00/0/fcamera.hevc",
			wantRoute:   "2024-03-15--12-30-00",
			wantSegment: "0",
			wantFile:    "fcamera.hevc",
		},
		{
			name:    "too few parts",
			path:    "routeonly/segment",
			wantErr: true,
		},
		{
			name:    "empty path",
			path:    "",
			wantErr: true,
		},
		{
			name:    "empty component",
			path:    "route//filename",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, segment, filename, err := parseUploadPath(tt.path)

			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got route=%q segment=%q filename=%q", route, segment, filename)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if route != tt.wantRoute {
				t.Errorf("route = %q, want %q", route, tt.wantRoute)
			}
			if segment != tt.wantSegment {
				t.Errorf("segment = %q, want %q", segment, tt.wantSegment)
			}
			if filename != tt.wantFile {
				t.Errorf("filename = %q, want %q", filename, tt.wantFile)
			}
		})
	}
}

func TestBuildUploadParams(t *testing.T) {
	trueVal := pgtype.Bool{Bool: true, Valid: true}
	zeroVal := pgtype.Bool{}

	const routeID int32 = 42
	const segNum int32 = 3

	tests := []struct {
		name     string
		filename string
		wantNil  bool
		// wantFlag identifies which single field should be set to trueVal.
		// Empty string means we expect nil (wantNil == true).
		wantFlag string
	}{
		{name: "rlog sets rlog flag", filename: "rlog", wantFlag: "rlog"},
		{name: "rlog.bz2 sets rlog flag", filename: "rlog.bz2", wantFlag: "rlog"},
		{name: "qlog sets qlog flag", filename: "qlog", wantFlag: "qlog"},
		{name: "qlog.bz2 sets qlog flag", filename: "qlog.bz2", wantFlag: "qlog"},
		{name: "fcamera.hevc sets fcamera flag", filename: "fcamera.hevc", wantFlag: "fcamera"},
		{name: "fcamera.hevc~ sets fcamera flag", filename: "fcamera.hevc~", wantFlag: "fcamera"},
		{name: "ecamera.hevc sets ecamera flag", filename: "ecamera.hevc", wantFlag: "ecamera"},
		{name: "ecamera.hevc~ sets ecamera flag", filename: "ecamera.hevc~", wantFlag: "ecamera"},
		{name: "dcamera.hevc sets dcamera flag", filename: "dcamera.hevc", wantFlag: "dcamera"},
		{name: "dcamera.hevc~ sets dcamera flag", filename: "dcamera.hevc~", wantFlag: "dcamera"},
		{name: "qcamera.ts sets qcamera flag", filename: "qcamera.ts", wantFlag: "qcamera"},
		{name: "unknown filename returns nil", filename: "unknown.bin", wantNil: true},
		{name: "empty filename returns nil", filename: "", wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildUploadParams(routeID, segNum, tt.filename)

			if tt.wantNil {
				if result != nil {
					t.Fatalf("expected nil, got %+v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result, got nil")
			}

			if result.RouteID != routeID {
				t.Errorf("RouteID = %d, want %d", result.RouteID, routeID)
			}
			if result.SegmentNumber != segNum {
				t.Errorf("SegmentNumber = %d, want %d", result.SegmentNumber, segNum)
			}

			// Build a map of flag name to actual value for easy checking.
			flags := map[string]pgtype.Bool{
				"rlog":    result.RlogUploaded,
				"qlog":    result.QlogUploaded,
				"fcamera": result.FcameraUploaded,
				"ecamera": result.EcameraUploaded,
				"dcamera": result.DcameraUploaded,
				"qcamera": result.QcameraUploaded,
			}

			for flagName, flagVal := range flags {
				if flagName == tt.wantFlag {
					if flagVal != trueVal {
						t.Errorf("%s = %+v, want %+v", flagName, flagVal, trueVal)
					}
				} else {
					if flagVal != zeroVal {
						t.Errorf("%s = %+v, want zero value %+v (only %s should be set)",
							flagName, flagVal, zeroVal, tt.wantFlag)
					}
				}
			}
		})
	}
}

func TestUploadFileUnsupportedFilename(t *testing.T) {
	tmpDir := t.TempDir()
	store := storage.New(tmpDir)
	handler := NewUploadHandler(store, nil)

	e := echo.New()
	filePath := "2024-03-15--12-30-00/0/unknown.bin"
	target := "/upload/abc123/" + filePath
	req := httptest.NewRequest(http.MethodPut, target, strings.NewReader("data"))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("dongle_id", "*")
	c.SetParamValues("abc123", filePath)
	c.Set(middleware.ContextKeyDongleID, "abc123")

	err := handler.UploadFile(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse error body: %v", err)
	}
	if !strings.Contains(body.Error, "unsupported file type") {
		t.Errorf("error = %q, want substring %q", body.Error, "unsupported file type")
	}
}

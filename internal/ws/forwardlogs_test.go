package ws

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// memDeviceLogStorage is an in-memory DeviceLogStorage used by the forwardLogs
// and storeStats handler tests so each case can assert against the recorded
// payloads without touching the filesystem.
type memDeviceLogStorage struct {
	mu    sync.Mutex
	logs  []writtenPayload
	stats []writtenPayload
}

type writtenPayload struct {
	dongleID string
	id       string
	body     []byte
}

func (m *memDeviceLogStorage) WriteDeviceLog(dongleID, id string, data io.Reader) error {
	body, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.logs = append(m.logs, writtenPayload{dongleID, id, body})
	m.mu.Unlock()
	return nil
}

func (m *memDeviceLogStorage) WriteDeviceStats(dongleID, id string, data io.Reader) error {
	body, err := io.ReadAll(data)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.stats = append(m.stats, writtenPayload{dongleID, id, body})
	m.mu.Unlock()
	return nil
}

// fixedIDFunc returns an IDFunc that produces "id-1", "id-2", ... so tests
// can match writes deterministically.
func fixedIDFunc() IDFunc {
	var n int
	var mu sync.Mutex
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return "id-" + strconv.Itoa(n)
	}
}

func TestForwardLogsHandler_PlainPayload(t *testing.T) {
	store := &memDeviceLogStorage{}
	h := MakeForwardLogsHandler(store, fixedIDFunc())

	params, _ := json.Marshal(forwardLogsParams{Logs: "hello world"})
	result, rpcErr := h("device42", params)
	if rpcErr != nil {
		t.Fatalf("handler returned RPC error: %v", rpcErr)
	}
	if result == nil {
		t.Fatal("handler returned nil result")
	}

	if got := len(store.logs); got != 1 {
		t.Fatalf("logs written = %d, want 1", got)
	}
	w := store.logs[0]
	if w.dongleID != "device42" {
		t.Errorf("dongleID = %q, want device42", w.dongleID)
	}
	if w.id != "id-1" {
		t.Errorf("id = %q, want id-1", w.id)
	}
	if string(w.body) != "hello world" {
		t.Errorf("body = %q, want %q", w.body, "hello world")
	}
}

func TestForwardLogsHandler_CompressedPayload(t *testing.T) {
	store := &memDeviceLogStorage{}
	h := MakeForwardLogsHandler(store, fixedIDFunc())

	original := strings.Repeat("a sunnylink log line\n", 5000) // ~100 KB raw
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write([]byte(original)); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	params, _ := json.Marshal(forwardLogsParams{Logs: encoded, Compressed: true})
	result, rpcErr := h("device42", params)
	if rpcErr != nil {
		t.Fatalf("handler returned RPC error: %v", rpcErr)
	}
	if result == nil {
		t.Fatal("handler returned nil result")
	}

	if got := len(store.logs); got != 1 {
		t.Fatalf("logs written = %d, want 1", got)
	}
	if string(store.logs[0].body) != original {
		t.Errorf("decompressed body mismatch (len got=%d want=%d)",
			len(store.logs[0].body), len(original))
	}
}

func TestForwardLogsHandler_InvalidParams(t *testing.T) {
	store := &memDeviceLogStorage{}
	h := MakeForwardLogsHandler(store, fixedIDFunc())

	_, rpcErr := h("device42", json.RawMessage(`not-json`))
	if rpcErr == nil {
		t.Fatal("expected RPC error for malformed params")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}

	if got := len(store.logs); got != 0 {
		t.Fatalf("expected no writes on invalid params, got %d", got)
	}
}

func TestForwardLogsHandler_InvalidBase64(t *testing.T) {
	store := &memDeviceLogStorage{}
	h := MakeForwardLogsHandler(store, fixedIDFunc())

	params, _ := json.Marshal(forwardLogsParams{Logs: "not!valid!base64!", Compressed: true})
	_, rpcErr := h("device42", params)
	if rpcErr == nil {
		t.Fatal("expected RPC error for invalid base64")
	}
	if rpcErr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rpcErr.Code, CodeInvalidParams)
	}
}

func TestForwardLogsHandler_InvalidGzip(t *testing.T) {
	store := &memDeviceLogStorage{}
	h := MakeForwardLogsHandler(store, fixedIDFunc())

	params, _ := json.Marshal(forwardLogsParams{
		Logs:       base64.StdEncoding.EncodeToString([]byte("not-gzip")),
		Compressed: true,
	})
	_, rpcErr := h("device42", params)
	if rpcErr == nil {
		t.Fatal("expected RPC error for invalid gzip")
	}
}

func TestStoreStatsHandler_PlainPayload(t *testing.T) {
	store := &memDeviceLogStorage{}
	h := MakeStoreStatsHandler(store, fixedIDFunc())

	body := `{"key":"value"}`
	params, _ := json.Marshal(storeStatsParams{Stats: body})
	result, rpcErr := h("device99", params)
	if rpcErr != nil {
		t.Fatalf("handler returned RPC error: %v", rpcErr)
	}
	if result == nil {
		t.Fatal("handler returned nil result")
	}

	if got := len(store.stats); got != 1 {
		t.Fatalf("stats written = %d, want 1", got)
	}
	if string(store.stats[0].body) != body {
		t.Errorf("body = %q, want %q", store.stats[0].body, body)
	}
}

func TestDecodeMaybeCompressedPayload_RejectsGzipBomb(t *testing.T) {
	// 1 MiB of zeros gzip-compresses to ~1 KiB; verify the decompressed-size
	// guard catches a payload that exceeds the cap.
	original := make([]byte, maxDecompressedPayloadBytes+1024)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(original); err != nil {
		t.Fatalf("gzip write failed: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close failed: %v", err)
	}
	encoded := base64.StdEncoding.EncodeToString(buf.Bytes())

	_, rpcErr := decodeMaybeCompressedPayload(encoded, true)
	if rpcErr == nil {
		t.Fatal("expected RPC error for oversized payload")
	}
}

func TestDefaultIDFunc_ReturnsUnique(t *testing.T) {
	idFn := DefaultIDFunc()
	seen := make(map[string]struct{})
	for i := 0; i < 1000; i++ {
		id := idFn()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate id at iteration %d: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

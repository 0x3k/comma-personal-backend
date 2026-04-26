package ws

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"time"
)

// DeviceLogStorage is the subset of *storage.Storage that the forwardLogs and
// storeStats handlers depend on. Kept as an interface so tests can substitute
// an in-memory implementation.
type DeviceLogStorage interface {
	WriteDeviceLog(dongleID, id string, data io.Reader) error
	WriteDeviceStats(dongleID, id string, data io.Reader) error
}

// maxDecompressedPayloadBytes caps how large a decompressed forwardLogs or
// storeStats payload may be after gzip-decode. Hardware caps the on-the-wire
// frame at 1 MiB (see maxMessageSize), so the only reason a payload would
// exceed this on disk is a maliciously crafted gzip bomb.
const maxDecompressedPayloadBytes = 64 * 1024 * 1024

// forwardLogsParams matches the params of athenad's forwardLogs notification.
// `compressed` is sunnylink-only; upstream athenad sends raw text and omits the
// flag. We accept either flavor.
type forwardLogsParams struct {
	Logs       string `json:"logs"`
	Compressed bool   `json:"compressed,omitempty"`
}

// storeStatsParams matches the params of athenad's storeStats notification.
type storeStatsParams struct {
	Stats      string `json:"stats"`
	Compressed bool   `json:"compressed,omitempty"`
}

// IDFunc returns a unique filename component for each persisted payload.
// In production this is a wall-clock timestamp; tests inject a deterministic
// counter so assertions can name the resulting file.
type IDFunc func() string

// MakeForwardLogsHandler returns a JSON-RPC method handler that ingests the
// log payloads pushed unprompted by athenad/sunnylinkd. Sunnylink gzip+base64
// encodes any payload larger than 32 KiB and signals this with `compressed`;
// the handler decodes both flavors transparently and persists the raw body via
// the storage layer.
func MakeForwardLogsHandler(store DeviceLogStorage, idFn IDFunc) MethodHandler {
	if idFn == nil {
		idFn = DefaultIDFunc()
	}
	return func(dongleID string, params json.RawMessage) (interface{}, *RPCError) {
		var p forwardLogsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
		}
		body, rpcErr := decodeMaybeCompressedPayload(p.Logs, p.Compressed)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if err := store.WriteDeviceLog(dongleID, idFn(), bytes.NewReader(body)); err != nil {
			return nil, NewRPCError(CodeInternalError, fmt.Sprintf("failed to persist log: %v", err))
		}
		return map[string]int{"success": 1}, nil
	}
}

// MakeStoreStatsHandler is the storeStats sibling of MakeForwardLogsHandler.
func MakeStoreStatsHandler(store DeviceLogStorage, idFn IDFunc) MethodHandler {
	if idFn == nil {
		idFn = DefaultIDFunc()
	}
	return func(dongleID string, params json.RawMessage) (interface{}, *RPCError) {
		var p storeStatsParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid params: %v", err))
		}
		body, rpcErr := decodeMaybeCompressedPayload(p.Stats, p.Compressed)
		if rpcErr != nil {
			return nil, rpcErr
		}
		if err := store.WriteDeviceStats(dongleID, idFn(), bytes.NewReader(body)); err != nil {
			return nil, NewRPCError(CodeInternalError, fmt.Sprintf("failed to persist stats: %v", err))
		}
		return map[string]int{"success": 1}, nil
	}
}

// decodeMaybeCompressedPayload returns the raw payload bytes. When compressed
// is true, the input is treated as base64(gzip(body)). The decompressed output
// is bounded by maxDecompressedPayloadBytes to defend against gzip bombs.
func decodeMaybeCompressedPayload(payload string, compressed bool) ([]byte, *RPCError) {
	if !compressed {
		return []byte(payload), nil
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid base64: %v", err))
	}
	gr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("invalid gzip: %v", err))
	}
	defer gr.Close()
	out, err := io.ReadAll(io.LimitReader(gr, maxDecompressedPayloadBytes+1))
	if err != nil {
		return nil, NewRPCError(CodeInvalidParams, fmt.Sprintf("failed to decompress: %v", err))
	}
	if len(out) > maxDecompressedPayloadBytes {
		return nil, NewRPCError(CodeInvalidParams, "decompressed payload exceeds size limit")
	}
	return out, nil
}

// DefaultIDFunc returns an IDFunc that produces a unique, monotonically
// increasing filename component. The format is `<unix_nano>-<seq>` where seq
// is per-process and starts at zero -- this guards against two payloads
// arriving in the same nanosecond from different goroutines.
func DefaultIDFunc() IDFunc {
	var seq atomic.Uint64
	return func() string {
		n := seq.Add(1)
		return strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + strconv.FormatUint(n, 10)
	}
}

package ws

import (
	"encoding/json"
	"testing"
)

func TestIsSunnylinkBlockedParam(t *testing.T) {
	cases := []struct {
		key     string
		blocked bool
	}{
		{"GithubSshKeys", true},
		{"GithubUsername", true},
		{"HasAcceptedTerms", true},
		{"HasAcceptedTermsSP", true},
		{"CompletedSunnylinkConsentVersion", true},
		{"CompletedTrainingVersion", true},
		{"OpenpilotEnabledToggle", false},
		{"DongleId", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			if got := IsSunnylinkBlockedParam(tc.key); got != tc.blocked {
				t.Errorf("IsSunnylinkBlockedParam(%q) = %v, want %v", tc.key, got, tc.blocked)
			}
		})
	}
}

func TestCallToggleLogUpload_Roundtrip(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, map[string]interface{}{}, nil)

	if err := CallToggleLogUpload(caller, client, true); err != nil {
		t.Fatalf("CallToggleLogUpload failed: %v", err)
	}
}

func TestCallGetParamsAllKeys_DecodesList(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, []string{"DongleId", "GithubSshKeys"}, nil)

	keys, err := CallGetParamsAllKeys(caller, client)
	if err != nil {
		t.Fatalf("CallGetParamsAllKeys failed: %v", err)
	}
	if len(keys) != 2 || keys[0] != "DongleId" || keys[1] != "GithubSshKeys" {
		t.Errorf("keys = %v", keys)
	}
}

func TestCallGetParamsAllKeysV1_UnwrapsInnerJSON(t *testing.T) {
	inner := []map[string]interface{}{
		{"key": "DongleId", "type": float64(1), "default_value": nil},
		{"key": "GithubSshKeys", "type": float64(0), "default_value": "abc"},
	}
	innerJSON, _ := json.Marshal(inner)

	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, map[string]interface{}{
		"keys": string(innerJSON),
	}, nil)

	entries, err := CallGetParamsAllKeysV1(caller, client)
	if err != nil {
		t.Fatalf("CallGetParamsAllKeysV1 failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0]["key"] != "DongleId" {
		t.Errorf("entry 0 key = %v, want DongleId", entries[0]["key"])
	}
}

func TestCallGetParamsMetadata_PassesThroughString(t *testing.T) {
	caller := NewRPCCaller()
	const payload = "Zm9vYmFy" // base64('foobar') -- a placeholder gzipped+b64 string
	client := testClientWithResponder(t, caller, payload, nil)

	got, err := CallGetParamsMetadata(caller, client)
	if err != nil {
		t.Fatalf("CallGetParamsMetadata failed: %v", err)
	}
	if got != payload {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestCallGetParams_PassesObjectThrough(t *testing.T) {
	caller := NewRPCCaller()
	client := testClientWithResponder(t, caller, map[string]interface{}{
		"DongleId": "YWJjMTIz",
		"params":   `[{"key":"DongleId","value":"YWJjMTIz","type":1,"is_compressed":false}]`,
	}, nil)

	got, err := CallGetParams(caller, client, []string{"DongleId"}, false)
	if err != nil {
		t.Fatalf("CallGetParams failed: %v", err)
	}
	if got["DongleId"] != "YWJjMTIz" {
		t.Errorf("DongleId = %v", got["DongleId"])
	}
}

func TestCallSaveParams_StripsBlockedKeys(t *testing.T) {
	// The responder records the params it received so the test can assert
	// the blocked key was removed before the request was sent.
	var received map[string]interface{}
	caller := NewRPCCaller()
	client := newRecordingResponder(t, caller, &received)

	rejected, err := CallSaveParams(caller, client, map[string]string{
		"OpenpilotEnabledToggle": "MQ==",
		"GithubSshKeys":          "ZXZpbA==",
	}, false)
	if err != nil {
		t.Fatalf("CallSaveParams failed: %v", err)
	}
	if len(rejected) != 1 || rejected[0] != "GithubSshKeys" {
		t.Errorf("rejected = %v, want [GithubSshKeys]", rejected)
	}

	updates, ok := received["params_to_update"].(map[string]interface{})
	if !ok {
		t.Fatalf("params_to_update not present in sent params: %#v", received)
	}
	if _, present := updates["GithubSshKeys"]; present {
		t.Errorf("GithubSshKeys was forwarded to the device despite being blocked")
	}
	if updates["OpenpilotEnabledToggle"] != "MQ==" {
		t.Errorf("OpenpilotEnabledToggle was not forwarded: %v", updates["OpenpilotEnabledToggle"])
	}
}

// newRecordingResponder is testClientWithResponder with the request
// inspection hook needed by TestCallSaveParams_StripsBlockedKeys. It runs a
// tiny goroutine that decodes outgoing RPC requests, captures their params,
// and replies with a generic success.
func newRecordingResponder(t *testing.T, caller *RPCCaller, captured *map[string]interface{}) *Client {
	t.Helper()
	hub := NewHub()
	c := &Client{
		DongleID: "test-device",
		hub:      hub,
		sendCh:   make(chan []byte, sendChSize),
		done:     make(chan struct{}),
		handlers: make(map[string]MethodHandler),
	}

	go func() {
		for msg := range c.sendCh {
			var req RPCRequest
			if err := json.Unmarshal(msg, &req); err != nil {
				continue
			}
			if len(req.Params) > 0 {
				_ = json.Unmarshal(req.Params, captured)
			}
			caller.HandleResponse(&RPCResponse{
				JSONRPC: jsonRPCVersion,
				Result:  map[string]interface{}{"ok": true},
				ID:      req.ID,
			})
		}
	}()

	return c
}

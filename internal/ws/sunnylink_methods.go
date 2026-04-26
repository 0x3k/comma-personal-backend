package ws

import (
	"encoding/json"
	"fmt"
)

// SunnylinkBlockedParams mirrors BLOCKED_PARAMS in
// sunnypilot/sunnylink/athena/sunnylinkd.py. Keep this list in sync when
// rebasing -- the device-side enforcement is the source of truth, but the
// backend rejects writes to these keys as defense-in-depth so a compromised
// operator UI cannot grant itself SSH access via the params channel.
var SunnylinkBlockedParams = map[string]struct{}{
	"CompletedSunnylinkConsentVersion": {},
	"CompletedTrainingVersion":         {},
	"GithubUsername":                   {}, // could grant SSH access
	"GithubSshKeys":                    {}, // direct SSH key injection
	"HasAcceptedTerms":                 {},
	"HasAcceptedTermsSP":               {},
}

// IsSunnylinkBlockedParam reports whether the given param name is on the
// sunnylink-side blocklist. Callers should reject writes to blocked keys
// before invoking saveParams on the device.
func IsSunnylinkBlockedParam(key string) bool {
	_, blocked := SunnylinkBlockedParams[key]
	return blocked
}

// ToggleLogUploadParams matches sunnylinkd's toggleLogUpload(enabled: bool).
type ToggleLogUploadParams struct {
	Enabled bool `json:"enabled"`
}

// CallToggleLogUpload tells the device to suspend or resume log uploads.
// When false, sunnylinkd sets DISALLOW_LOG_UPLOAD and stops forwardLogs;
// when true, log forwarding resumes.
func CallToggleLogUpload(caller *RPCCaller, client *Client, enabled bool) error {
	resp, err := caller.Call(client, "toggleLogUpload", ToggleLogUploadParams{Enabled: enabled})
	if err != nil {
		return fmt.Errorf("toggleLogUpload failed: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("toggleLogUpload returned error: %w", resp.Error)
	}
	return nil
}

// CallGetParamsAllKeys returns every param key the device is aware of.
func CallGetParamsAllKeys(caller *RPCCaller, client *Client) ([]string, error) {
	resp, err := caller.Call(client, "getParamsAllKeys", nil)
	if err != nil {
		return nil, fmt.Errorf("getParamsAllKeys failed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("getParamsAllKeys returned error: %w", resp.Error)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal getParamsAllKeys result: %w", err)
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("failed to decode getParamsAllKeys result: %w", err)
	}
	return keys, nil
}

// CallGetParamsAllKeysV1 returns every param key with metadata. The device
// returns {"keys": "<json string>"} where the inner JSON encodes a list of
// param entries; we surface that inner list directly so callers don't have
// to double-decode.
func CallGetParamsAllKeysV1(caller *RPCCaller, client *Client) ([]map[string]interface{}, error) {
	resp, err := caller.Call(client, "getParamsAllKeysV1", nil)
	if err != nil {
		return nil, fmt.Errorf("getParamsAllKeysV1 failed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("getParamsAllKeysV1 returned error: %w", resp.Error)
	}

	wrapper, ok := resp.Result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("getParamsAllKeysV1: expected object result, got %T", resp.Result)
	}
	innerStr, ok := wrapper["keys"].(string)
	if !ok {
		return nil, fmt.Errorf("getParamsAllKeysV1: missing or non-string `keys` field")
	}
	var entries []map[string]interface{}
	if err := json.Unmarshal([]byte(innerStr), &entries); err != nil {
		return nil, fmt.Errorf("getParamsAllKeysV1: failed to decode inner keys JSON: %w", err)
	}
	return entries, nil
}

// CallGetParamsMetadata returns the gzip+base64-compressed equivalent of
// getParamsAllKeysV1. The device sends this when bandwidth is precious;
// callers can pass the raw string straight through to a compatible client
// or decompress it themselves.
func CallGetParamsMetadata(caller *RPCCaller, client *Client) (string, error) {
	resp, err := caller.Call(client, "getParamsMetadata", nil)
	if err != nil {
		return "", fmt.Errorf("getParamsMetadata failed: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("getParamsMetadata returned error: %w", resp.Error)
	}
	s, ok := resp.Result.(string)
	if !ok {
		return "", fmt.Errorf("getParamsMetadata: expected string result, got %T", resp.Result)
	}
	return s, nil
}

// GetParamsParams matches sunnylinkd's getParams(params_keys, compression).
type GetParamsParams struct {
	ParamsKeys  []string `json:"params_keys"`
	Compression bool     `json:"compression,omitempty"`
}

// CallGetParams asks the device for the values of the named params. The
// device returns a dict with one entry per key (base64-encoded values, gzip
// when compression=true) plus a "params" field holding a JSON-encoded
// detailed list. We pass the dict back as-is so callers can pick the form
// they prefer.
func CallGetParams(caller *RPCCaller, client *Client, keys []string, compression bool) (map[string]interface{}, error) {
	resp, err := caller.Call(client, "getParams", GetParamsParams{ParamsKeys: keys, Compression: compression})
	if err != nil {
		return nil, fmt.Errorf("getParams failed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("getParams returned error: %w", resp.Error)
	}
	if resp.Result == nil {
		return map[string]interface{}{}, nil
	}
	out, ok := resp.Result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("getParams: expected object result, got %T", resp.Result)
	}
	return out, nil
}

// SaveParamsParams matches sunnylinkd's saveParams(params_to_update, compression).
type SaveParamsParams struct {
	ParamsToUpdate map[string]string `json:"params_to_update"`
	Compression    bool              `json:"compression,omitempty"`
}

// CallSaveParams tells the device to write the given param updates. Blocked
// keys are stripped before the request leaves the server, so the operator
// UI cannot use this method to bypass the device-side allowlist. The
// returned slice lists the keys that were rejected.
func CallSaveParams(caller *RPCCaller, client *Client, updates map[string]string, compression bool) (rejected []string, err error) {
	if updates == nil {
		updates = map[string]string{}
	}
	filtered := make(map[string]string, len(updates))
	for k, v := range updates {
		if IsSunnylinkBlockedParam(k) {
			rejected = append(rejected, k)
			continue
		}
		filtered[k] = v
	}

	resp, err := caller.Call(client, "saveParams", SaveParamsParams{
		ParamsToUpdate: filtered,
		Compression:    compression,
	})
	if err != nil {
		return rejected, fmt.Errorf("saveParams failed: %w", err)
	}
	if resp.Error != nil {
		return rejected, fmt.Errorf("saveParams returned error: %w", resp.Error)
	}
	return rejected, nil
}
